package audit

import (
	"context"
	"errors"
	"slices"
	"testing"
	"time"

	"snugbox.dev/snugbox/agent/internal/collect"
	"snugbox.dev/snugbox/agent/internal/fix"
	"snugbox.dev/snugbox/agent/internal/model"
	"snugbox.dev/snugbox/agent/internal/provider"
	"snugbox.dev/snugbox/agent/internal/testbox"
)

const mib, gib = model.MiB, model.GiB

type fixedProber provider.Info

func (p fixedProber) Detect(context.Context) provider.Info { return provider.Info(p) }

// clock is a fake clock that a test moves forward from Sleep.
type clock struct {
	now   time.Time
	steps int
	tick  func(step int)
}

func (c *clock) Now() time.Time { return c.now }

func (c *clock) Sleep(_ context.Context, d time.Duration) error {
	c.now = c.now.Add(d)
	c.steps++
	if c.tick != nil {
		c.tick(c.steps)
	}
	return nil
}

func shopBox() (*testbox.Box, *testbox.Workload) {
	b := testbox.New()
	app := b.Add(&testbox.Workload{Path: "system.slice/docker-" + hexID('a') + ".scope", Programs: []string{"node"}, Cmdline: "node\x00server.js\x00", Anon: 1100 * mib, ActiveFile: 100 * mib, InactiveFile: 300 * mib})
	b.Add(&testbox.Workload{Path: "system.slice/docker-" + hexID('b') + ".scope", Programs: []string{"postgres"}, Anon: 300 * mib, Shmem: 128 * mib, ActiveFile: 200 * mib, InactiveFile: 400 * mib})
	b.Add(&testbox.Workload{Path: "system.slice/backup.service", Programs: []string{"restic"}, Anon: 300 * mib})
	b.Add(&testbox.Workload{Path: "system.slice/ssh.service", Programs: []string{"sshd"}, Anon: 6 * mib})
	return b, app
}

func hexID(c byte) string {
	id := make([]byte, 64)
	for i := range id {
		id[i] = c
	}
	return string(id)
}

type names map[string]collect.ContainerMeta

func (n names) Containers(context.Context, []string) (map[string]collect.ContainerMeta, error) {
	return n, nil
}

func deps(t *testing.T, b *testbox.Box, clk *clock, info provider.Info) Deps {
	t.Helper()
	c, err := collect.New(collect.Options{FS: b, PageSize: 4096, Containers: names{
		hexID('a'): {Name: "shop-app-1"},
		hexID('b'): {Name: "shop-db-1", Image: "postgres:18"},
	}})
	if err != nil {
		t.Fatal(err)
	}
	return Deps{Collector: c, Host: nil, Prober: fixedProber(info), Now: clk.Now, Sleep: clk.Sleep, FS: b, Root: true,
		KernelLog: func(time.Time) (collect.KernelLog, error) { return collect.KernelLog{}, errors.New("no log in tests") }}
}

func TestRunShopBox(t *testing.T) {
	b, app := shopBox()
	b.OOMD = true
	clk := &clock{now: time.Date(2026, 10, 31, 9, 0, 0, 0, time.UTC)}
	clk.tick = func(step int) {
		b.Update(func(*testbox.Box) {
			app.Anon = 1100*mib + uint64(step%5)*20*mib
			b.CPUBusy += 200
			b.CPUIdle += 600
		})
	}
	r, err := Run(context.Background(), deps(t, b, clk, provider.Info{Provider: provider.DigitalOcean, Region: "fra1", FromMetadata: true}), Options{Duration: time.Minute})
	if err != nil {
		t.Fatal(err)
	}
	if r.Summary.Samples != 31 || r.Summary.Window() != time.Minute {
		t.Fatalf("%d samples over %v", r.Summary.Samples, r.Summary.Window())
	}
	if r.Advice.Current == nil || r.Advice.Current.ID != "s-4vcpu-8gb" || !r.Advice.PaysForExact {
		t.Fatalf("current plan %+v", r.Advice.Current)
	}
	if r.Estimate.Confidence != model.ConfidenceLow {
		t.Errorf("a one-minute audit has confidence %v", r.Estimate.Confidence)
	}
	if e := r.Estimate; e.Snug < e.Need || e.Snug > 4*gib {
		t.Errorf("snug %d MiB, need %d MiB", e.Snug/mib, e.Need/mib)
	}
	if len(r.Advice.Options) == 0 || r.Advice.Withheld {
		t.Fatalf("no plans advised: %+v", r.Advice)
	}
	var kinds []RiskKind
	for _, k := range r.Risks {
		kinds = append(kinds, k.Kind)
	}
	want := []RiskKind{RiskNoSwap, RiskNoOOMProtection, RiskUnlimited}
	if !slices.Equal(kinds, want) {
		t.Errorf("risks %v, want %v", kinds, want)
	}
	unl := r.Risks[2]
	if unl.Count != 2 || unl.Of != 2 || !slices.Equal(unl.Names, []string{"shop-app-1", "shop-db-1"}) {
		t.Errorf("unlimited containers %+v", unl)
	}
	var fixes []fix.Kind
	for _, f := range r.Fixes {
		fixes = append(fixes, f.Kind)
	}
	if !slices.Equal(fixes, []fix.Kind{fix.KindContainer, fix.KindService, fix.KindOOMProtection, fix.KindHeap}) {
		t.Errorf("fixes %v", fixes)
	}
	if c := r.Fixes[0]; len(c.Limits) != 1 || c.Limits[0].Name != "shop-app-1" || !c.Limits[0].LowConfidence {
		t.Errorf("the database container got a limit, or the app's limit is not marked early: %+v", c.Limits)
	}
	if s := r.Fixes[1]; len(s.Limits) != 1 || s.Limits[0].Name != "backup.service" {
		t.Errorf("service limits %+v; ssh must be left alone", s.Limits)
	}
	if r.Workloads["/system.slice/docker-"+hexID('b')+".scope"].Database != "PostgreSQL" {
		t.Error("the postgres container was not recognised as a database")
	}
	if !slices.ContainsFunc(r.Gaps, func(g collect.Gap) bool { return g.What == "the kernel log at /dev/kmsg" }) {
		t.Errorf("unreadable kernel log not reported: %+v", r.Gaps)
	}
}

