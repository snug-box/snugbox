package helper

import (
	"encoding/json"
	"math"
	"path"
	"slices"
	"strings"
	"testing"
	"time"

	"snugbox.dev/snugbox/agent/internal/docker"
	"snugbox.dev/snugbox/agent/internal/helperapi"
	helperv1 "snugbox.dev/snugbox/gen/snugbox/helper/v1"
)

const (
	mib = helperapi.MiB
	gib = helperapi.GiB
	cid = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
)

func swapFix(shrinker bool) *helperv1.Fix {
	return &helperv1.Fix{Kind: &helperv1.Fix_Swap{Swap: &helperv1.SwapFix{SizeBytes: gib, Compressor: helperv1.Compressor_COMPRESSOR_ZSTD, Shrinker: shrinker}}}
}

func serviceFix(unit string, high, maxB uint64) *helperv1.Fix {
	return &helperv1.Fix{Kind: &helperv1.Fix_ServiceLimits{ServiceLimits: &helperv1.ServiceLimitsFix{
		Limits: []*helperv1.ServiceLimit{{Unit: unit, HighBytes: high, MaxBytes: maxB}}}}}
}

func containerFix(maxB, res uint64, persist bool) *helperv1.Fix {
	return &helperv1.Fix{Kind: &helperv1.Fix_ContainerLimits{ContainerLimits: &helperv1.ContainerLimitsFix{
		Persist: persist, Limits: []*helperv1.ContainerLimit{{ContainerId: cid, MaxBytes: maxB, ReservationBytes: res}}}}}
}

func heapFix(unit string, rt helperv1.Runtime, heap uint64) *helperv1.Fix {
	return &helperv1.Fix{Kind: &helperv1.Fix_HeapCaps{HeapCaps: &helperv1.HeapCapsFix{Caps: []*helperv1.HeapCap{{Unit: unit, Runtime: rt, HeapBytes: heap}}}}}
}

func oomFix() *helperv1.Fix {
	return &helperv1.Fix{Kind: &helperv1.Fix_OomProtection{OomProtection: &helperv1.OOMProtectionFix{}}}
}

func TestSwapApplyAndUndo(t *testing.T) {
	h := newHarness(t)
	dry, _ := h.do(applyReq(swapFix(true), true), Root)
	if dry.GetApply() == nil || dry.GetApply().GetTransaction() != "" || len(dry.GetApply().GetChanges()) == 0 {
		t.Fatalf("dry run: %v", dry)
	}
	if len(h.e.jr.Entries()) != 0 || len(h.box.snaps) != 0 || slices.ContainsFunc(h.box.calls, func(c string) bool { return strings.Contains(c, "enable") }) {
		t.Fatal("a dry run changed something")
	}

	a := h.mustApply(swapFix(true))
	tx := a.GetTransaction()
	if tx != "20261031-100000-swap" {
		t.Errorf("transaction %q", tx)
	}
	if got, want := h.states(tx), []string{"approved", "preflight", "snapshot", "applying", "verifying"}; !slices.Equal(got, want) {
		t.Errorf("journal states %v, want %v", got, want)
	}
	if !slices.Contains(h.box.swaps, SwapFile) {
		t.Error("the swap file is not in use")
	}
	for p, want := range map[string]string{
		path.Join(ZswapDir, "enabled"): "Y\n", path.Join(ZswapDir, "compressor"): "zstd\n", path.Join(ZswapDir, "shrinker_enabled"): "Y\n",
	} {
		if h.box.files[p] != want {
			t.Errorf("%s = %q, want %q", p, h.box.files[p], want)
		}
	}
	unit := h.box.files[path.Join(UnitDir, SwapUnit)]
	if !strings.Contains(unit, "What="+SwapFile) || !strings.Contains(unit, "WantedBy=swap.target") || !strings.HasPrefix(unit, "# Made by Snugbox for transaction "+tx) {
		t.Errorf("swap unit:\n%s", unit)
	}
	boot := h.box.files[TmpFiles]
	if !strings.Contains(boot, "w /sys/module/zswap/parameters/enabled - - - - Y") || strings.Index(boot, "compressor") > strings.Index(boot, "/enabled") {
		t.Errorf("tmpfiles entry must set the compressor before turning zswap on:\n%s", boot)
	}

	resp, _ := h.do(undoReq("", ""), Root)
	if resp.GetUndo().GetTransaction() != tx {
		t.Fatalf("undo: %v", resp)
	}
	if slices.Contains(h.box.swaps, SwapFile) || h.box.files[SwapFile] != "" || h.box.files[path.Join(UnitDir, SwapUnit)] != "" || h.box.files[TmpFiles] != "" {
		t.Error("undo left part of the swap fix")
	}
	if h.box.files[path.Join(ZswapDir, "enabled")] != "N\n" || h.box.files[path.Join(ZswapDir, "compressor")] != "lzo\n" {
		t.Error("undo did not put the zswap settings back")
	}
	if h.state(tx) != helperv1.State_STATE_UNDONE {
		t.Errorf("state %s after undo", h.state(tx))
	}
	if v := h.e.find(tx); v.reason != "asked for with snugbox undo" {
		t.Errorf("reason %q after a plain undo", v.reason)
	}
}

