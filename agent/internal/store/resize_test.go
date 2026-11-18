package store

import (
	"context"
	"testing"
	"time"

	"snugbox.dev/snugbox/agent/internal/model"
)

func TestResizes(t *testing.T) {
	ctx := context.Background()
	s := open(t)
	down := Resize{Time: t0, OldRAM: 4 << 30, NewRAM: 2 << 30, OldCPUs: 2, NewCPUs: 1, Provider: "digitalocean",
		OldPlan: "s-2vcpu-4gb", NewPlan: "s-1vcpu-2gb", OldCents: 2400, NewCents: 1200}
	up := Resize{Time: t0.Add(48 * time.Hour), OldRAM: 2 << 30, NewRAM: 3 << 30, OldCPUs: 1, NewCPUs: 1}
	for _, r := range []Resize{up, down} {
		if err := s.AddResize(ctx, r); err != nil {
			t.Fatal(err)
		}
	}
	got, err := s.Resizes(ctx, t0)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[0] != down || got[1] != up {
		t.Errorf("resizes %+v", got)
	}
	if got, _ := s.Resizes(ctx, t0.Add(time.Hour)); len(got) != 1 || got[0] != up {
		t.Errorf("resizes after the first %+v", got)
	}
	if err := s.Prune(ctx, t0.Add(EventRetention+time.Hour)); err != nil {
		t.Fatal(err)
	}
	if got, _ := s.Resizes(ctx, time.Time{}); len(got) != 1 || got[0] != up {
		t.Errorf("after pruning %+v", got)
	}
}

func TestSnapshotAt(t *testing.T) {
	ctx := context.Background()
	s := open(t)
	if _, ok, err := s.SnapshotAt(ctx, t0); ok || err != nil {
		t.Fatalf("an empty store has a snapshot: %v, %v", ok, err)
	}
	for i, snug := range []uint64{2048 * mib, 1536 * mib} {
		if _, err := s.AddSnapshot(ctx, t0.Add(time.Duration(i)*24*time.Hour), model.Estimate{Snug: snug, Confidence: model.ConfidenceMedium}); err != nil {
			t.Fatal(err)
		}
	}
	for _, c := range []struct {
		at   time.Time
		want uint64
	}{
		{t0.Add(-time.Hour), 2048 * mib},
		{t0, 2048 * mib},
		{t0.Add(23 * time.Hour), 2048 * mib},
		{t0.Add(25 * time.Hour), 1536 * mib},
	} {
		sn, ok, err := s.SnapshotAt(ctx, c.at)
		if err != nil || !ok || sn.Snug != c.want || sn.Confidence != "medium" {
			t.Errorf("at %s: %+v %v %v, want snug %d", c.at, sn, ok, err, c.want)
		}
	}
}