func TestRunWithholdsPlansUnderPressure(t *testing.T) {
	b, _ := shopBox()
	b.PressureFull = 5 * time.Hour
	clk := &clock{now: time.Date(2026, 10, 31, 9, 0, 0, 0, time.UTC)}
	r, err := Run(context.Background(), deps(t, b, clk, provider.Info{}), Options{Duration: 20 * time.Second})
	if err != nil {
		t.Fatal(err)
	}
	if !r.Estimate.Upward.Pressure() || !r.Advice.Withheld || len(r.Advice.Options) != 0 {
		t.Errorf("5 hours of full pressure in 10 days: upward %+v, withheld %v, %d options", r.Estimate.Upward, r.Advice.Withheld, len(r.Advice.Options))
	}
	if r.Advice.PaysForExact || r.Advice.PaysFor != b.MemTotal {
		t.Errorf("unknown provider: pays for %d, exact %v", r.Advice.PaysFor, r.Advice.PaysForExact)
	}
}

func TestRunKills(t *testing.T) {
	b, app := shopBox()
	b.Uptime = 3 * 24 * time.Hour
	b.OOMKills = 2
	app.OOMKills = 1
	clk := &clock{now: time.Date(2026, 10, 31, 9, 0, 0, 0, time.UTC)}
	clk.tick = func(step int) {
		if step == 3 {
			b.Update(func(*testbox.Box) { app.OOMKills, b.OOMKills = 2, 3 })
		}
	}
	r, err := Run(context.Background(), deps(t, b, clk, provider.Info{}), Options{Duration: 20 * time.Second})
	if err != nil {
		t.Fatal(err)
	}
	if r.Risks[0].Kind != RiskKills || r.Risks[0].Count != 3 || !slices.Equal(r.Risks[0].Names, []string{"shop-app-1"}) {
		t.Errorf("kills risk %+v", r.Risks[0])
	}
	if !r.Advice.Withheld {
		t.Error("plans offered to a box with recent kills")
	}
}

func TestRunRefusals(t *testing.T) {
	b, _ := shopBox()
	clk := &clock{now: time.Now()}
	for _, d := range []time.Duration{time.Second, 2 * time.Hour} {
		if _, err := Run(context.Background(), deps(t, b, clk, provider.Info{}), Options{Duration: d}); err == nil {
			t.Errorf("a %v audit was accepted", d)
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	d := deps(t, b, clk, provider.Info{})
	d.Sleep = func(ctx context.Context, _ time.Duration) error { return ctx.Err() }
	if _, err := Run(ctx, d, Options{}); !errors.Is(err, context.Canceled) {
		t.Errorf("cancelled audit: %v", err)
	}
}

func TestInterval(t *testing.T) {
	for d, want := range map[time.Duration]time.Duration{time.Minute: 2 * time.Second, 10 * time.Minute: 2 * time.Second, time.Hour: 2 * time.Second} {
		if got := Interval(d); got != want {
			t.Errorf("Interval(%v) = %v, want %v", d, got, want)
		}
	}
}

func TestDiskFor(t *testing.T) {
	disks := []collect.Disk{{MountPoint: "/", FSType: "ext4"}, {MountPoint: "/var", FSType: "xfs"}, {MountPoint: "/var/lib/docker", FSType: "btrfs"}}
	if d, ok := diskFor(disks, "/var/lib/snugbox"); !ok || d.FSType != "xfs" {
		t.Errorf("diskFor = %+v, %v; want the /var filesystem", d, ok)
	}
	if d, ok := diskFor(disks[:1], "/var/lib/snugbox"); !ok || d.FSType != "ext4" {
		t.Errorf("diskFor = %+v, %v; want /", d, ok)
	}
}
