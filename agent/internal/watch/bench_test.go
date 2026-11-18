package watch

import (
	"context"
	"fmt"
	"testing"
	"time"

	"snugbox.dev/snugbox/agent/internal/testbox"
)

// BenchmarkSample takes one 10-second sample of a box with 50 services,
// the load blueprint section 9.4 budgets snugboxd's CPU for: under 0.5% of
// a core, so 50 ms a sample. The box's files are in memory here, so this
// leaves out reading them from the kernel.
func BenchmarkSample(b *testing.B) {
	box := testbox.New()
	for i := range 50 {
		box.Add(&testbox.Workload{Path: fmt.Sprintf("system.slice/load-%d.service", i), Programs: []string{"sleep"}, Anon: 2 * mib, ActiveFile: mib})
	}
	clk := &fakeClock{now: time.Date(2026, 10, 31, 9, 0, 0, 0, time.UTC)}
	d := newTestDaemon(b, box, clk)
	ctx := context.Background()
	for b.Loop() {
		if err := d.sampleOnce(ctx, clk.now); err != nil {
			b.Fatal(err)
		}
		clk.now = clk.now.Add(SampleEvery)
	}
}
