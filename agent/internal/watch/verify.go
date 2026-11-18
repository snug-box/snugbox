package watch

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"math"
	"regexp"
	"slices"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"snugbox.dev/snugbox/agent/internal/helperapi"
	"snugbox.dev/snugbox/agent/internal/localapi"
	"snugbox.dev/snugbox/agent/internal/model"
	"snugbox.dev/snugbox/agent/internal/store"
	helperv1 "snugbox.dev/snugbox/gen/snugbox/helper/v1"
)

// Verification settings of blueprint section 3.7 rule 3 and section 5.6. The
// pressure bars are starting values for the outside memory review.
const (
	// ChangesEvery is how often the watcher reads the helper's transactions
	// when nothing it did has changed them.
	ChangesEvery = 5 * time.Minute
	// PressureWindow is the stretch over which a workload's stall share is
	// compared with its 7-day 99th percentile.
	PressureWindow = time.Minute
	// PressureFloor keeps tiny stalls from failing a fix: the share must
	// pass both the workload's own 99th percentile and this, in percent.
	PressureFloor = 1.0
	// PressureNoHistory is the bar when the store holds no pressure history
	// for the workload.
	PressureNoHistory = 5.0
	// CountedAfter is how long a fix must run under watch without trouble
	// before it counts as done in reports: a full day with its nightly jobs.
	CountedAfter = 24 * time.Hour
	// MissingFor is how long a changed workload may be gone before
	// verification counts it as stopped.
	MissingFor    = time.Minute
	actEvery      = time.Minute
	dockerEvery   = time.Minute
	historyWindow = 7 * 24 * time.Hour
)

var containerCgroup = regexp.MustCompile(`(?:^|/)(?:docker-([0-9a-f]{64})\.scope|docker/([0-9a-f]{64}))$`)

// target is one workload a fix changed, and how it looked when the watcher
// began judging the fix.
type target struct {
	id        string
	container string
	seen      bool
	kills     uint64
	highs     uint64
	start     uint64
	hasStart  bool
	missing   time.Time
	p99       float64
	hasP99    bool
	stalls    []stall
	restarts  int32
	oomKilled bool
	hasDocker bool
}

type stall struct {
	at    time.Time
	total time.Duration
}

type watched struct {
	tx      *helperv1.Transaction
	state   helperv1.State
	from    time.Time
	targets []*target
	trouble string
	nextAct time.Time
}

// verifier watches every fix the helper reports as verifying or watching.
// It runs in the sampling goroutine; the mutex covers what the API reads.
type verifier struct {
	d *Daemon
	h Helper

	mu          sync.Mutex
	list        []*helperv1.Transaction
	read        time.Time
	checked     time.Time
	journalSize int64
	journalTime time.Time
	err         error
	open        map[string]*watched
	dirty       bool
	dockers     map[string]*helperv1.Container
	dockAt      time.Time
}

func newVerifier(d *Daemon, h Helper) *verifier {
	return &verifier{d: d, h: h, open: map[string]*watched{}, dirty: true}
}

// poke makes the next sample read the helper's transactions again, so a fix
// snugbox fit just applied is judged from its first seconds. It only changes
// when the watcher looks, so the local API stays read-only towards the box.
func (v *verifier) poke() {
	v.mu.Lock()
	v.dirty = true
	v.mu.Unlock()
}

func (v *verifier) refresh(ctx context.Context, now time.Time) {
	v.mu.Lock()
	force := v.dirty || len(v.open) > 0 || v.err != nil || now.Sub(v.read) >= helperResync
	v.dirty = false
	v.checked = now
	v.mu.Unlock()
	// The root-owned journal is append-only. Check its metadata before
	// asking, so a write during the request is caught by the next check.
	var journal fs.FileInfo
	if v.d.deps.FS != nil {
		journal, _ = fs.Stat(v.d.deps.FS, strings.TrimPrefix(helperapi.JournalPath, "/"))
	}
	if !force && journal != nil && journal.Size() == v.journalSize && journal.ModTime().Equal(v.journalTime) {
		return
	}
	v.journalSize = -1
	if journal != nil {
		v.journalSize, v.journalTime = journal.Size(), journal.ModTime()
	}
	list, err := v.h.List(ctx)
	v.mu.Lock()
	defer v.mu.Unlock()
	v.read, v.err = now, err
	if err != nil {
		v.d.deps.Log.Warn("could not read the helper's transactions", "err", err)
		return
	}
	v.list = list
	open := make(map[string]*watched)
	for _, tx := range list {
		s := tx.GetState()
		if s != helperv1.State_STATE_VERIFYING && s != helperv1.State_STATE_WATCHING {
			continue
		}
		w := v.open[tx.GetId()]
		if w == nil || w.state != s {
			w = v.begin(ctx, tx)
		}
		w.tx = tx
		open[tx.GetId()] = w
	}
	v.open = open
}