// Blueprint section 16.2: undo never runs swapoff under pressure.
func TestSwapUndoWaitsForCalm(t *testing.T) {
	h := newHarness(t)
	tx := h.mustApply(swapFix(false)).GetTransaction()
	h.box.files["/proc/pressure/memory"] = "some avg10=9.00 avg60=6.50 avg300=2.00 total=900\nfull avg10=1.00 avg60=0.50 avg300=0.10 total=90\n"
	h.box.calls = nil
	resp, _ := h.do(undoReq(tx, ""), Root)
	wantError(t, resp, helperv1.ErrorCode_ERROR_CODE_PRESSURE, "could set off the out-of-memory killer")
	if slices.ContainsFunc(h.box.calls, func(c string) bool { return strings.Contains(c, "stop") }) || !slices.Contains(h.box.swaps, SwapFile) {
		t.Error("swap was turned off under pressure")
	}

	h.box.files["/proc/pressure/memory"] = "some avg10=0.00 avg60=0.00 avg300=0.00 total=900\nfull avg10=0.00 avg60=0.00 avg300=0.00 total=90\n"
	h.box.swapKiB = 900 * 1024
	resp, _ = h.do(undoReq(tx, ""), Root)
	wantError(t, resp, helperv1.ErrorCode_ERROR_CODE_PRESSURE, "only 1.0 GiB is free")

	h.box.swapKiB = 100 * 1024
	if resp, _ = h.do(undoReq(tx, ""), Root); resp.GetUndo() == nil {
		t.Fatalf("undo with room to spare: %v", resp)
	}
	if h.state(tx) != helperv1.State_STATE_UNDONE {
		t.Errorf("state %s", h.state(tx))
	}
}

func TestSwapPreconditions(t *testing.T) {
	cases := []struct {
		name  string
		setup func(b *fakeBox)
		want  string
	}{
		{"swap already on", func(b *fakeBox) { b.swaps = []string{"/dev/sdb2"} }, "has swap already"},
		{"a container", func(b *fakeBox) { b.virt = "lxc" }, "lxc container"},
		{"btrfs without mkswapfile", func(b *fakeBox) { b.fstype = "btrfs" }, "needs btrfs filesystem mkswapfile from btrfs-progs 6.1"},
		{"zfs", func(b *fakeBox) { b.fstype = "zfs" }, "only on ext4, XFS and Btrfs"},
		{"small disk", func(b *fakeBox) { b.free = 3 * gib }, "needs 3.1 GiB free"},
		{"fstab swap that is off", func(b *fakeBox) { b.files["/etc/fstab"] = "UUID=abc / ext4 defaults 0 1\n/swap.img none swap sw 0 0\n" }, "/etc/fstab lists"},
		// Fedora's swap on zram, turned off by hand: its unit stays.
		{"a swap unit of the box's own", func(b *fakeBox) { b.swapUnits = []string{"dev-zram0.swap"} }, "systemd has swap units of its own (dev-zram0.swap)"},
		{"no zswap in the kernel", func(b *fakeBox) { delete(b.files, path.Join(ZswapDir, "enabled")) }, "no zswap enabled setting"},
		{"no shrinker before 6.8", func(b *fakeBox) { delete(b.files, path.Join(ZswapDir, "shrinker_enabled")) }, "no zswap shrinker_enabled"},
		{"a file in the way", func(b *fakeBox) { b.files[SwapFile] = "someone's" }, "already exists"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness(t)
			tc.setup(h.box)
			resp, _ := h.do(applyReq(swapFix(true), false), Root)
			wantError(t, resp, helperv1.ErrorCode_ERROR_CODE_PRECONDITION, tc.want)
			if got := h.states(h.e.views()[0].id); !slices.Equal(got, []string{"approved", "preflight", "failed"}) {
				t.Errorf("journal %v", got)
			}
			if len(h.box.snaps) != 0 || slices.Contains(h.box.swaps, SwapFile) {
				t.Error("a refused fix changed something")
			}
		})
	}
	t.Run("fstab comment", func(t *testing.T) {
		h := newHarness(t)
		h.box.files["/etc/fstab"] = "# /swap.img none swap sw 0 0\n"
		h.mustApply(swapFix(true))
	})
	t.Run("zswap on already", func(t *testing.T) {
		h := newHarness(t)
		h.box.files[path.Join(ZswapDir, "enabled")] = "Y\n"
		a := h.mustApply(swapFix(true))
		if !slices.ContainsFunc(a.GetNotes(), func(n string) bool { return strings.Contains(n, "keeps its settings") }) || h.box.files[TmpFiles] != "" {
			t.Errorf("notes %q, tmpfiles %q", a.GetNotes(), h.box.files[TmpFiles])
		}
	})
}

func TestServiceLimitsVerifyAndAutoUndo(t *testing.T) {
	h := newHarness(t)
	h.box.unit("app.service", "loaded", "active", nil)
	a := h.mustApply(serviceFix("app.service", 448*mib, 576*mib))
	tx := a.GetTransaction()
	cg := path.Join(CgroupFS, "system.slice/app.service")
	if h.box.files[cg+"/memory.max"] != "603979776\n" || h.box.files[cg+"/memory.high"] != "469762048\n" {
		t.Fatalf("cgroup after apply: max %q high %q", h.box.files[cg+"/memory.max"], h.box.files[cg+"/memory.high"])
	}
	list, _ := h.do(&helperv1.Request{Kind: &helperv1.Request_List{List: &helperv1.ListRequest{}}}, Watcher)
	got := list.GetList().GetTransactions()
	if len(got) != 1 || got[0].GetState() != helperv1.State_STATE_VERIFYING || !got[0].GetAutoUndo() || !slices.Equal(got[0].GetCgroups(), []string{"system.slice/app.service"}) {
		t.Fatalf("list: %v", got)
	}
	if got[0].GetFix().GetServiceLimits().GetLimits()[0].GetMaxBytes() != 576*mib {
		t.Error("the listed fix lost its parameters")
	}

	resp, _ := h.do(undoReq(tx, "app.service restarted 2 times in verification"), Watcher)
	if resp.GetUndo() == nil {
		t.Fatalf("automatic undo: %v", resp)
	}
	if h.box.files[cg+"/memory.max"] != "max\n" || h.box.files[path.Join(UnitDir, "app.service.d/50-snugbox-memory.conf")] != "" {
		t.Error("the limit is still in place after the undo")
	}
	en := h.e.jr.Entries()
	if last := en[len(en)-2]; last.State != "undoing" || last.By != "snugboxd" || !strings.Contains(last.Note, "restarted 2 times") {
		t.Errorf("undo line: %+v", last)
	}
}

