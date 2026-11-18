package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// Resize is a change of the box's RAM or CPU count the watcher saw: the
// sizes before and after, and the plans of the box's provider that match
// each in the price catalogue of the release that saw it, with their prices
// a month in cents. A plan is "" when the provider or a matching plan is
// unknown.
type Resize struct {
	Time               time.Time
	OldRAM, NewRAM     uint64
	OldCPUs, NewCPUs   int
	Provider           string
	OldPlan, NewPlan   string
	OldCents, NewCents int64
}

// AddResize records a resize.
func (s *Store) AddResize(ctx context.Context, r Resize) error {
	_, err := s.db.ExecContext(ctx, `INSERT OR REPLACE INTO resize (t, old_ram, new_ram, old_cpus, new_cpus, provider, old_plan, new_plan, old_cents, new_cents)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		r.Time.Unix(), r.OldRAM, r.NewRAM, r.OldCPUs, r.NewCPUs, r.Provider, r.OldPlan, r.NewPlan, r.OldCents, r.NewCents)
	if err != nil {
		return fmt.Errorf("store: add resize: %w", err)
	}
	return nil
}

// Resizes returns the resizes since a time, oldest first.
func (s *Store) Resizes(ctx context.Context, since time.Time) ([]Resize, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT t, old_ram, new_ram, old_cpus, new_cpus, provider, old_plan, new_plan, old_cents, new_cents
		FROM resize WHERE t >= ? ORDER BY t`, since.Unix())
	if err != nil {
		return nil, fmt.Errorf("store: resizes: %w", err)
	}
	defer rows.Close()
	var out []Resize
	for rows.Next() {
		var r Resize
		var t int64
		if err := rows.Scan(&t, &r.OldRAM, &r.NewRAM, &r.OldCPUs, &r.NewCPUs, &r.Provider, &r.OldPlan, &r.NewPlan, &r.OldCents, &r.NewCents); err != nil {
			return nil, fmt.Errorf("store: resizes: %w", err)
		}
		r.Time = time.Unix(t, 0).UTC()
		out = append(out, r)
	}
	return out, rows.Err()
}

// SnapshotAt returns the estimate that stood at a time: the last one stored
// at or before it, or, when watching began later, the first one after it.
// It reports false when there is none.
func (s *Store) SnapshotAt(ctx context.Context, at time.Time) (Snapshot, bool, error) {
	const cols = "SELECT t, need, headroom, cache, snug, confidence, window_s FROM snapshot "
	for _, q := range []string{cols + "WHERE t <= ? ORDER BY t DESC LIMIT 1", cols + "WHERE t > ? ORDER BY t LIMIT 1"} {
		var sn Snapshot
		var t, win int64
		err := s.db.QueryRowContext(ctx, q, at.Unix()).Scan(&t, &sn.Need, &sn.Headroom, &sn.Cache, &sn.Snug, &sn.Confidence, &win)
		if errors.Is(err, sql.ErrNoRows) {
			continue
		}
		if err != nil {
			return Snapshot{}, false, fmt.Errorf("store: snapshot at %s: %w", at.Format(time.RFC3339), err)
		}
		sn.Time, sn.Window = time.Unix(t, 0).UTC(), time.Duration(win)*time.Second
		return sn, true, nil
	}
	return Snapshot{}, false, nil
}
