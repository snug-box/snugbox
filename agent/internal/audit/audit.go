// Package audit runs the read-only audit of blueprint section 7.2: it samples
// a box for a short window, reads what the kernel remembers from before, and
// puts the memory model, the risks, the fixes and the plan advice together.
// It changes nothing and sends nothing; the only network request it can make
// is to the cloud metadata service on the box's own link-local address.
package audit

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"slices"
	"strings"
	"time"

	"snugbox.dev/snugbox/agent/internal/collect"
	"snugbox.dev/snugbox/agent/internal/cron"
	"snugbox.dev/snugbox/agent/internal/fix"
	"snugbox.dev/snugbox/agent/internal/model"
	"snugbox.dev/snugbox/agent/internal/plan"
	"snugbox.dev/snugbox/agent/internal/provider"
)

// Sampling limits. An audit longer than MaxDuration belongs to snugbox watch.
const (
	DefaultDuration = 60 * time.Second
	MinDuration     = 10 * time.Second
	MaxDuration     = time.Hour
	MinInterval     = 2 * time.Second
	maxSamples      = 1800
	rediscoverEvery = 60 * time.Second
)

// Options set how long to sample and what to skip.
type Options struct {
	Duration time.Duration
	// Offline skips the metadata service, so the audit makes no network
	// request at all.
	Offline bool
	// Progress, when set, is called after each sample with the time left.
	Progress func(left time.Duration)
}

// Interval is the sampling interval for an audit of the given length: two
// seconds, stretched so a long audit keeps at most 1,800 samples.
func Interval(d time.Duration) time.Duration {
	return max(MinInterval, d/maxSamples)
}

// Prober finds the box's provider.
type Prober interface {
	Detect(ctx context.Context) provider.Info
}

// Deps are the audit's connections to the box. Live builds them for a real
// server; tests replace them.
type Deps struct {
	Collector *collect.Collector
	Host      collect.Host
	Prober    Prober
	KernelLog func(boot time.Time) (collect.KernelLog, error)
	Now       func() time.Time
	Sleep     func(ctx context.Context, d time.Duration) error
	// FS is the root filesystem, for checks such as whether systemd-oomd is
	// installed.
	FS fs.FS
	// Root is true when the audit runs with root privileges.
	Root bool
}

// Live returns the dependencies of an audit of this machine.
func Live(containers collect.ContainerSource) (Deps, error) {
	root := os.DirFS("/")
	c, err := collect.New(collect.Options{FS: root, Containers: containers})
	if err != nil {
		return Deps{}, err
	}
	return Deps{
		Collector: c,
		Host:      collect.LiveHost{},
		Prober:    provider.NewProber(root),
		KernelLog: collect.ReadKernelLog,
		Now:       time.Now,
		Sleep:     sleep,
		FS:        root,
		Root:      os.Geteuid() == 0,
	}, nil
}