func TestWhoMayDoWhat(t *testing.T) {
	h := newHarness(t)
	resp, _ := h.do(applyReq(swapFix(false), false), Watcher)
	wantError(t, resp, helperv1.ErrorCode_ERROR_CODE_DENIED, "only root applies fixes")

	tx := h.mustApply(swapFix(false)).GetTransaction()
	resp, _ = h.do(undoReq(tx, "it failed"), Watcher)
	wantError(t, resp, helperv1.ErrorCode_ERROR_CODE_DENIED, "only a limit fix")
	resp, _ = h.do(undoReq("", "it failed"), Watcher)
	wantError(t, resp, helperv1.ErrorCode_ERROR_CODE_DENIED, "must name the transaction")

	h.box.now = h.box.now.Add(31 * time.Minute)
	h.box.unit("app.service", "loaded", "active", nil)
	if resp, _ := h.do(advanceReq(tx, helperv1.State_STATE_WATCHING, ""), Watcher); resp.GetAdvance() == nil {
		t.Fatalf("advance: %v", resp)
	}
	svc := h.mustApply(serviceFix("app.service", 448*mib, 576*mib)).GetTransaction()
	resp, _ = h.do(undoReq(svc, ""), Watcher)
	wantError(t, resp, helperv1.ErrorCode_ERROR_CODE_DENIED, "needs a reason")
	h.box.paused.Store(true)
	resp, _ = h.do(undoReq(svc, "killed"), Watcher)
	wantError(t, resp, helperv1.ErrorCode_ERROR_CODE_PAUSED, "pause")
	// A person can always put a change back, paused or not.
	if resp, _ := h.do(undoReq(svc, ""), Root); resp.GetUndo() == nil {
		t.Errorf("root undo while paused: %v", resp)
	}
}

// Automatic undo of a limit fix still runs under pressure and right after a
// kill, since putting a limit back only gives memory back.
func TestAutoUndoOfALimitIgnoresPressure(t *testing.T) {
	h := newHarness(t)
	h.box.unit("app.service", "loaded", "active", nil)
	tx := h.mustApply(serviceFix("app.service", 448*mib, 576*mib)).GetTransaction()
	h.box.files["/proc/pressure/memory"] = "some avg10=40.00 avg60=30.00 avg300=9.00 total=900\nfull avg10=20.00 avg60=10.00 avg300=1.00 total=90\n"
	h.box.kills = []time.Time{h.box.now.Add(-time.Minute)}
	if resp, _ := h.do(undoReq(tx, "killed by its limit"), Watcher); resp.GetUndo() == nil {
		t.Fatalf("automatic undo under pressure: %v", resp)
	}
}

func TestFailedApplyIsPutBack(t *testing.T) {
	h := newHarness(t)
	h.box.unit("app.service", "loaded", "active", nil)
	h.box.noApply = true
	resp, _ := h.do(applyReq(serviceFix("app.service", 448*mib, 576*mib), false), Root)
	wantError(t, resp, helperv1.ErrorCode_ERROR_CODE_FAILED, "memory.max of app.service reads max")
	tx := h.e.views()[0].id
	if got, want := h.states(tx), []string{"approved", "preflight", "snapshot", "applying", "undoing", "undone"}; !slices.Equal(got, want) {
		t.Errorf("journal %v, want %v", got, want)
	}
	if _, ok := h.box.files[path.Join(UnitDir, "app.service.d/50-snugbox-memory.conf")]; ok {
		t.Error("the drop-in is still there")
	}
}

func TestRecoverAfterACrash(t *testing.T) {
	h := newHarness(t)
	h.box.unit("app.service", "loaded", "active", nil)
	drop := path.Join(UnitDir, "app.service.d/50-snugbox-memory.conf")
	// The box loses power right after the drop-in is written: the reload
	// never runs and the journal's last line is "applying".
	h.box.onCall = func(call string) {
		if call == "systemctl daemon-reload" && h.box.files[drop] != "" {
			panic("power cut")
		}
	}
	func() {
		defer func() {
			if recover() == nil {
				t.Fatal("the simulated crash did not happen")
			}
		}()
		h.do(applyReq(serviceFix("app.service", 448*mib, 576*mib), false), Root)
	}()
	h.box.onCall = nil
	tx := h.e.views()[0].id
	if h.state(tx) != helperv1.State_STATE_APPLYING || h.box.files[drop] == "" {
		t.Fatalf("before recovery: state %s, drop-in %q", h.state(tx), h.box.files[drop])
	}

	h.reopen()
	if err := h.e.Recover(t.Context()); err != nil {
		t.Fatal(err)
	}
	if _, ok := h.box.files[drop]; ok {
		t.Error("recovery left the drop-in")
	}
	en := h.e.jr.Entries()
	last := en[len(en)-1]
	if last.Tx != tx || last.State != "undone" || last.By != "helper" || !strings.Contains(last.Note, "stopped in the middle") {
		t.Errorf("last line: %+v", last)
	}
	if !slices.Contains(h.box.calls, "systemctl daemon-reload") {
		t.Error("recovery did not reload systemd after removing the drop-in")
	}
}

