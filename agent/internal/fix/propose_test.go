package fix

import (
	"testing"
	"time"

	"snugbox.dev/snugbox/agent/internal/collect"
	"snugbox.dev/snugbox/agent/internal/kernel"
	"snugbox.dev/snugbox/agent/internal/model"
	helperv1 "snugbox.dev/snugbox/gen/snugbox/helper/v1"
)

const mib, gib = model.MiB, model.GiB

func baseInput() Input {
	return Input{
		Machine: collect.Machine{
			Virt: "kvm", CPUs: 4, MemTotal: 8 * gib, CgroupV2: true,
			Kernel: kernel.Release{Major: 6, Minor: 8},
			Zswap:  collect.Zswap{Available: true},
		},
		Workloads: map[string]model.Workload{
			"/system.slice/docker-a.scope":           {ID: "/system.slice/docker-a.scope", Kind: model.KindContainer, Name: "shop-app-1", Runtime: "node"},
			"/system.slice/docker-b.scope":           {ID: "/system.slice/docker-b.scope", Kind: model.KindContainer, Name: "shop-cache-1", Max: model.Limit{Bytes: 256 * mib, Set: true}},
			"/system.slice/backup.service":           {ID: "/system.slice/backup.service", Kind: model.KindService, Name: "backup.service"},
			"/system.slice/ssh.service":              {ID: "/system.slice/ssh.service", Kind: model.KindService, Name: "ssh.service"},
			"/system.slice/systemd-journald.service": {ID: "/system.slice/systemd-journald.service", Kind: model.KindService, Name: "systemd-journald.service"},
			"/system.slice/api.service":              {ID: "/system.slice/api.service", Kind: model.KindService, Name: "api.service", Runtime: "java", HeapFlags: []string{"-Xmx2g"}},
		},
		Summary: model.Summary{Workloads: map[string]model.WorkloadSummary{
			"/system.slice/docker-a.scope":           {WorkingSetQ95: 1100 * mib, WorkingSetQ999: 1240 * mib, WorkingSetMax: 1300 * mib, HardMax: 1100 * mib},
			"/system.slice/docker-b.scope":           {WorkingSetQ999: 140 * mib},
			"/system.slice/backup.service":           {WorkingSetQ999: 60 * mib, HardMax: 420 * mib, WorkingSetMax: 430 * mib},
			"/system.slice/ssh.service":              {WorkingSetQ999: 10 * mib},
			"/system.slice/systemd-journald.service": {WorkingSetQ999: 40 * mib},
			"/system.slice/api.service":              {WorkingSetQ999: 2000 * mib},
		}},
		Window:      14 * 24 * time.Hour,
		SystemdOOMD: true,
		FreeDisk:    40 * gib,
		SwapFS:      "ext4",
	}
}

func find(ps []Proposal, k Kind) (Proposal, bool) {
	for _, p := range ps {
		if p.Kind == k {
			return p, true
		}
	}
	return Proposal{}, false
}