// begin starts judging a fix in its current state. After a reboot the
// window starts again at boot, since every workload restarted with the box.
func (v *verifier) begin(ctx context.Context, tx *helperv1.Transaction) *watched {
	v.d.mu.RLock()
	boot := v.d.machine.BootTime
	v.d.mu.RUnlock()
	w := &watched{tx: tx, state: tx.GetState(), from: tx.GetStateSince().AsTime()}
	if boot.After(w.from) {
		w.from = boot
	}
	var ids []string
	for _, cg := range tx.GetCgroups() {
		t := &target{id: "/" + strings.TrimPrefix(cg, "/")}
		if m := containerCgroup.FindStringSubmatch(cg); m != nil {
			t.container = m[1] + m[2]
		}
		w.targets = append(w.targets, t)
		ids = append(ids, t.id)
	}
	if w.state == helperv1.State_STATE_VERIFYING {
		q, err := v.d.st.PressureQ99(ctx, ids, w.from.Add(-historyWindow), w.from)
		if err != nil {
			v.d.deps.Log.Warn("could not read pressure history for a fix's verification", "tx", tx.GetId(), "err", err)
		}
		for _, t := range w.targets {
			t.p99, t.hasP99 = q[t.id]
		}
	}
	w.trouble = v.killedSince(ctx, w)
	return w
}

// killedSince looks in the store for out-of-memory kills in a fix's
// workloads after it was applied, which the watcher may have recorded
// before it started judging the fix: after its own restart, or in the
// minutes before it next read the helper's list.
func (v *verifier) killedSince(ctx context.Context, w *watched) string {
	evs, err := v.d.st.Events(ctx, w.from, 10000)
	if err != nil {
		v.d.deps.Log.Warn("could not read the events since a fix was applied", "tx", w.tx.GetId(), "err", err)
		return ""
	}
	ws := v.d.deps.Collector.Workloads()
	for _, t := range w.targets {
		names := []string{ws[t.id].Label(), strings.TrimPrefix(t.id, "/")}
		if len(t.container) >= 12 {
			names = append(names, t.container[:12])
		}
		for _, e := range evs {
			if e.Kind == "oom_kill" && e.Time.After(w.from) && slices.Contains(names, e.Workload) {
				return fmt.Sprintf("the kernel killed a process in %s for lack of memory", e.Workload)
			}
		}
	}
	return ""
}

// clockTick is the kernel's USER_HZ, the unit of a process's start time,
// which is 100 on every architecture Linux builds for.
const clockTick = 10 * time.Millisecond

// startedAfter reports whether a process that started start ticks after
// boot did so after t, with two seconds for the whole-second boot time.
func startedAfter(boot time.Time, start uint64, t time.Time) bool {
	if boot.IsZero() || start > uint64(math.MaxInt64/clockTick) {
		return false
	}
	return boot.Add(time.Duration(start) * clockTick).After(t.Add(2 * time.Second))
}

func (v *verifier) check(ctx context.Context, now time.Time, s model.Sample) {
	v.mu.Lock()
	due := v.dirty || now.Sub(v.checked) >= ChangesEvery
	v.mu.Unlock()
	if due {
		v.refresh(ctx, now)
	}
	v.mu.Lock()
	open := make([]*watched, 0, len(v.open))
	for _, w := range v.open {
		open = append(open, w)
	}
	v.mu.Unlock()
	slices.SortFunc(open, func(a, b *watched) int { return strings.Compare(a.tx.GetId(), b.tx.GetId()) })
	for _, w := range open {
		trouble := v.judge(ctx, now, w, s)
		v.mu.Lock()
		if w.trouble == "" {
			w.trouble = trouble
		}
		v.mu.Unlock()
		v.act(ctx, now, w)
	}
}