func TestRecoverBeforeAnySnapshot(t *testing.T) {
	h := newHarness(t)
	if _, err := h.e.jr.Append("20261031-095959-swap", "preflight", "helper", "", nil); err != nil {
		t.Fatal(err)
	}
	h.reopen()
	if err := h.e.Recover(t.Context()); err != nil {
		t.Fatal(err)
	}
	if v := h.e.find("20261031-095959-swap"); v.state != helperv1.State_STATE_UNDONE || !strings.Contains(v.reason, "before changing anything") {
		t.Errorf("after recovery: %s %q", v.state, v.reason)
	}
}

func TestPauseStopsAFixMidway(t *testing.T) {
	h := newHarness(t)
	h.box.onCall = func(call string) {
		if strings.HasPrefix(call, "mkswap") {
			h.box.paused.Store(true)
		}
	}
	resp, _ := h.do(applyReq(swapFix(false), false), Root)
	wantError(t, resp, helperv1.ErrorCode_ERROR_CODE_FAILED, "pause came into force")
	if _, ok := h.box.files[SwapFile]; ok || slices.Contains(h.box.swaps, SwapFile) {
		t.Error("the swap file outlived the pause")
	}
	resp, _ = h.do(applyReq(swapFix(false), false), Root)
	wantError(t, resp, helperv1.ErrorCode_ERROR_CODE_PAUSED, "resume")
}

// Blueprint section 16.2: snugbox pause stops everything within 5 seconds,
// a step that is running included. The restart here would take 20 seconds.
func TestPauseStopsARunningStep(t *testing.T) {
	h := newHarness(t)
	h.box.unit("app.service", "loaded", "active", nil)
	h.box.block = "systemctl restart -- app.service"
	var paused time.Time
	h.box.onCall = func(call string) {
		if call == "systemctl restart -- app.service" && paused.IsZero() {
			paused = time.Now()
			h.box.paused.Store(true)
		}
	}
	var undoing time.Duration
	resp := h.e.Handle(t.Context(), applyReq(heapFix("app.service", helperv1.Runtime_RUNTIME_GLIBC, 0), false), Root, func(p *helperv1.Progress) {
		if p.GetState() == helperv1.State_STATE_UNDOING {
			undoing = time.Since(paused)
		}
	})
	wantError(t, resp, helperv1.ErrorCode_ERROR_CODE_FAILED, "pause came into force in the middle of the change, while restarting app.service")
	if undoing <= 0 || undoing > 5*time.Second {
		t.Errorf("the fix was being put back %v after the pause; blueprint section 16.2 allows 5 seconds", undoing)
	}
	if _, ok := h.box.files[path.Join(UnitDir, "app.service.d/50-snugbox-heap.conf")]; ok {
		t.Error("the heap cap outlived the pause")
	}
	tx := h.e.views()[0].id
	if got, want := h.states(tx), []string{"approved", "preflight", "snapshot", "applying", "undoing", "undone"}; !slices.Equal(got, want) {
		t.Errorf("journal %v, want %v", got, want)
	}
}

// A pause that comes during the last step stops the fix too, before it
// counts as applied.
func TestPauseDuringTheLastStep(t *testing.T) {
	h := newHarness(t)
	h.box.unit("app.service", "loaded", "active", nil)
	h.box.onCall = func(call string) {
		if call == "systemctl daemon-reload" {
			h.box.paused.Store(true)
		}
	}
	resp, _ := h.do(applyReq(serviceFix("app.service", 448*mib, 576*mib), false), Root)
	wantError(t, resp, helperv1.ErrorCode_ERROR_CODE_FAILED, "pause came into force")
	if got := h.box.files[path.Join(CgroupFS, "system.slice/app.service/memory.max")]; got != "max\n" {
		t.Errorf("memory.max reads %q after the pause", got)
	}
}

func TestGates(t *testing.T) {
	h := newHarness(t)
	h.box.files["/proc/pressure/memory"] = "some avg10=3.00 avg60=1.50 avg300=0.40 total=900\nfull avg10=0.00 avg60=0.00 avg300=0.00 total=90\n"
	resp, _ := h.do(applyReq(swapFix(false), false), Root)
	wantError(t, resp, helperv1.ErrorCode_ERROR_CODE_PRESSURE, "1.5% of the last minute")

	h.box.files["/proc/pressure/memory"] = "some avg10=0.00 avg60=0.20 avg300=0.40 total=900\nfull avg10=0.00 avg60=0.00 avg300=0.00 total=90\n"
	h.box.kills = []time.Time{h.box.now.Add(-4 * time.Minute)}
	resp, _ = h.do(applyReq(swapFix(false), false), Root)
	wantError(t, resp, helperv1.ErrorCode_ERROR_CODE_PRESSURE, "4m0s ago")

	h.box.kills = []time.Time{h.box.now.Add(-11 * time.Minute)}
	h.mustApply(swapFix(false))
}

// RHEL 9 and its rebuilds ship with pressure stall information off; no fix
// may run there until it is on, and the refusal says how.
func TestFixesNeedPressureInformation(t *testing.T) {
	h := newHarness(t)
	delete(h.box.files, "/proc/pressure/memory")
	resp, _ := h.do(applyReq(swapFix(false), false), Root)
	wantError(t, resp, helperv1.ErrorCode_ERROR_CODE_PRECONDITION, "boot with psi=1")
}

