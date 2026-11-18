package store

import (
	"context"
	"os"
	"testing"
	"time"
)

// BenchmarkThirtyDaySummary measures the watcher's 30-day summary on a
// database that TestThirtyDaysOfFiftyWorkloads kept, named by
// SNUGBOX_SOAK_DB.
func BenchmarkThirtyDaySummary(b *testing.B) {
	path := os.Getenv("SNUGBOX_SOAK_DB")
	if path == "" {
		b.Skip("set SNUGBOX_SOAK_DB to a database TestThirtyDaysOfFiftyWorkloads kept")
	}
	ctx := context.Background()
	s, err := Open(ctx, path)
	if err != nil {
		b.Fatal(err)
	}
	b.Cleanup(func() { _ = s.Close() })
	var last int64
	if err := s.db.QueryRowContext(ctx, "SELECT MAX(t) FROM box_1m").Scan(&last); err != nil {
		b.Fatal(err)
	}
	end := time.Unix(last, 0).UTC()
	from := end.Add(-30 * 24 * time.Hour)
	b.ReportAllocs()
	for b.Loop() {
		if _, _, err := s.Summary(ctx, from, end); err != nil {
			b.Fatal(err)
		}
	}
}

// BenchmarkTwoHoursOfWorkloadPoints measures what the early warnings read
// from the same database when the watcher starts: two hours of every
// workload's points, one every 10 seconds.
func BenchmarkTwoHoursOfWorkloadPoints(b *testing.B) {
	path := os.Getenv("SNUGBOX_SOAK_DB")
	if path == "" {
		b.Skip("set SNUGBOX_SOAK_DB to a database TestThirtyDaysOfFiftyWorkloads kept")
	}
	ctx := context.Background()
	s, err := Open(ctx, path)
	if err != nil {
		b.Fatal(err)
	}
	b.Cleanup(func() { _ = s.Close() })
	var last int64
	if err := s.db.QueryRowContext(ctx, "SELECT MAX(t) FROM wl_10s").Scan(&last); err != nil {
		b.Fatal(err)
	}
	since := time.Unix(last, 0).UTC().Add(-2 * time.Hour)
	b.ReportAllocs()
	for b.Loop() {
		if _, err := s.WorkloadPoints(ctx, since); err != nil {
			b.Fatal(err)
		}
	}
}