// judge looks at one fix's workloads in the latest sample and returns the
// first trouble it finds, or "".
func (v *verifier) judge(ctx context.Context, now time.Time, w *watched, s model.Sample) string {
	verifying := w.state == helperv1.State_STATE_VERIFYING
	ws := v.d.deps.Collector.Workloads()
	for _, t := range w.targets {
		name := ws[t.id].Label()
		if name == "" {
			name = strings.TrimPrefix(t.id, "/")
		}
		u, ok := s.Workloads[t.id]
		if !ok {
			if t.missing.IsZero() {
				t.missing = now
			}
			if now.Sub(t.missing) < MissingFor {
				continue
			}
			if c := v.docker(ctx, now, t); c != nil && c.GetOomKilled() && (!t.hasDocker || !t.oomKilled) {
				return fmt.Sprintf("%s was killed for lack of memory and has not come back", name)
			}
			if verifying {
				return fmt.Sprintf("%s stopped during verification", name)
			}
			continue
		}
		back := !t.missing.IsZero()
		t.missing = time.Time{}
		start, hasStart := v.d.deps.Collector.OldestStart(t.id)
		if !t.seen {
			t.seen, t.kills, t.highs, t.start, t.hasStart = true, u.OOMKills, u.HighEvents, start, hasStart
			if c := v.docker(ctx, now, t); c != nil {
				t.restarts, t.oomKilled, t.hasDocker = c.GetRestartCount(), c.GetOomKilled(), true
			}
			t.stalls = append(t.stalls[:0], stall{at: s.Time, total: u.PressureSome})
			v.d.mu.RLock()
			boot := v.d.machine.BootTime
			v.d.mu.RUnlock()
			if hasStart && startedAfter(boot, start, w.from) {
				return fmt.Sprintf("%s restarted", name)
			}
			continue
		}
		switch {
		case u.OOMKills > t.kills:
			return fmt.Sprintf("the kernel killed a process in %s for lack of memory", name)
		case back, u.OOMKills < t.kills, u.HighEvents < t.highs, hasStart && t.hasStart && start > t.start:
			return fmt.Sprintf("%s restarted", name)
		case !verifying && u.HighEvents > t.highs:
			return fmt.Sprintf("%s went over its MemoryHigh %d times, so it is throttled", name, u.HighEvents-t.highs)
		}
		if c := v.docker(ctx, now, t); verifying && c != nil && t.hasDocker && (c.GetRestartCount() > t.restarts || c.GetOomKilled() && !t.oomKilled) {
			return fmt.Sprintf("%s restarted", name)
		}
		if verifying {
			if why := t.pressure(s.Time, u.PressureSome, name); why != "" {
				return why
			}
		}
	}
	return ""
}

// pressure keeps the last minute of a workload's stall total and returns a
// reason when its share of that minute passes the workload's own 7-day 99th
// percentile.
func (t *target) pressure(at time.Time, total time.Duration, name string) string {
	t.stalls = append(t.stalls, stall{at: at, total: total})
	for len(t.stalls) > 2 && at.Sub(t.stalls[1].at) >= PressureWindow {
		t.stalls = t.stalls[1:]
	}
	first := t.stalls[0]
	span := at.Sub(first.at)
	if span < PressureWindow*5/6 {
		return ""
	}
	share, ok := store.PressureShare(first.total, total, span)
	if !ok {
		return ""
	}
	bar := PressureNoHistory
	if t.hasP99 {
		bar = max(t.p99, PressureFloor)
	}
	if share <= bar {
		return ""
	}
	if t.hasP99 {
		return fmt.Sprintf("%s stalled on memory %.1f%% of the last minute, above the 99th percentile of %.1f%% it showed before the fix", name, share, t.p99)
	}
	return fmt.Sprintf("%s stalled on memory %.1f%% of the last minute, and there is no pressure history to compare with", name, share)
}

// docker returns Docker's view of a container target, asking the helper
// at most once a minute. A service, or a box without Docker, gives nil.
func (v *verifier) docker(ctx context.Context, now time.Time, t *target) *helperv1.Container {
	if t.container == "" {
		return nil
	}
	if v.dockers == nil || now.Sub(v.dockAt) >= dockerEvery {
		resp, err := v.h.Containers(ctx)
		v.dockAt = now
		if err != nil {
			v.d.deps.Log.Warn("could not ask the helper about containers", "err", err)
			return v.dockers[t.container]
		}
		v.dockers = make(map[string]*helperv1.Container, len(resp.GetContainers()))
		for _, c := range resp.GetContainers() {
			v.dockers[c.GetId()] = c
		}
	}
	return v.dockers[t.container]
}