func TestOneFixVerifiedAtATime(t *testing.T) {
	h := newHarness(t)
	h.box.unit("app.service", "loaded", "active", nil)
	tx := h.mustApply(swapFix(false)).GetTransaction()
	h.box.now = h.box.now.Add(10 * time.Minute)
	resp, _ := h.do(applyReq(serviceFix("app.service", 448*mib, 576*mib), false), Root)
	wantError(t, resp, helperv1.ErrorCode_ERROR_CODE_BUSY, "about 20 more minutes")

	resp, _ = h.do(advanceReq(tx, helperv1.State_STATE_WATCHING, ""), Watcher)
	wantError(t, resp, helperv1.ErrorCode_ERROR_CODE_PRECONDITION, "10m0s of its 30 minutes")
	h.box.now = h.box.now.Add(21 * time.Minute)
	if resp, _ = h.do(advanceReq(tx, helperv1.State_STATE_WATCHING, ""), Watcher); resp.GetAdvance().GetTransaction().GetState() != helperv1.State_STATE_WATCHING {
		t.Fatalf("advance: %v", resp)
	}
	h.mustApply(serviceFix("app.service", 448*mib, 576*mib))

	resp, _ = h.do(advanceReq(tx, helperv1.State_STATE_DONE, ""), Watcher)
	wantError(t, resp, helperv1.ErrorCode_ERROR_CODE_PRECONDITION, "of its 7 days")
	resp, _ = h.do(advanceReq(tx, helperv1.State_STATE_REOPENED, "hit MemoryHigh 40 times"), Watcher)
	if resp.GetAdvance().GetTransaction().GetReason() != "hit MemoryHigh 40 times" {
		t.Fatalf("reopen: %v", resp)
	}
	resp, _ = h.do(advanceReq(tx, helperv1.State_STATE_DONE, ""), Watcher)
	wantError(t, resp, helperv1.ErrorCode_ERROR_CODE_PRECONDITION, "can't go from reopened to done")
	resp, _ = h.do(advanceReq(tx, helperv1.State_STATE_DONE, ""), Root)
	if got := resp.GetAdvance().GetTransaction(); got.GetState() != helperv1.State_STATE_DONE || !strings.Contains(got.GetReason(), "reviewed it and kept it") {
		t.Errorf("a person keeping a reopened fix: %v", resp)
	}
}

func dockerBox(h *harness, owner int, files string) *fakeDocker {
	d := &fakeDocker{cs: map[string]*docker.Container{}}
	c := &docker.Container{ID: cid, Name: "/shop-web-1"}
	c.State.Running = true
	c.Config.Labels = map[string]string{
		"com.docker.compose.project": "shop", "com.docker.compose.service": "web",
		"com.docker.compose.project.working_dir": "/home/ana/shop", "com.docker.compose.project.config_files": files,
	}
	d.cs[cid] = c
	h.box.docker = d
	h.box.owners["/home/ana/shop"] = [2]int{owner, owner}
	cg := path.Join(CgroupFS, "system.slice/docker-"+cid+".scope")
	h.box.files[cg+"/memory.max"] = "max\n"
	h.box.files[cg+"/memory.stat"] = "anon 314572800\nshmem 0\n"
	return d
}

func TestContainerLimitsWithOverride(t *testing.T) {
	h := newHarness(t)
	d := dockerBox(h, 1000, "/home/ana/shop/compose.yaml")
	a := h.mustApply(containerFix(640*mib, 300*mib, true))
	got := d.cs[cid].HostConfig
	if got.Memory != int64(640*mib) || got.MemoryReservation != int64(300*mib) || got.MemorySwap != -1 {
		t.Fatalf("after apply: %+v", got)
	}
	override := h.box.files["/home/ana/shop/compose.override.yaml"]
	want := "services:\n  web:\n    mem_limit: 640m\n    mem_reservation: 300m\n"
	if !strings.HasSuffix(override, want) || !strings.HasPrefix(override, "# Made by Snugbox for transaction "+a.GetTransaction()) {
		t.Errorf("override:\n%s", override)
	}
	run := h.box.calls[slices.IndexFunc(h.box.calls, func(c string) bool { return strings.HasPrefix(c, "systemd-run") })]
	for _, part := range []string{"--uid=1000", "--gid=1000", "ReadWritePaths=/home/ana/shop", "ProtectSystem=strict", "write-override /home/ana/shop compose.override.yaml"} {
		if !strings.Contains(run, part) {
			t.Errorf("systemd-run call lacks %q: %s", part, run)
		}
	}

	resp, _ := h.do(undoReq(a.GetTransaction(), ""), Root)
	if resp.GetUndo() == nil {
		t.Fatalf("undo: %v", resp)
	}
	got = d.cs[cid].HostConfig
	if got.Memory != math.MaxInt64 || got.MemoryReservation != 6<<20 {
		t.Errorf("after undo: %+v", got)
	}
	if !slices.ContainsFunc(resp.GetUndo().GetNotes(), func(n string) bool { return strings.Contains(n, "Docker can't remove a limit once set") }) {
		t.Errorf("notes %q", resp.GetUndo().GetNotes())
	}
	if _, ok := h.box.files["/home/ana/shop/compose.override.yaml"]; ok {
		t.Error("undo left the override file")
	}
}

