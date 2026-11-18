package helper

import (
	"path"
	"slices"
	"strings"
	"testing"

	helperv1 "snugbox.dev/snugbox/gen/snugbox/helper/v1"
)

const appCgroup = CgroupFS + "/system.slice/app.service"

// cgroupLimits reads app.service's memory.high and memory.max.
func (h *harness) cgroupLimits() (high, maxB string) {
	return strings.TrimSpace(h.box.files[appCgroup+"/memory.high"]), strings.TrimSpace(h.box.files[appCgroup+"/memory.max"])
}

// runtimeLeft lists the files set-property left for app.service.
func (h *harness) runtimeLeft() []string {
	var left []string
	for _, m := range memorySettings {
		if _, ok := h.box.files[runtimeFile("app.service", m.name)]; ok {
			left = append(left, runtimeFile("app.service", m.name))
		}
	}
	return left
}

func (h *harness) calledSetProperty() []string {
	var calls []string
	for _, c := range h.box.calls {
		if strings.HasPrefix(c, "systemctl set-property") {
			calls = append(calls, c)
		}
	}
	return calls
}

// systemd 257 loads a drop-in's limits at the reload but leaves the running
// cgroup as it was, so the helper sets them with set-property --runtime,
// takes away the files that leaves, and does the same with the service's
// own limits on undo.
func TestServiceLimitsWhenTheReloadLeavesTheCgroup(t *testing.T) {
	h := newHarness(t)
	h.box.reloadKeepsCgroups = true
	h.box.unit("app.service", "loaded", "active", nil)
	tx := h.mustApply(serviceFix("app.service", 448*mib, 576*mib)).GetTransaction()
	if high, maxB := h.cgroupLimits(); high != "469762048" || maxB != "603979776" {
		t.Fatalf("cgroup after apply: high %s, max %s", high, maxB)
	}
	want := []string{"systemctl set-property --runtime -- app.service MemoryHigh=469762048 MemoryMax=603979776"}
	if got := h.calledSetProperty(); !slices.Equal(got, want) {
		t.Errorf("set-property calls %q, want %q", got, want)
	}
	if left := h.runtimeLeft(); len(left) > 0 {
		t.Errorf("apply left %q", left)
	}
	if h.exists(path.Join(ControlDir, "app.service.d")) {
		t.Error("apply left the runtime folder of app.service")
	}
	if i := slices.Index(h.box.calls, want[0]); i < 0 || !slices.Contains(h.box.calls[i:], "systemctl daemon-reload") {
		t.Error("systemd was not reloaded after the runtime files were removed")
	}

	h.box.calls = nil
	h.mustUndo(tx)
	if high, maxB := h.cgroupLimits(); high != "max" || maxB != "max" {
		t.Errorf("cgroup after undo: high %s, max %s", high, maxB)
	}
	want = []string{"systemctl set-property --runtime -- app.service MemoryHigh=infinity MemoryMax=infinity"}
	if got := h.calledSetProperty(); !slices.Equal(got, want) {
		t.Errorf("undo set-property calls %q, want %q", got, want)
	}
	if left := h.runtimeLeft(); len(left) > 0 {
		t.Errorf("undo left %q", left)
	}
	if !h.exists(ControlDir) {
		t.Error("the undo took away systemd's runtime settings folder")
	}
}

// Where the reload puts the limits on the cgroup, as up to systemd 255,
// nothing calls set-property at all.
func TestServiceLimitsWhenTheReloadSetsTheCgroup(t *testing.T) {
	h := newHarness(t)
	h.box.unit("app.service", "loaded", "active", nil)
	tx := h.mustApply(serviceFix("app.service", 448*mib, 576*mib)).GetTransaction()
	h.mustUndo(tx)
	if got := h.calledSetProperty(); len(got) > 0 {
		t.Errorf("set-property ran: %q", got)
	}
	if high, maxB := h.cgroupLimits(); high != "max" || maxB != "max" {
		t.Errorf("cgroup after undo: high %s, max %s", high, maxB)
	}
}