func TestProposeAll(t *testing.T) {
	ps := Propose(baseInput())
	var kinds []Kind
	for _, p := range ps {
		kinds = append(kinds, p.Kind)
	}
	want := []Kind{KindSwap, KindContainer, KindService, KindOOMProtection, KindHeap}
	if len(kinds) != len(want) {
		t.Fatalf("kinds %v, want %v", kinds, want)
	}
	for i := range want {
		if kinds[i] != want[i] {
			t.Fatalf("kinds %v, want %v", kinds, want)
		}
	}
	swap, _ := find(ps, KindSwap)
	if swap.SwapSize != 4*gib || swap.Compressor != "zstd" || !swap.Zswap || !swap.Shrinker || swap.Risk != RiskLow {
		t.Errorf("swap %+v", swap)
	}
	c, _ := find(ps, KindContainer)
	if len(c.Limits) != 1 || c.Limits[0].Name != "shop-app-1" {
		t.Fatalf("container limits %+v", c.Limits)
	}
	// max(1.3 x 1240, 1.2 x 1100) = 1612 MiB, rounded up to 1664 MiB.
	if l := c.Limits[0]; l.Max != 1664*mib || l.Soft != 1100*mib || l.LowConfidence {
		t.Errorf("container limit %+v", l)
	}
	s, _ := find(ps, KindService)
	if len(s.Limits) != 2 || s.Limits[0].Name != "api.service" || s.Limits[1].Name != "backup.service" {
		t.Fatalf("service limits %+v; ssh, systemd units and limited services must be left alone", s.Limits)
	}
	// api: base 2000 MiB, MemoryHigh 2300 -> 2304, MemoryMax 2800 -> 2816.
	if l := s.Limits[0]; l.Soft != 2304*mib || l.Max != 2816*mib {
		t.Errorf("api limit %+v", l)
	}
	// backup: base max(60, 420) = 420 MiB, MemoryHigh 483 -> 512, MemoryMax 588 -> 640.
	if l := s.Limits[1]; l.Soft != 512*mib || l.Max != 640*mib {
		t.Errorf("backup limit %+v", l)
	}
	h, _ := find(ps, KindHeap)
	if len(h.Heaps) != 1 || h.Heaps[0].Setting != "--max-old-space-size=1248" || !h.NeedsRestart {
		t.Errorf("heap caps %+v; java with -Xmx is left alone, node gets 75%% of its proposed 1664 MiB limit", h.Heaps)
	}
}

func TestProposeSwapPreconditions(t *testing.T) {
	cases := map[string]func(*Input){
		"existing swap":   func(in *Input) { in.Machine.Swaps = []kernel.Swap{{Filename: "/swap.img"}} },
		"zram":            func(in *Input) { in.Machine.ZramSwap = true },
		"lxc container":   func(in *Input) { in.Machine.Virt = "lxc" },
		"wsl":             func(in *Input) { in.Machine.Virt = "wsl" },
		"zfs":             func(in *Input) { in.SwapFS = "zfs" },
		"too little disk": func(in *Input) { in.FreeDisk = 4*gib + 400*mib + 2*gib - 1 },
	}
	for name, mutate := range cases {
		in := baseInput()
		mutate(&in)
		if _, ok := find(Propose(in), KindSwap); ok {
			t.Errorf("%s: swap proposed", name)
		}
	}
	in := baseInput()
	in.Machine.MemTotal = 2 * gib
	in.Machine.Zswap = collect.Zswap{}
	in.Summary.CPUQ95 = 3.5
	swap, ok := find(Propose(in), KindSwap)
	if !ok || swap.SwapSize != gib || swap.Zswap || swap.Shrinker || swap.Compressor != "lz4" {
		t.Errorf("2 GiB box, no zswap, busy CPU: %+v", swap)
	}
}

func TestLimitForSockHeavy(t *testing.T) {
	hard, _ := LimitFor(model.WorkloadSummary{WorkingSetQ999: 1000 * mib, WorkingSetMax: 1000 * mib, SockMax: 150 * mib})
	if hard != 1536*mib {
		t.Errorf("sock-heavy limit %d MiB, want 1.5 x 1000 rounded to 1536", hard/mib)
	}
}

func TestProposeShmemNeedsSwap(t *testing.T) {
	in := baseInput()
	s := in.Summary.Workloads["/system.slice/docker-a.scope"]
	s.ShmemMax = 400 * mib
	in.Summary.Workloads["/system.slice/docker-a.scope"] = s
	if _, ok := find(Propose(in), KindContainer); ok {
		t.Error("a container whose shared memory is 30% of its working set got a limit on a box without swap")
	}
	in.Machine.Swaps = []kernel.Swap{{Filename: "/swap.img"}}
	if _, ok := find(Propose(in), KindContainer); !ok {
		t.Error("no limit even with swap")
	}
}