func TestContainerOverrideIsOnlyShown(t *testing.T) {
	cases := []struct {
		name  string
		owner int
		files string
		setup func(h *harness)
		why   string
	}{
		{"root's folder", 0, "/home/ana/shop/compose.yaml", nil, "never as root"},
		{"own file list", 1000, "/home/ana/shop/compose.yaml,/home/ana/shop/prod.yaml", nil, "its own list of files"},
		{"a file from elsewhere", 1000, "/srv/other/compose.yaml", nil, "its own list of files"},
		{"an override exists", 1000, "/home/ana/shop/docker-compose.yml", func(h *harness) {
			h.box.files["/home/ana/shop/docker-compose.override.yml"] = "services: {}\n"
		}, "docker-compose.override.yml exists already"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness(t)
			d := dockerBox(h, tc.owner, tc.files)
			if tc.setup != nil {
				tc.setup(h)
			}
			a := h.mustApply(containerFix(640*mib, 0, true))
			if d.cs[cid].HostConfig.Memory != int64(640*mib) {
				t.Error("the live limit was not set")
			}
			if _, ok := h.box.files["/home/ana/shop/compose.override.yaml"]; ok {
				t.Error("an override was written")
			}
			manual := slices.IndexFunc(a.GetChanges(), func(c *helperv1.Change) bool { return c.GetManual() })
			if manual < 0 || !strings.Contains(a.GetChanges()[manual].GetNewValue(), "mem_limit: 640m") {
				t.Errorf("changes %v", a.GetChanges())
			}
			if !slices.ContainsFunc(a.GetNotes(), func(n string) bool { return strings.Contains(n, tc.why) }) {
				t.Errorf("notes %q, want one saying %q", a.GetNotes(), tc.why)
			}
		})
	}
}

func TestContainerPreconditions(t *testing.T) {
	h := newHarness(t)
	d := dockerBox(h, 1000, "/home/ana/shop/compose.yaml")
	resp, _ := h.do(applyReq(containerFix(320*mib, 0, false), false), Root)
	wantError(t, resp, helperv1.ErrorCode_ERROR_CODE_PRECONDITION, "holds 300 MiB it can't give back")
	d.cs[cid].HostConfig.Memory = int64(gib)
	resp, _ = h.do(applyReq(containerFix(640*mib, 0, false), false), Root)
	wantError(t, resp, helperv1.ErrorCode_ERROR_CODE_PRECONDITION, "has a limit already")
	// A limit an undo raised to docker.NoLimit is none, so the container
	// may be limited again.
	d.cs[cid].HostConfig.Memory = docker.NoLimit
	if resp, _ = h.do(applyReq(containerFix(640*mib, 0, false), true), Root); resp.GetError() != nil {
		t.Errorf("a container raised to no limit: %v", resp.GetError())
	}
	delete(d.cs, cid)
	resp, _ = h.do(applyReq(containerFix(640*mib, 0, false), false), Root)
	wantError(t, resp, helperv1.ErrorCode_ERROR_CODE_PRECONDITION, "is gone")
}

func TestHeapCaps(t *testing.T) {
	h := newHarness(t)
	h.box.unit("api.service", "loaded", "active", map[string]string{"MemoryMax": "536870912"})
	a := h.mustApply(heapFix("api.service", helperv1.Runtime_RUNTIME_NODE, 384*mib))
	drop := h.box.files[path.Join(UnitDir, "api.service.d/50-snugbox-heap.conf")]
	if !strings.HasSuffix(drop, "[Service]\nEnvironment=NODE_OPTIONS=--max-old-space-size=384\n") {
		t.Errorf("drop-in:\n%s", drop)
	}
	reload, restart := slices.Index(h.box.calls, "systemctl daemon-reload"), slices.Index(h.box.calls, "systemctl restart -- api.service")
	if reload < 0 || restart < reload {
		t.Errorf("calls %q: the restart must follow the reload", h.box.calls)
	}
	h.box.calls = nil
	if resp, _ := h.do(undoReq(a.GetTransaction(), ""), Root); resp.GetUndo() == nil {
		t.Fatalf("undo: %v", resp)
	}
	if !slices.Equal(h.box.calls[len(h.box.calls)-2:], []string{"systemctl daemon-reload", "systemctl restart -- api.service"}) {
		t.Errorf("undo calls %q", h.box.calls)
	}

	h.box.unit("web.service", "loaded", "active", map[string]string{"MemoryMax": "infinity"})
	resp, _ := h.do(applyReq(heapFix("web.service", helperv1.Runtime_RUNTIME_JAVA, 256*mib), false), Root)
	wantError(t, resp, helperv1.ErrorCode_ERROR_CODE_PRECONDITION, "limit the service first")
	// Under api.service's 512 MiB, Java may have three quarters and Go nine
	// tenths, in whole MiB.
	resp, _ = h.do(applyReq(heapFix("api.service", helperv1.Runtime_RUNTIME_JAVA, 448*mib), false), Root)
	wantError(t, resp, helperv1.ErrorCode_ERROR_CODE_PRECONDITION, "more than the 384 MiB Snugbox allows under the 512 MiB limit of api.service")
	resp, _ = h.do(applyReq(heapFix("api.service", helperv1.Runtime_RUNTIME_GO, 461*mib), false), Root)
	wantError(t, resp, helperv1.ErrorCode_ERROR_CODE_PRECONDITION, "more than the 460 MiB Snugbox allows")
	dry, _ := h.do(applyReq(heapFix("api.service", helperv1.Runtime_RUNTIME_GO, 460*mib), true), Root)
	if !slices.ContainsFunc(dry.GetApply().GetChanges(), func(c *helperv1.Change) bool { return strings.Contains(c.GetNewValue(), "GOMEMLIMIT=460MiB") }) {
		t.Errorf("a Go heap of nine tenths: %v", dry)
	}

	h.box.unit("worker.service", "loaded", "active", map[string]string{"EnvironmentFiles": "/etc/default/worker (ignore_errors=yes)"})
	h.box.files["/etc/default/worker"] = "# settings\nexport MALLOC_ARENA_MAX=4\n"
	resp, _ = h.do(applyReq(heapFix("worker.service", helperv1.Runtime_RUNTIME_GLIBC, 0), false), Root)
	wantError(t, resp, helperv1.ErrorCode_ERROR_CODE_PRECONDITION, "sets MALLOC_ARENA_MAX already in /etc/default/worker")
}