func sleep(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

// Result is everything one audit found. It travels as JSON from the watcher
// to the command line, which reattaches the catalogue on arrival.
type Result struct {
	Started, Ended time.Time
	Root           bool
	Offline        bool
	// Watched is true when the result comes from the watcher's history
	// rather than a one-off sample.
	Watched   bool
	Machine   collect.Machine
	Provider  provider.Info
	Workloads map[string]model.Workload
	Summary   model.Summary
	Estimate  model.Estimate
	History   model.History
	KernelLog collect.KernelLog
	Gaps      []collect.Gap
	// CrashLoops are the containers Docker keeps restarting, which may be
	// missing from Workloads while they wait to start again.
	CrashLoops []collect.CrashLoop
	Advice     Advice
	Risks      []Risk
	Fixes      []fix.Proposal
	// Idle are the containers that used almost no CPU over at least
	// IdleAfter of watching, largest first.
	Idle []Idle
	// Runs are the services' runs over the last week, which only the
	// watcher records, for the schedule fix. They stay on the box that
	// proposed the fix.
	Runs []model.Run `json:"-"`
	// Cron are the box's cron jobs and Minutes its highest working set in
	// each minute of the last week, which the watcher reads for the
	// schedule fix's advice on cron jobs. They stay on the box too.
	Cron    []cron.Job          `json:"-"`
	Minutes []model.MinuteTotal `json:"-"`

	logErr error
}

// Run samples the box for the chosen duration and assembles the result.
func Run(ctx context.Context, d Deps, opts Options) (*Result, error) {
	if opts.Duration == 0 {
		opts.Duration = DefaultDuration
	}
	if opts.Duration < MinDuration || opts.Duration > MaxDuration {
		return nil, fmt.Errorf("an audit samples for %v to %v; for longer, use snugbox watch", MinDuration, MaxDuration)
	}
	c := d.Collector
	if c.CgroupRoot() == "" {
		return nil, ErrNoCgroupV2
	}
	r := &Result{Started: d.Now(), Root: d.Root, Offline: opts.Offline}
	if err := c.Discover(ctx); err != nil {
		return nil, err
	}
	r.Machine = c.Machine(ctx, d.Host)
	if !opts.Offline && d.Prober != nil {
		r.Provider = d.Prober.Detect(ctx)
	}

	samples, points, err := sampleWindow(ctx, d, opts)
	if err != nil {
		return nil, err
	}
	r.Ended = d.Now()
	r.Workloads = c.Workloads()
	r.CrashLoops = c.CrashLoops(r.Ended)
	r.Summary = model.Summarize(samples, points, max(r.Machine.CPUs, 1), c.PageSize())

	r.History = r.history(samples[len(samples)-1].Box, d)
	r.Gaps = c.Gaps()
	if !r.History.LogRead {
		why := "needs root, so kill times are unknown"
		if r.logErr != nil && !errors.Is(r.logErr, fs.ErrPermission) {
			why = r.logErr.Error() + ", so kill times are unknown"
		}
		r.Gaps = append(r.Gaps, collect.Gap{What: "the kernel log at /dev/kmsg", Why: why})
	}
	Assemble(r, d.FS)
	return r, nil
}

// Assemble works out the estimate, the plan advice, the risks and the fixes
// of a result whose machine, provider, workloads, summary and history are
// set. The one-off audit and the watcher both finish through it. root is the
// box's root filesystem, for checks such as whether systemd-oomd is
// installed; it may be nil.
func Assemble(r *Result, root fs.FS) {
	r.Estimate = model.Size(r.Summary, r.History)
	r.Advice = advise(r)
	r.Risks = findRisks(r)
	r.Fixes = fix.Propose(r.fixInput(root))
	r.Idle = findIdle(r)
}

// ErrNoCgroupV2 means the box mounts cgroup v1 only. Snugbox measures
// workloads through cgroup v2.
var ErrNoCgroupV2 = errors.New("this box has no cgroup v2; Snugbox measures workloads through cgroup v2 (Linux 5.4 or newer, systemd 247 or newer)")

func sampleWindow(ctx context.Context, d Deps, opts Options) ([]model.Sample, []model.Point, error) {
	c := d.Collector
	deriver := model.NewDeriver(c.PageSize())
	interval := Interval(opts.Duration)
	start := d.Now()
	deadline := start.Add(opts.Duration)
	lastDiscover := start
	var samples []model.Sample
	var points []model.Point
	for {
		now := d.Now()
		if now.Sub(lastDiscover) >= rediscoverEvery {
			if err := c.Discover(ctx); err != nil {
				return nil, nil, err
			}
			lastDiscover = now
		}
		s := c.Sample(now)
		samples = append(samples, s)
		points = append(points, deriver.Add(s))
		left := deadline.Sub(d.Now())
		if left <= 0 {
			return samples, points, nil
		}
		if opts.Progress != nil {
			opts.Progress(left)
		}
		if err := d.Sleep(ctx, min(interval, left)); err != nil {
			return nil, nil, err
		}
	}
}

func (r *Result) history(last model.BoxUsage, d Deps) model.History {
	h := model.History{
		Now:          r.Ended,
		Uptime:       r.Machine.Uptime + r.Ended.Sub(r.Started),
		PressureFull: last.PressureFull,
		HasPressure:  last.HasPressure,
		OOMKills:     last.OOMKills,
	}
	if d.KernelLog == nil || r.Machine.BootTime.IsZero() {
		return h
	}
	log, err := d.KernelLog(r.Machine.BootTime)
	if err != nil {
		r.logErr = err
		return h
	}
	r.KernelLog = log
	h.LogRead, h.LogSince, h.Kills = true, log.Since, log.KillTimes()
	return h
}

func (r *Result) fixInput(root fs.FS) fix.Input {
	in := fix.Input{Machine: r.Machine, Workloads: r.Workloads, Summary: r.Summary, Window: r.Summary.Window(), Runs: r.Runs, Now: r.Ended,
		Cron: r.Cron, Minutes: r.Minutes, Zone: time.Local}
	if root != nil {
		in.SystemdOOMD = anyExists(root, "usr/lib/systemd/systemd-oomd", "lib/systemd/systemd-oomd")
		in.Earlyoom = anyExists(root, "usr/bin/earlyoom", "bin/earlyoom")
		if len(r.Runs) > 0 {
			var services []string
			for _, run := range r.Runs {
				services = append(services, run.Name)
			}
			slices.Sort(services)
			in.Timers = collect.Timers(root, slices.Compact(services))
		}
	}
	// The swap file goes in the helper's own folder.
	if d, ok := diskFor(r.Machine.Disks, "/var/lib/snugbox-helper"); ok {
		in.FreeDisk, in.SwapFS = d.Size-d.Used, d.FSType
	}
	return in
}

func anyExists(root fs.FS, paths ...string) bool {
	for _, p := range paths {
		if _, err := fs.Stat(root, p); err == nil {
			return true
		}
	}
	return false
}

// diskFor returns the filesystem that holds path: the mount with the longest
// mount point that is a prefix of it.
func diskFor(disks []collect.Disk, path string) (collect.Disk, bool) {
	var best collect.Disk
	found := false
	for _, d := range disks {
		mp := strings.TrimSuffix(d.MountPoint, "/") + "/"
		if (d.MountPoint == "/" || strings.HasPrefix(path+"/", mp)) && len(d.MountPoint) >= len(best.MountPoint) {
			best, found = d, true
		}
	}
	return best, found
}

// Advice is the plan advisor's answer for the box.
type Advice struct {
	Catalogue *plan.Catalogue `json:"-"`
	// Box is what the advisor knew about the box when it chose.
	Box plan.Box
	// Current is the plan the box is on, nil when Snugbox can't tell.
	Current *plan.Plan
	// PaysFor is the memory the box pays for: the current plan's advertised
	// size when the plan is known (PaysForExact), otherwise the memory the
	// kernel sees, which is a little less than the plan's.
	PaysFor      uint64
	PaysForExact bool
	Waste        uint64
	SizeClass    uint64
	HasSizeClass bool
	Options      []plan.Option
	Unavailable  []plan.Option
	// Withheld is true when the box shows signs it needs more memory, so no
	// smaller plan is offered.
	Withheld bool
}

// OwnClass is the common size the box is sold as: its plan's advertised
// memory when the plan is known, otherwise the smallest common size that
// memTotal, the memory its kernel sees, fits in. It is 0 above the largest.
func (a Advice) OwnClass(memTotal uint64) uint64 {
	if a.PaysForExact {
		return a.PaysFor
	}
	return plan.ClassOf(memTotal)
}

// Fits reports whether the box's own memory, as its kernel sees it, holds
// what the box needs.
func (r *Result) Fits() bool { return r.Estimate.Snug <= r.Machine.MemTotal }

// Reattach restores the parts of a result that don't travel as JSON: the
// price catalogue built into this release.
func (r *Result) Reattach() error {
	cat, err := plan.Embedded()
	if err != nil {
		return err
	}
	r.Advice.Catalogue = cat
	return nil
}

func advise(r *Result) Advice {
	a := Advice{}
	cat, err := plan.Embedded()
	if err != nil {
		return a
	}
	a.Catalogue = cat
	box := plan.Box{
		ProviderID:     r.Provider.Provider,
		MemTotal:       r.Machine.MemTotal,
		CPUs:           r.Machine.CPUs,
		DiskUsed:       r.Machine.DiskUsed(),
		CPUQ95:         r.Summary.CPUQ95,
		EgressMonthly:  r.Machine.EgressPerMonth,
		IngressMonthly: r.Machine.IngressPerMonth,
	}
	if root, ok := r.Machine.RootDisk(); ok {
		box.DiskDevice = root.DeviceSize
	}
	a.Box = box
	if r.Provider.Provider != provider.AWS {
		if cur, ok := cat.Current(box); ok {
			a.Current = &cur
			a.PaysFor, a.PaysForExact = cur.RAM, true
		}
	}
	if !a.PaysForExact {
		a.PaysFor = r.Machine.MemTotal
	}
	snug := r.Estimate.Snug
	if a.PaysFor > snug {
		a.Waste = a.PaysFor - snug
	}
	a.SizeClass, a.HasSizeClass = plan.SizeClass(snug)
	// UsableShare stands in for plans nobody has measured, but this box's
	// own plan is measured, by MemTotal: its own size fits exactly when
	// MemTotal holds snug. Without this a box with 7.7 GiB that needs 7.53
	// GiB was told to move to 16 GiB, as 93% of 8 GiB is 7.44 GiB, and a box
	// on a known plan that is short of memory could be told to move to the
	// size it is already on.
	if own := a.OwnClass(r.Machine.MemTotal); own > 0 {
		switch fits := r.Fits(); {
		case fits && (!a.HasSizeClass || a.SizeClass > own):
			a.SizeClass, a.HasSizeClass = own, true
		case !fits && a.PaysForExact && a.HasSizeClass && a.SizeClass <= own:
			a.SizeClass, a.HasSizeClass = plan.NextClass(own)
		}
	}
	a.Withheld = r.Estimate.Upward.Any()
	if !a.Withheld {
		a.Options, a.Unavailable = cat.Advise(snug, box, a.Current)
	}
	return a
}