// act carries out what judging found: undo or reopen a fix in trouble, move
// a quiet one on. Nothing is done while snugbox pause is in force; the
// trouble waits and is acted on after resume.
func (v *verifier) act(ctx context.Context, now time.Time, w *watched) {
	if now.Before(w.nextAct) || v.d.Paused() {
		return
	}
	tx, id := w.tx, w.tx.GetId()
	log := v.d.deps.Log
	var err error
	switch {
	case w.trouble != "" && w.state == helperv1.State_STATE_VERIFYING && tx.GetAutoUndo():
		if _, err = v.h.Undo(ctx, id, clip(w.trouble), nil); err == nil {
			v.d.event(ctx, now, "fix_undone", id, w.trouble+"; Snugbox put the limits back")
		}
	case w.trouble != "":
		if _, err = v.h.Advance(ctx, id, helperv1.State_STATE_REOPENED, clip(w.trouble)); err == nil {
			v.d.event(ctx, now, "fix_reopened", id, w.trouble+"; a person should review it")
		}
	case w.state == helperv1.State_STATE_VERIFYING && now.Sub(w.from) >= helperVerifyWindow:
		if _, err = v.h.Advance(ctx, id, helperv1.State_STATE_WATCHING, ""); err == nil {
			v.d.event(ctx, now, "fix_verified", id, "30 quiet minutes; watched for 7 days from now")
		}
	case w.state == helperv1.State_STATE_WATCHING && now.Sub(tx.GetStateSince().AsTime()) >= helperWatchWindow:
		if _, err = v.h.Advance(ctx, id, helperv1.State_STATE_DONE, ""); err == nil {
			v.d.event(ctx, now, "fix_done", id, "7 days without trouble")
		}
	default:
		return
	}
	w.nextAct = now.Add(actEvery)
	if err != nil {
		var he *helperapi.Error
		if errors.As(err, &he) {
			switch he.Code {
			case helperv1.ErrorCode_ERROR_CODE_PAUSED:
				return
			case helperv1.ErrorCode_ERROR_CODE_NOT_FOUND, helperv1.ErrorCode_ERROR_CODE_PRECONDITION, helperv1.ErrorCode_ERROR_CODE_DENIED:
				// Someone else moved the fix on, such as a person running
				// snugbox undo; read the journal again.
				v.mu.Lock()
				v.dirty = true
				v.mu.Unlock()
			}
		}
		log.Error("could not act on a fix", "tx", id, "trouble", w.trouble, "err", err)
		return
	}
	v.mu.Lock()
	v.dirty = true
	v.mu.Unlock()
}

// The helper's own windows: it refuses to move a fix on any sooner.
const (
	helperVerifyWindow = 30 * time.Minute
	helperWatchWindow  = 7 * 24 * time.Hour
)

// clip shortens a reason to what the helper takes, at a character boundary
// so the message stays valid UTF-8.
func clip(s string) string {
	if len(s) <= helperapi.MaxReason {
		return s
	}
	cut := helperapi.MaxReason
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return s[:cut]
}

// changes is GET /v1/changes.
func (v *verifier) changes(now time.Time) localapi.Changes {
	v.mu.Lock()
	defer v.mu.Unlock()
	out := localapi.Changes{Helper: v.err == nil && !v.read.IsZero(), Read: v.read, Pending: v.dirty, Changes: []localapi.Change{}}
	if v.err != nil {
		out.Error = v.err.Error()
	}
	for _, tx := range v.list {
		c := localapi.Change{ID: tx.GetId(), Kind: helperapi.KindOf(tx.GetFix()), State: helperapi.StateName(tx.GetState()),
			Started: tx.GetStarted().AsTime(), Since: tx.GetStateSince().AsTime(), Reason: tx.GetReason(), Cgroups: tx.GetCgroups()}
		for _, ch := range tx.GetChanges() {
			c.Changes = append(c.Changes, localapi.ChangeItem{Target: ch.GetTarget(), Old: ch.GetOldValue(), New: ch.GetNewValue(), File: ch.GetFile(), Manual: ch.GetManual()})
		}
		switch tx.GetState() {
		case helperv1.State_STATE_VERIFYING:
			from := c.Since
			if w := v.open[tx.GetId()]; w != nil {
				from, c.Trouble = w.from, w.trouble
			}
			c.VerifyEnds = from.Add(helperVerifyWindow)
		case helperv1.State_STATE_WATCHING:
			if w := v.open[tx.GetId()]; w != nil {
				c.Trouble = w.trouble
			}
			c.Counted = c.Trouble == "" && now.Sub(c.Since) >= CountedAfter
		case helperv1.State_STATE_DONE:
			c.Counted = true
		}
		out.Changes = append(out.Changes, c)
	}
	return out
}
