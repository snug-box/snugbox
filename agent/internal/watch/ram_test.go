package watch

import (
	"context"
	"strings"
	"testing"
	"time"

	"snugbox.dev/snugbox/agent/internal/testbox"
)

// A host that balloons the box's memory changes MemTotal while the watcher
// runs. The change is recorded and the next analysis uses the new size.
func TestRAMChangeIsRecordedAndReanalysed(t *testing.T) {
	b, _ := shop()
	clk := &fakeClock{now: time.Date(2026, 10, 31, 9, 0, 0, 0, time.UTC)}
	d := newTestDaemon(t, b, clk)
	before := d.Machine().MemTotal
	run(t, d, clk, time.Minute, nil)
	select {
	case <-d.analyse:
		t.Fatal("an analysis was asked for with no change of RAM")
	default:
	}

	b.Update(func(*testbox.Box) { b.MemTotal = 4 << 30 })
	run(t, d, clk, 20*time.Second, nil)
	if got := d.Machine().MemTotal; got != 4<<30 {
		t.Fatalf("MemTotal %d after the change, want %d", got, 4<<30)
	}
	select {
	case <-d.analyse:
	default:
		t.Error("no analysis was asked for after RAM changed")
	}
	evs, err := d.st.Events(context.Background(), time.Time{}, 10)
	if err != nil {
		t.Fatal(err)
	}
	var found []string
	for _, e := range evs {
		if e.Kind == "ram_changed" {
			found = append(found, e.Detail)
		}
	}
	want := "MemTotal went from 7957 MiB to 4096 MiB"
	if len(found) != 1 || !strings.HasPrefix(found[0], want) {
		t.Errorf("ram_changed events %q, want one starting %q (before: %d bytes)", found, want, before)
	}

	if err := d.analyseNow(context.Background()); err != nil {
		t.Fatal(err)
	}
	if r := d.Result(); r == nil || r.Machine.MemTotal != 4<<30 {
		t.Errorf("the analysis after the change still used the old RAM: %+v", r.Machine.MemTotal)
	}
}