func TestOOMProtection(t *testing.T) {
	h := newHarness(t)
	a := h.mustApply(oomFix())
	for p, want := range map[string]string{
		path.Join(UnitDir, "system.slice.d/50-snugbox-oomd.conf"): "[Slice]\nManagedOOMMemoryPressure=kill\n",
		path.Join(UnitDir, "user.slice.d/50-snugbox-oomd.conf"):   "[Slice]\nManagedOOMMemoryPressure=kill\n",
		path.Join(UnitDir, "ssh.service.d/50-snugbox-oomd.conf"):  "[Service]\nManagedOOMPreference=omit\n",
	} {
		if !strings.HasSuffix(h.box.files[p], want) {
			t.Errorf("%s:\n%s", p, h.box.files[p])
		}
	}
	if h.box.units["systemd-oomd.service"]["ActiveState"] != "active" {
		t.Error("systemd-oomd is not running")
	}
	for _, f := range h.box.files {
		if strings.Contains(f, "ManagedOOMSwap") {
			t.Error("swap-based killing was turned on")
		}
	}
	if resp, _ := h.do(undoReq(a.GetTransaction(), ""), Root); resp.GetUndo() == nil {
		t.Fatalf("undo: %v", resp)
	}
	if p := h.box.units["systemd-oomd.service"]; p["ActiveState"] != "inactive" || p["UnitFileState"] != "disabled" {
		t.Errorf("systemd-oomd after undo: %v", p)
	}

	h2 := newHarness(t)
	h2.box.unit("earlyoom.service", "loaded", "active", nil)
	resp, _ := h2.do(applyReq(oomFix(), false), Root)
	wantError(t, resp, helperv1.ErrorCode_ERROR_CODE_PRECONDITION, "earlyoom runs already")

	// Fedora's systemd-oomd-defaults gives system.slice a policy; with the
	// daemon stopped, the fix still leaves that policy alone.
	h3 := newHarness(t)
	h3.box.units["system.slice"]["ManagedOOMMemoryPressure"] = "kill"
	resp, _ = h3.do(applyReq(oomFix(), false), Root)
	wantError(t, resp, helperv1.ErrorCode_ERROR_CODE_PRECONDITION, "system.slice has ManagedOOMMemoryPressure=kill set already")
	for p := range h3.box.files {
		if strings.Contains(p, "50-snugbox-oomd.conf") {
			t.Errorf("the refused fix wrote %s", p)
		}
	}
	if h3.box.units["systemd-oomd.service"]["ActiveState"] != "inactive" {
		t.Error("the refused fix started systemd-oomd")
	}
}

func earlyoomFix() *helperv1.Fix {
	return &helperv1.Fix{Kind: &helperv1.Fix_OomProtection{OomProtection: &helperv1.OOMProtectionFix{Daemon: helperv1.OOMDaemon_OOM_DAEMON_EARLYOOM}}}
}

const earlyoomExec = "{ path=/usr/bin/earlyoom ; argv[]=/usr/bin/earlyoom $EARLYOOM_ARGS ; ignore_errors=no ; start_time=[n/a] ; stop_time=[n/a] ; pid=0 ; code=(null) ; status=0/0 }"

func TestEarlyoomFallback(t *testing.T) {
	h := newHarness(t)
	resp, _ := h.do(applyReq(earlyoomFix(), false), Root)
	wantError(t, resp, helperv1.ErrorCode_ERROR_CODE_PRECONDITION, "earlyoom is not installed")

	h.box.unit("earlyoom.service", "loaded", "inactive", map[string]string{"UnitFileState": "disabled", "ExecStart": earlyoomExec})
	a := h.mustApply(earlyoomFix())
	dropPath := path.Join(UnitDir, "earlyoom.service.d/50-snugbox-earlyoom.conf")
	drop := h.box.files[dropPath]
	want := "[Service]\nExecStart=\nExecStart=/usr/bin/earlyoom -r 3600 --avoid " +
		"'^(sshd|systemd|systemd-.*|init|dbus-daemon|dbus-broker|snugboxd|snugbox-helper)$$'\n"
	if !strings.HasPrefix(drop, "# Made by Snugbox for transaction "+a.GetTransaction()) || !strings.HasSuffix(drop, want) {
		t.Errorf("drop-in:\n%s", drop)
	}
	if p := h.box.units["earlyoom.service"]; p["ActiveState"] != "active" || p["UnitFileState"] != "enabled" {
		t.Errorf("earlyoom after the fix: %v", p)
	}
	for p := range h.box.files {
		if strings.Contains(p, "50-snugbox-oomd.conf") {
			t.Errorf("a systemd-oomd drop-in came with the earlyoom fix: %s", p)
		}
	}
	if resp, _ := h.do(undoReq(a.GetTransaction(), ""), Root); resp.GetUndo() == nil {
		t.Fatalf("undo: %v", resp)
	}
	if _, ok := h.box.files[dropPath]; ok {
		t.Error("the drop-in is still there after the undo")
	}
	if p := h.box.units["earlyoom.service"]; p["ActiveState"] != "inactive" || p["UnitFileState"] != "disabled" {
		t.Errorf("earlyoom after undo: %v", p)
	}

	h2 := newHarness(t)
	h2.box.unit("systemd-oomd.service", "loaded", "active", nil)
	h2.box.unit("earlyoom.service", "loaded", "inactive", map[string]string{"UnitFileState": "disabled", "ExecStart": earlyoomExec})
	resp, _ = h2.do(applyReq(earlyoomFix(), false), Root)
	wantError(t, resp, helperv1.ErrorCode_ERROR_CODE_PRECONDITION, "systemd-oomd runs already")

	h3 := newHarness(t)
	h3.box.unit("earlyoom.service", "loaded", "inactive", map[string]string{"ExecStart": ""})
	resp, _ = h3.do(applyReq(earlyoomFix(), false), Root)
	wantError(t, resp, helperv1.ErrorCode_ERROR_CODE_PRECONDITION, "can't tell which program")
}