func TestProposeLowConfidence(t *testing.T) {
	in := baseInput()
	in.Window = time.Minute
	c, _ := find(Propose(in), KindContainer)
	if !c.Limits[0].LowConfidence {
		t.Error("a one-minute audit produced a limit without the low-confidence mark")
	}
}

func TestProposeSwapFilesystemsAndFloor(t *testing.T) {
	// Btrfs is proposed; the helper checks for mkswapfile before it applies.
	for fs, want := range map[string]bool{"ext4": true, "xfs": true, "btrfs": true, "zfs": false, "tmpfs": false} {
		in := baseInput()
		in.SwapFS = fs
		if _, ok := find(Propose(in), KindSwap); ok != want {
			t.Errorf("swap on %s proposed: %v, want %v", fs, ok, want)
		}
	}
	in := baseInput()
	in.Machine.MemTotal = 200 * mib
	if p, ok := find(Propose(in), KindSwap); ok {
		t.Errorf("a %d MiB swap file proposed, below the helper's 128 MiB floor", p.SwapSize/mib)
	}
}

func TestServiceLimitsStayApart(t *testing.T) {
	in := baseInput()
	in.Summary.Workloads["/system.slice/backup.service"] = model.WorkloadSummary{WorkingSetQ999: 64 * mib, HardMax: 64 * mib, WorkingSetMax: 64 * mib}
	s, _ := find(Propose(in), KindService)
	for _, l := range s.Limits {
		if l.Name == "backup.service" && (l.Soft != 128*mib || l.Max != 192*mib) {
			t.Errorf("a 64 MiB service: MemoryHigh %d MiB, MemoryMax %d MiB; want 128 and 192 so MemoryMax stays above MemoryHigh", l.Soft/mib, l.Max/mib)
		}
	}
}

func TestHeapCapsByRuntime(t *testing.T) {
	in := baseInput()
	in.Workloads["/system.slice/api.service"] = model.Workload{ID: "/system.slice/api.service", Kind: model.KindService, Name: "api.service", Runtime: "java"}
	in.Workloads["/system.slice/worker.service"] = model.Workload{ID: "/system.slice/worker.service", Kind: model.KindService, Name: "worker.service", Runtime: "ruby", Threads: 12}
	in.Workloads["/system.slice/cron.service"] = model.Workload{ID: "/system.slice/cron.service", Kind: model.KindService, Name: "cron.service", Runtime: "python", Threads: 1}
	in.Workloads["/system.slice/docker-j.scope"] = model.Workload{ID: "/system.slice/docker-j.scope", Kind: model.KindContainer, Name: "shop-search-1",
		Runtime: "java", Max: model.Limit{Bytes: gib, Set: true}}
	h, _ := find(Propose(in), KindHeap)
	got := map[string]Heap{}
	for _, x := range h.Heaps {
		got[x.Name] = x
	}
	// api: proposed MemoryMax 2816 MiB, three quarters is 2112 MiB.
	if a := got["api.service"]; a.Unit != "api.service" || a.Env != "JAVA_TOOL_OPTIONS" || a.Setting != "-Xmx2112m" || a.Bytes != 2112*mib {
		t.Errorf("java service %+v", a)
	}
	if w := got["worker.service"]; w.Env != "MALLOC_ARENA_MAX" || w.Setting != "2" || w.Unit != "worker.service" {
		t.Errorf("threaded ruby %+v", w)
	}
	if _, ok := got["cron.service"]; ok {
		t.Error("a single-threaded python service got MALLOC_ARENA_MAX")
	}
	if s := got["shop-search-1"]; s.Unit != "" || s.Setting != "-XX:MaxRAMPercentage=75" {
		t.Errorf("java container %+v; a container gets advice, sized against its own limit", s)
	}
}

