package helper

import (
	"path"
	"slices"
	"testing"
	"time"

	helperv1 "snugbox.dev/snugbox/gen/snugbox/helper/v1"
)

func (h *harness) exists(dir string) bool {
	_, err := h.box.ListDir(dir)
	return err == nil
}

// verified moves a fix past its 30 minutes of verification, so the next one
// may run.
func (h *harness) verified(tx string) {
	h.t.Helper()
	h.box.now = h.box.now.Add(31 * time.Minute)
	if resp, _ := h.do(advanceReq(tx, helperv1.State_STATE_WATCHING, ""), Watcher); resp.GetAdvance() == nil {
		h.t.Fatalf("advance %s: %v", tx, resp)
	}
}

func (h *harness) mustUndo(tx string) {
	h.t.Helper()
	if resp, _ := h.do(undoReq(tx, ""), Root); resp.GetUndo() == nil {
		h.t.Fatalf("undo %s: %v", tx, resp)
	}
}

func TestUndoTakesAwayTheFoldersItMade(t *testing.T) {
	h := newHarness(t)
	dirs := []string{path.Join(UnitDir, "system.slice.d"), path.Join(UnitDir, "user.slice.d"), path.Join(UnitDir, "ssh.service.d")}
	tx := h.mustApply(oomFix()).GetTransaction()
	for _, d := range dirs {
		if !h.exists(d) {
			t.Fatalf("%s was not made", d)
		}
	}
	h.mustUndo(tx)
	for _, d := range dirs {
		if h.exists(d) {
			t.Errorf("%s is still there after the undo", d)
		}
	}
	if !h.exists(UnitDir) {
		t.Error("the undo took away the unit folder itself")
	}
}

// A folder a person made stays, with whatever they keep in it, and so does
// one they put a file in after the fix.
func TestUndoKeepsAFolderItDidNotMake(t *testing.T) {
	h := newHarness(t)
	theirs := path.Join(UnitDir, "ssh.service.d/10-mine.conf")
	h.box.files[theirs] = "[Service]\nNice=5\n"
	empty := path.Join(UnitDir, "system.slice.d")
	h.box.dirs[empty] = true
	tx := h.mustApply(oomFix()).GetTransaction()
	later := path.Join(UnitDir, "user.slice.d/20-later.conf")
	h.box.files[later] = "[Slice]\nCPUWeight=50\n"
	h.mustUndo(tx)
	for _, f := range []string{theirs, later} {
		if _, ok := h.box.files[f]; !ok {
			t.Errorf("%s went with the undo", f)
		}
	}
	if !h.exists(empty) {
		t.Errorf("the empty folder %s that was there before went with the undo", empty)
	}
	for _, f := range []string{"ssh.service.d", "system.slice.d", "user.slice.d"} {
		if _, ok := h.box.files[path.Join(UnitDir, f, "50-snugbox-oomd.conf")]; ok {
			t.Errorf("the drop-in in %s is still there", f)
		}
	}
}

// Two fixes can put drop-ins in one service's folder. Whichever is undone
// last takes the folder with it.
func TestSharedFolderGoesWithTheLastDropIn(t *testing.T) {
	limits := serviceFix("app.service", 448*mib, 576*mib)
	arenas := heapFix("app.service", helperv1.Runtime_RUNTIME_GLIBC, 0)
	dir := path.Join(UnitDir, "app.service.d")
	for _, c := range []struct {
		name             string
		first, second    *helperv1.Fix
		undoFirstApplied bool
	}{
		{"limits then arenas, limits undone first", limits, arenas, true},
		{"limits then arenas, arenas undone first", limits, arenas, false},
		{"arenas then limits, arenas undone first", arenas, limits, true},
		{"arenas then limits, limits undone first", arenas, limits, false},
	} {
		t.Run(c.name, func(t *testing.T) {
			h := newHarness(t)
			h.box.unit("app.service", "loaded", "active", nil)
			one := h.mustApply(c.first).GetTransaction()
			h.verified(one)
			two := h.mustApply(c.second).GetTransaction()
			order := []string{one, two}
			if !c.undoFirstApplied {
				slices.Reverse(order)
			}
			h.mustUndo(order[0])
			if !h.exists(dir) {
				t.Fatal("the folder went while the other fix's drop-in was still in it")
			}
			h.mustUndo(order[1])
			if h.exists(dir) {
				t.Errorf("%s is still there after both undos: %v", dir, h.box.within(dir))
			}
		})
	}
}

func TestRollBackTakesAwayTheFolder(t *testing.T) {
	h := newHarness(t)
	h.box.unit("app.service", "loaded", "active", nil)
	h.box.noApply = true
	resp, _ := h.do(applyReq(serviceFix("app.service", 448*mib, 576*mib), false), Root)
	wantError(t, resp, helperv1.ErrorCode_ERROR_CODE_FAILED, "put back")
	if d := path.Join(UnitDir, "app.service.d"); h.exists(d) {
		t.Errorf("%s outlived the rollback", d)
	}
}

// A snapshot saved before Snugbox kept track of folders has no Dir, and its
// undo leaves the folder as before.
func TestOldSnapshotKeepsTheFolder(t *testing.T) {
	b := newBox(t)
	drop := path.Join(UnitDir, "app.service.d/50-snugbox-memory.conf")
	s := Step{Op: OpFile, Target: drop, New: "[Service]\nMemoryMax=1G\n", Mode: 0o644}
	if err := s.do(t.Context(), b, ""); err != nil {
		t.Fatal(err)
	}
	if _, err := s.undo(t.Context(), b, ""); err != nil {
		t.Fatal(err)
	}
	if _, ok := b.files[drop]; ok {
		t.Error("the drop-in is still there")
	}
	if _, err := b.ListDir(path.Dir(drop)); err != nil {
		t.Errorf("the folder went with an old snapshot's undo: %v", err)
	}
}