func TestSnapshotTamperingStopsUndo(t *testing.T) {
	h := newHarness(t)
	h.box.unit("app.service", "loaded", "active", nil)
	tx := h.mustApply(serviceFix("app.service", 448*mib, 576*mib)).GetTransaction()
	var p Plan
	if err := json.Unmarshal(h.box.snaps[tx], &p); err != nil {
		t.Fatal(err)
	}
	p.Steps[0].Target = "/etc/shadow"
	b, err := json.Marshal(p)
	if err != nil {
		t.Fatal(err)
	}
	h.box.snaps[tx] = b
	resp, _ := h.do(undoReq(tx, ""), Root)
	wantError(t, resp, helperv1.ErrorCode_ERROR_CODE_STUCK, "was changed after it was saved")
}

func TestUndoLeavesAFileChangedSince(t *testing.T) {
	h := newHarness(t)
	h.box.unit("app.service", "loaded", "active", nil)
	tx := h.mustApply(serviceFix("app.service", 448*mib, 576*mib)).GetTransaction()
	drop := path.Join(UnitDir, "app.service.d/50-snugbox-memory.conf")
	h.box.files[drop] = "[Service]\nMemoryMax=1G\n"
	resp, _ := h.do(undoReq(tx, ""), Root)
	if resp.GetUndo() == nil || !slices.ContainsFunc(resp.GetUndo().GetNotes(), func(n string) bool { return strings.Contains(n, "left as it is") }) {
		t.Fatalf("undo: %v", resp)
	}
	if h.box.files[drop] != "[Service]\nMemoryMax=1G\n" {
		t.Error("undo overwrote a person's edit")
	}
	if v := h.e.find(tx); !strings.HasPrefix(v.reason, "asked for with snugbox undo ") || !strings.Contains(v.reason, "left as it is") {
		t.Errorf("reason %q; it should say why, then what was left", v.reason)
	}
}

// Every path a fix writes lies under the unit's ReadWritePaths or the
// helper's own state folder.
func TestWritePathsCoverEveryStep(t *testing.T) {
	h := newHarness(t)
	h.box.unit("app.service", "loaded", "active", nil)
	h.box.unit("api.service", "loaded", "active", map[string]string{"MemoryMax": "536870912"})
	h.box.timer("backup.timer", "backup.service", "*-*-* 02:00:00")
	dockerBox(h, 1000, "/home/ana/shop/compose.yaml")
	fixes := []*helperv1.Fix{swapFix(true), oomFix(), serviceFix("app.service", 448*mib, 576*mib), heapFix("api.service", helperv1.Runtime_RUNTIME_JAVA, 384*mib), containerFix(640*mib, 0, true),
		scheduleFix(shift("backup.timer", "backup.service", 15))}
	for _, f := range fixes {
		plan, err := h.e.preflight(t.Context(), "20261031-100000-"+helperapi.KindOf(f), f)
		if err != nil {
			t.Fatalf("%s: %v", helperapi.KindOf(f), err)
		}
		if len(plan.Steps) == 0 {
			t.Fatalf("%s has no steps", plan.Kind)
		}
		for _, s := range plan.Steps {
			target := s.Target
			switch s.Op {
			case OpFile, OpKernel, OpSwapFile:
			case OpLimit:
				// The step removes the files set-property leaves.
				target = runtimeFile(s.Target, "MemoryMax")
			default:
				continue
			}
			under := strings.HasPrefix(target, StateDir+"/")
			for _, w := range WritePaths {
				under = under || strings.HasPrefix(target, w+"/")
			}
			if !under {
				t.Errorf("%s writes %s, outside the helper's ReadWritePaths", plan.Kind, target)
			}
		}
	}
}

// On Btrfs the swap file comes from btrfs filesystem mkswapfile, which also
// writes the swap header, so mkswap doesn't run; undo is the same.
func TestSwapOnBtrfs(t *testing.T) {
	h := newHarness(t)
	h.box.fstype, h.box.mkswapfile = "btrfs", true
	tx := h.mustApply(swapFix(false)).GetTransaction()
	var made []string
	for _, c := range h.box.calls {
		if strings.HasPrefix(c, "btrfs filesystem mkswapfile --size") || strings.HasPrefix(c, "mkswap") {
			made = append(made, c)
		}
	}
	if want := "btrfs filesystem mkswapfile --size 1024m -- " + SwapFile; len(made) != 1 || made[0] != want {
		t.Errorf("the swap file was made by %q, want only %q", made, want)
	}
	if !slices.Contains(h.box.swaps, SwapFile) {
		t.Error("the swap file is not in use")
	}
	if resp, _ := h.do(undoReq(tx, ""), Root); resp.GetUndo() == nil {
		t.Fatalf("undo: %v", resp)
	}
	if _, ok := h.box.files[SwapFile]; ok || slices.Contains(h.box.swaps, SwapFile) {
		t.Error("undo left the swap file")
	}
}