// GOMEMLIMIT counts all the memory the Go runtime manages, so a Go program
// gets nine tenths of its limit where Java and Node.js get three quarters.
// Units no fix may touch get nothing, whatever runs in them.
func TestHeapCapsForGo(t *testing.T) {
	in := baseInput()
	in.Workloads["/system.slice/caddy.service"] = model.Workload{ID: "/system.slice/caddy.service", Kind: model.KindService, Name: "caddy.service", Runtime: "go"}
	in.Summary.Workloads["/system.slice/caddy.service"] = model.WorkloadSummary{WorkingSetQ999: 300 * mib, HardMax: 280 * mib, WorkingSetMax: 310 * mib}
	in.Workloads["/system.slice/docker-g.scope"] = model.Workload{ID: "/system.slice/docker-g.scope", Kind: model.KindContainer, Name: "shop-proxy-1",
		Runtime: "go", Max: model.Limit{Bytes: 200 * mib, Set: true}}
	for _, unit := range []string{"snugboxd.service", "docker.service", "containerd.service"} {
		id := "/system.slice/" + unit
		in.Workloads[id] = model.Workload{ID: id, Kind: model.KindService, Name: unit, Runtime: "go"}
		in.Summary.Workloads[id] = model.WorkloadSummary{WorkingSetQ999: 300 * mib, HardMax: 280 * mib}
	}
	// snugboxd has a MemoryMax of its own, which a heap cap would be sized
	// against; docker and containerd have none, so a service limit would be
	// proposed for them.
	snugboxd := in.Workloads["/system.slice/snugboxd.service"]
	snugboxd.Max = model.Limit{Bytes: 64 * mib, Set: true}
	in.Workloads["/system.slice/snugboxd.service"] = snugboxd
	ps := Propose(in)
	h, _ := find(ps, KindHeap)
	got := map[string]Heap{}
	for _, x := range h.Heaps {
		got[x.Name] = x
	}
	// caddy: proposed MemoryMax is 1.4 times 300 MiB, rounded up to 448 MiB;
	// nine tenths of that is 403 MiB in whole MiB.
	if c := got["caddy.service"]; c.Unit != "caddy.service" || c.Env != "GOMEMLIMIT" || c.Setting != "403MiB" || c.Bytes != 403*mib {
		t.Errorf("go service %+v", c)
	}
	if p := got["shop-proxy-1"]; p.Unit != "" || p.Setting != "180MiB" {
		t.Errorf("go container %+v; a container gets advice against its own 200 MiB limit", p)
	}
	for _, unit := range []string{"snugboxd.service", "docker.service", "containerd.service"} {
		if _, ok := got[unit]; ok {
			t.Errorf("%s got a heap cap", unit)
		}
	}
	s, _ := find(ps, KindService)
	for _, l := range s.Limits {
		if l.Name == "docker.service" || l.Name == "containerd.service" {
			t.Errorf("%s got a limit; a kill there stops every container", l.Name)
		}
	}
}