// The power goes after set-property and before its files are removed. The
// restarted helper takes them away with the rest of the fix.
func TestRecoverAfterACrashInTheLimitStep(t *testing.T) {
	h := newHarness(t)
	h.box.reloadKeepsCgroups = true
	h.box.unit("app.service", "loaded", "active", nil)
	h.box.onRemove = func(p string) {
		if strings.HasPrefix(p, ControlDir+"/") {
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
	h.box.onRemove = nil
	tx := h.e.views()[0].id
	if h.state(tx) != helperv1.State_STATE_APPLYING || len(h.runtimeLeft()) != 2 {
		t.Fatalf("before recovery: state %s, runtime files %q", h.state(tx), h.runtimeLeft())
	}

	h.reopen()
	if err := h.e.Recover(t.Context()); err != nil {
		t.Fatal(err)
	}
	if left := h.runtimeLeft(); len(left) > 0 {
		t.Errorf("recovery left %q", left)
	}
	if _, ok := h.box.files[path.Join(UnitDir, "app.service.d/50-snugbox-memory.conf")]; ok {
		t.Error("recovery left the drop-in")
	}
	if high, maxB := h.cgroupLimits(); high != "max" || maxB != "max" {
		t.Errorf("cgroup after recovery: high %s, max %s", high, maxB)
	}
	if got := h.state(tx); got != helperv1.State_STATE_UNDONE {
		t.Errorf("state after recovery %s", got)
	}
}

// A person who changes a limit at runtime while the fix is in place keeps
// that setting through the undo; the other one goes back to the service's
// own.
func TestUndoKeepsARuntimeSettingSomeoneMade(t *testing.T) {
	h := newHarness(t)
	h.box.reloadKeepsCgroups = true
	h.box.unit("app.service", "loaded", "active", nil)
	tx := h.mustApply(serviceFix("app.service", 448*mib, 576*mib)).GetTransaction()
	if _, err := h.box.Systemctl(t.Context(), "set-property", "--runtime", "--", "app.service", "MemoryMax=1073741824"); err != nil {
		t.Fatal(err)
	}
	resp, _ := h.do(undoReq(tx, ""), Root)
	if resp.GetUndo() == nil {
		t.Fatalf("undo: %v", resp)
	}
	if high, maxB := h.cgroupLimits(); high != "max" || maxB != "1073741824" {
		t.Errorf("cgroup after undo: high %s, max %s; want max and the person's 1073741824", high, maxB)
	}
	if got := h.runtimeLeft(); !slices.Equal(got, []string{runtimeFile("app.service", "MemoryMax")}) {
		t.Errorf("runtime files after undo %q, want only the person's MemoryMax", got)
	}
	if notes := strings.Join(resp.GetUndo().GetNotes(), " "); !strings.Contains(notes, "50-MemoryMax.conf was changed by someone else") {
		t.Errorf("the undo doesn't say it left the person's setting: %q", notes)
	}
}

// A service with a setting someone made with set-property --runtime is left
// alone, even when the setting is no limit.
func TestServiceLimitsRefuseARuntimeSetting(t *testing.T) {
	h := newHarness(t)
	h.box.unit("app.service", "loaded", "active", nil)
	if _, err := h.box.Systemctl(t.Context(), "set-property", "--runtime", "--", "app.service", "MemoryHigh=infinity"); err != nil {
		t.Fatal(err)
	}
	resp, _ := h.do(applyReq(serviceFix("app.service", 448*mib, 576*mib), false), Root)
	wantError(t, resp, helperv1.ErrorCode_ERROR_CODE_PRECONDITION, "app.service has MemoryHigh set at runtime with systemctl set-property")
}

// Without systemd's runtime folder the helper couldn't clear what
// set-property leaves, so it doesn't call it, and the fix is put back.
func TestServiceLimitsWithoutTheRuntimeFolder(t *testing.T) {
	h := newHarness(t)
	h.box.reloadKeepsCgroups = true
	h.box.unit("app.service", "loaded", "active", nil)
	delete(h.box.dirs, ControlDir)
	resp, _ := h.do(applyReq(serviceFix("app.service", 448*mib, 576*mib), false), Root)
	wantError(t, resp, helperv1.ErrorCode_ERROR_CODE_FAILED, "can't set the limits on the running app.service")
	if got := h.calledSetProperty(); len(got) > 0 {
		t.Errorf("set-property ran: %q", got)
	}
	if _, ok := h.box.files[path.Join(UnitDir, "app.service.d/50-snugbox-memory.conf")]; ok {
		t.Error("the drop-in outlived the rollback")
	}
}

// A snapshot saved before the limit step existed holds only the drop-in and
// the reload. Undone on systemd 257, for instance after an upgrade from an
// older one, the running service still gets its own limits back.
func TestRevertOfASnapshotWithoutTheLimitStep(t *testing.T) {
	h := newHarness(t)
	h.box.reloadKeepsCgroups = true
	h.box.unit("app.service", "loaded", "active", nil)
	drop := path.Join(UnitDir, "app.service.d/50-snugbox-memory.conf")
	s := Step{Op: OpFile, Target: drop, Dir: path.Dir(drop), Label: "app.service", Mode: 0o644, High: 448 * mib, Max: 576 * mib,
		New: header("20261031-100000-service-limits", "service-limits") + "[Service]\nMemoryHigh=469762048\nMemoryMax=603979776\n"}
	if err := s.do(t.Context(), h.box, ""); err != nil {
		t.Fatal(err)
	}
	// The older systemd put the drop-in's limits on the running cgroup.
	h.box.files[appCgroup+"/memory.high"], h.box.files[appCgroup+"/memory.max"] = "469762048\n", "603979776\n"
	plan := &Plan{Kind: "service-limits", AutoUndo: true, Steps: []Step{s, {Op: OpReload}}}
	notes, err := h.e.revert(t.Context(), plan, true)
	if err != nil {
		t.Fatalf("revert: %v (notes %q)", err, notes)
	}
	if high, maxB := h.cgroupLimits(); high != "max" || maxB != "max" {
		t.Errorf("cgroup after the revert: high %s, max %s", high, maxB)
	}
	if left := h.runtimeLeft(); len(left) > 0 {
		t.Errorf("the revert left %q", left)
	}
}