func TestProposeOOMProtection(t *testing.T) {
	cases := []struct {
		name            string
		oomd, earlyoom  bool
		cgroupV2        bool
		policy          bool
		running, daemon string
	}{
		{name: "systemd-oomd installed", oomd: true, cgroupV2: true, daemon: DaemonSystemdOOMD},
		{name: "both installed, systemd-oomd first", oomd: true, earlyoom: true, cgroupV2: true, daemon: DaemonSystemdOOMD},
		{name: "only earlyoom installed", earlyoom: true, cgroupV2: true, daemon: DaemonEarlyoom},
		{name: "earlyoom works without cgroup v2", oomd: true, earlyoom: true, daemon: DaemonEarlyoom},
		{name: "neither installed", cgroupV2: true},
		{name: "one runs already", oomd: true, earlyoom: true, cgroupV2: true, running: "earlyoom"},
		// Fedora with systemd-oomd stopped: its policy is there, which the
		// helper leaves alone, so no fix at all, not even earlyoom.
		{name: "a policy of the distribution's, daemon off", oomd: true, earlyoom: true, cgroupV2: true, policy: true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			in := baseInput()
			in.SystemdOOMD, in.Earlyoom = c.oomd, c.earlyoom
			in.Machine.CgroupV2, in.Machine.OOMDaemon = c.cgroupV2, c.running
			if c.policy {
				in.Machine.OOMPolicy = collect.OOMPolicy{Slice: "system.slice", Value: "kill", File: "/usr/lib/systemd/system/system.slice.d/10-oomd-per-slice-defaults.conf"}
			}
			p, ok := find(Propose(in), KindOOMProtection)
			if c.daemon == "" {
				if ok {
					t.Errorf("offered %+v", p)
				}
				return
			}
			if !ok || p.OOMDaemon != c.daemon || p.Risk != RiskMedium {
				t.Fatalf("got %+v, %v; want %s", p, ok, c.daemon)
			}
			req, _, err := Request(p, in.Workloads, RequestOptions{})
			if err != nil {
				t.Fatal(err)
			}
			want := helperv1.OOMDaemon_OOM_DAEMON_SYSTEMD_OOMD
			if c.daemon == DaemonEarlyoom {
				want = helperv1.OOMDaemon_OOM_DAEMON_EARLYOOM
			}
			if got := req.GetOomProtection().GetDaemon(); got != want {
				t.Errorf("request names %v, want %v", got, want)
			}
		})
	}
}

// PHP-FPM gets a pm.max_children line as advice, worked out from its largest
// worker and its limit, and the helper is never asked to write it.
func TestPHPFPMAdvice(t *testing.T) {
	in := baseInput()
	id := "/system.slice/php8.3-fpm.service"
	in.Workloads[id] = model.Workload{ID: id, Kind: model.KindService, Name: "php8.3-fpm.service", Runtime: "php-fpm", Workers: 6, WorkerBytes: 52 * mib}
	in.Summary.Workloads[id] = model.WorkloadSummary{WorkingSetQ999: 400 * mib, HardMax: 380 * mib, WorkingSetMax: 410 * mib}
	idle := "/system.slice/php-idle.service"
	in.Workloads[idle] = model.Workload{ID: idle, Kind: model.KindService, Name: "php-idle.service", Runtime: "php-fpm"}
	in.Summary.Workloads[idle] = model.WorkloadSummary{WorkingSetQ999: 400 * mib, HardMax: 380 * mib}
	p, _ := find(Propose(in), KindHeap)
	var fpm *Heap
	for i, h := range p.Heaps {
		switch h.Name {
		case "php8.3-fpm.service":
			fpm = &p.Heaps[i]
		case "php-idle.service":
			t.Errorf("advice without a measured worker: %+v", h)
		}
	}
	// The proposed MemoryMax is 576 MiB; 90% of it holds 9 workers of 52 MiB.
	if fpm == nil || fpm.Unit != "" || fpm.Setting != "pm.max_children = 9" || fpm.Limit != 576*mib || fpm.Workers != 6 {
		t.Fatalf("php-fpm %+v", fpm)
	}
	f, advice, err := Request(p, in.Workloads, RequestOptions{Only: []string{"php8.3-fpm.service"}})
	if err != nil || f != nil {
		t.Fatalf("request %v, %v: PHP-FPM must never reach the helper", f, err)
	}
	want := "php8.3-fpm.service: set pm.max_children = 9 in the pool's file, such as www.conf, then reload PHP-FPM. " +
		"That many workers of the largest size seen, 52 MiB, fit in 90% of its 576 MiB limit; 6 run now."
	if len(advice) != 1 || advice[0] != want {
		t.Errorf("advice %q", advice)
	}
	if n := FPMWorkers(100*mib, 200*mib); n != 0 {
		t.Errorf("a worker bigger than the limit gives %d", n)
	}
}
