// Package store keeps the watcher's history in SQLite, at
// /var/lib/snugbox/snugbox.db, within the 64 MiB cap of blueprint section 5.3.
//
// The box's derived series are kept at 10 seconds for 48 hours and at one
// minute for 30 days, as the blueprint asks. Per-workload points are kept at
// 10 seconds for 6 hours and as hourly percentile sketches for 30 days,
// because 50 workloads at 10 seconds for 48 hours would not fit the cap.
// Hourly sketches are DDSketches with a 1% relative error bound, merged
// across hours for long windows.
package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/url"
	"time"

	_ "modernc.org/sqlite" // registers the "sqlite" driver: pure Go, so static builds need no C compiler

	"snugbox.dev/snugbox/agent/internal/model"
)

// Retention and size limits.
const (
	BoxFineRetention      = 48 * time.Hour
	BoxMinuteRetention    = 30 * 24 * time.Hour
	WorkloadFineRetention = 6 * time.Hour
	HourRetention         = 30 * 24 * time.Hour
	EventRetention        = 400 * 24 * time.Hour
	// MaxBytes is the store's cap. Above SoftBytes the oldest fine-grained
	// rows are dropped early to stay under it.
	MaxBytes  = 64 << 20
	SoftBytes = 56 << 20
)

// schemaVersion is stored in PRAGMA user_version. Each step in migrations
// moves the database from version i to i+1.
var migrations = []string{
	`CREATE TABLE meta (key TEXT PRIMARY KEY, value TEXT NOT NULL) WITHOUT ROWID;
	CREATE TABLE workload (
		id INTEGER PRIMARY KEY,
		key TEXT NOT NULL UNIQUE,
		kind TEXT NOT NULL,
		name TEXT NOT NULL,
		info TEXT NOT NULL,
		peak INTEGER,
		first_seen INTEGER NOT NULL,
		last_seen INTEGER NOT NULL
	);
	CREATE TABLE box_10s (
		t INTEGER PRIMARY KEY,
		hard INTEGER NOT NULL, total INTEGER NOT NULL, hot INTEGER NOT NULL, kother INTEGER NOT NULL,
		cur_sum INTEGER NOT NULL, mem_total INTEGER NOT NULL, mem_avail INTEGER NOT NULL,
		swap_used INTEGER NOT NULL, psi_some INTEGER NOT NULL, psi_full INTEGER NOT NULL
	);
	CREATE TABLE box_1m (
		t INTEGER PRIMARY KEY,
		samples INTEGER NOT NULL,
		hard_max INTEGER NOT NULL, total_max INTEGER NOT NULL, hot_max INTEGER NOT NULL,
		cur_sum_max INTEGER NOT NULL, peak_cur_max INTEGER NOT NULL, rise_max INTEGER NOT NULL,
		psi_full_us INTEGER NOT NULL, has_psi INTEGER NOT NULL,
		swapin_intervals INTEGER NOT NULL, intervals INTEGER NOT NULL, swapin_bytes INTEGER NOT NULL,
		oom_kills INTEGER NOT NULL
	);
	CREATE TABLE box_1h (
		t INTEGER PRIMARY KEY,
		total BLOB NOT NULL, hot BLOB NOT NULL, uncal BLOB NOT NULL, kother BLOB NOT NULL, cpu BLOB NOT NULL
	);
	CREATE TABLE wl_10s (
		wid INTEGER NOT NULL REFERENCES workload(id) ON DELETE CASCADE,
		t INTEGER NOT NULL,
		ws INTEGER NOT NULL, hard INTEGER NOT NULL, hot INTEGER NOT NULL, cur INTEGER NOT NULL,
		PRIMARY KEY (wid, t)
	) WITHOUT ROWID;
	CREATE TABLE wl_1h (
		wid INTEGER NOT NULL REFERENCES workload(id) ON DELETE CASCADE,
		t INTEGER NOT NULL,
		samples INTEGER NOT NULL,
		ws BLOB NOT NULL, hot BLOB NOT NULL,
		ws_max INTEGER NOT NULL, hard_max INTEGER NOT NULL, cur_max INTEGER NOT NULL,
		shmem_max INTEGER NOT NULL, sock_max INTEGER NOT NULL,
		pressured INTEGER NOT NULL, oom_kills INTEGER NOT NULL,
		PRIMARY KEY (wid, t)
	) WITHOUT ROWID;
	CREATE TABLE event (
		id INTEGER PRIMARY KEY,
		t INTEGER NOT NULL,
		kind TEXT NOT NULL,
		workload TEXT NOT NULL,
		detail TEXT NOT NULL
	);
	CREATE INDEX event_t ON event (t);
	CREATE TABLE snapshot (
		t INTEGER PRIMARY KEY,
		need INTEGER NOT NULL, headroom INTEGER NOT NULL, cache INTEGER NOT NULL, snug INTEGER NOT NULL,
		confidence TEXT NOT NULL, window_s INTEGER NOT NULL
	);`,
	// Version 2: each workload's memory pressure per 10-second interval, as
	// an hourly sketch, so a fix's verification can compare with the
	// workload's own 7-day 99th percentile.
	`ALTER TABLE wl_1h ADD COLUMN psi BLOB;`,
	// Version 3: the CPU time each workload used in the hour, in
	// microseconds, for the idle containers the report lists. NULL where
	// the cgroup gave none.
	`ALTER TABLE wl_1h ADD COLUMN cpu_us INTEGER;`,
	// Version 4: each run of a service, from when its cgroup appeared to
	// when it went, with its highest working set, so jobs that keep
	// starting in the same minute can be found. start_seen is 0 for a run
	// that was already going when the watcher started or came back, whose
	// real start is unknown.
	`CREATE TABLE run (
		wid INTEGER NOT NULL REFERENCES workload(id) ON DELETE CASCADE,
		start INTEGER NOT NULL,
		end INTEGER NOT NULL,
		peak INTEGER NOT NULL,
		start_seen INTEGER NOT NULL,
		PRIMARY KEY (wid, start)
	) WITHOUT ROWID;
	CREATE INDEX run_start ON run (start);`,
	// Version 5: each change of the box's RAM or CPU count the watcher saw,
	// with the plans that match the box before and after, so snugbox report
	// can count a saving only when a move to a smaller plan really
	// happened (blueprint section 7.5).
	`CREATE TABLE resize (
		t INTEGER PRIMARY KEY,
		old_ram INTEGER NOT NULL, new_ram INTEGER NOT NULL,
		old_cpus INTEGER NOT NULL, new_cpus INTEGER NOT NULL,
		provider TEXT NOT NULL,
		old_plan TEXT NOT NULL, new_plan TEXT NOT NULL,
		old_cents INTEGER NOT NULL, new_cents INTEGER NOT NULL
	) WITHOUT ROWID;`,
	// Version 6: the workload rows by time. Their keys start with the
	// workload, so a window of time, which the watcher reads every minute
	// and every 5 minutes, was read from every workload's whole history.
	`CREATE INDEX wl_1h_t ON wl_1h (t);
	CREATE INDEX wl_10s_t ON wl_10s (t);`,
	// Version 7 replays retained samples with correctLegacyRises in the
	// same transaction that advances the version.
	"",
}

// Store is the watcher's database. Its methods are safe for concurrent use;
// writes are serialised on one connection, which suits SQLite.
type Store struct {
	db *sql.DB
	// days keeps what Summary reads from whole days that are over.
	days dayCache
}

// Open opens or creates the database at path and brings its schema up to
// date. SQLite's page cache is held to 512 KiB: the C allocator under the
// pure-Go driver gives each 4 KiB page an 8 KiB slot, so SQLite's default
// of 2 MiB took about 4 MiB of the watcher's 24 MiB budget, and a 30-day
// summary on a full store takes 12 ms with either.
func Open(ctx context.Context, path string) (*Store, error) {
	q := url.Values{}
	for _, p := range []string{"journal_mode(WAL)", "synchronous(NORMAL)", "busy_timeout(5000)", "foreign_keys(ON)", "auto_vacuum(INCREMENTAL)", "cache_size(-512)"} {
		q.Add("_pragma", p)
	}
	db, err := sql.Open("sqlite", "file:"+path+"?"+q.Encode())
	if err != nil {
		return nil, fmt.Errorf("store: %w", err)
	}
	db.SetMaxOpenConns(1)
	s := &Store{db: db}
	if err := s.migrate(ctx); err != nil {
		return nil, errors.Join(err, db.Close())
	}
	return s, nil
}

// Close closes the database.
func (s *Store) Close() error { return s.db.Close() }

func (s *Store) migrate(ctx context.Context) error {
	var v int
	if err := s.db.QueryRowContext(ctx, "PRAGMA user_version").Scan(&v); err != nil {
		return fmt.Errorf("store: read schema version: %w", err)
	}
	if v > len(migrations) {
		return fmt.Errorf("store: the database has schema %d, newer than this release's %d; it was written by a newer Snugbox", v, len(migrations))
	}
	for ; v < len(migrations); v++ {
		tx, err := s.db.BeginTx(ctx, nil)
		if err != nil {
			return fmt.Errorf("store: %w", err)
		}
		if v+1 == 7 {
			err = correctLegacyRises(ctx, tx)
		} else {
			_, err = tx.ExecContext(ctx, migrations[v])
		}
		if err != nil {
			return errors.Join(fmt.Errorf("store: migrate to %d: %w", v+1, err), tx.Rollback())
		}
		if _, err := tx.ExecContext(ctx, fmt.Sprintf("PRAGMA user_version = %d", v+1)); err != nil {
			return errors.Join(fmt.Errorf("store: migrate to %d: %w", v+1, err), tx.Rollback())
		}
		if err := tx.Commit(); err != nil {
			return fmt.Errorf("store: migrate to %d: %w", v+1, err)
		}
	}
	return nil
}

// Size is the space the database's live pages take, in bytes: its pages
// less the free ones waiting for a vacuum.
func (s *Store) Size(ctx context.Context) (int64, error) {
	var pages, free, size int64
	for _, p := range []struct {
		q   string
		dst *int64
	}{{"PRAGMA page_count", &pages}, {"PRAGMA freelist_count", &free}, {"PRAGMA page_size", &size}} {
		if err := s.db.QueryRowContext(ctx, p.q).Scan(p.dst); err != nil {
			return 0, fmt.Errorf("store: %s: %w", p.q, err)
		}
	}
	return (pages - free) * size, nil
}

// trims is the order in which rows go early when the database nears its cap:
// fine-grained workload points first, the box's own long history last.
var trims = []struct {
	table string
	keep  time.Duration
}{
	{"wl_10s", time.Hour},
	{"box_10s", 12 * time.Hour},
	{"wl_1h", 14 * 24 * time.Hour},
	{"box_1m", 14 * 24 * time.Hour},
}

// Prune drops rows past their retention, then, while the database is over
// SoftBytes, older rows in the order of trims, and returns the freed pages to
// the filesystem.
func (s *Store) Prune(ctx context.Context, now time.Time) error {
	cut := func(d time.Duration) int64 { return now.Add(-d).Unix() }
	stmts := []struct {
		q   string
		arg int64
	}{
		{"DELETE FROM box_10s WHERE t < ?", cut(BoxFineRetention)},
		{"DELETE FROM box_1m WHERE t < ?", cut(BoxMinuteRetention)},
		{"DELETE FROM box_1h WHERE t < ?", cut(HourRetention)},
		{"DELETE FROM wl_10s WHERE t < ?", cut(WorkloadFineRetention)},
		{"DELETE FROM wl_1h WHERE t < ?", cut(HourRetention)},
		{"DELETE FROM event WHERE t < ?", cut(EventRetention)},
		{"DELETE FROM snapshot WHERE t < ?", cut(EventRetention)},
		{"DELETE FROM resize WHERE t < ?", cut(EventRetention)},
		{"DELETE FROM run WHERE end < ?", cut(HourRetention)},
		{"DELETE FROM workload WHERE last_seen < ? AND id NOT IN (SELECT wid FROM wl_1h)", cut(HourRetention)},
	}
	for _, st := range stmts {
		if _, err := s.db.ExecContext(ctx, st.q, st.arg); err != nil {
			return fmt.Errorf("store: prune: %w", err)
		}
	}
	for _, tr := range trims {
		size, err := s.Size(ctx)
		if err != nil {
			return err
		}
		if size <= SoftBytes {
			break
		}
		if _, err := s.db.ExecContext(ctx, "DELETE FROM "+tr.table+" WHERE t < ?", cut(tr.keep)); err != nil { //nolint:gosec // tr.table comes from the fixed trims list
			return fmt.Errorf("store: trim %s: %w", tr.table, err)
		}
		// A trim reaches into days Summary keeps; retention alone stops
		// before the 30-day window does.
		s.days.clear()
	}
	if _, err := s.db.ExecContext(ctx, "PRAGMA incremental_vacuum"); err != nil {
		return fmt.Errorf("store: vacuum: %w", err)
	}
	return nil
}

// Snapshot is one stored answer of the memory model.
type Snapshot struct {
	Time       time.Time
	Need       uint64
	Headroom   uint64
	Cache      uint64
	Snug       uint64
	Confidence string
	Window     time.Duration
}

// AddSnapshot stores an estimate when it differs from the last one stored,
// as blueprint section 7.1 asks, and reports whether it did.
func (s *Store) AddSnapshot(ctx context.Context, at time.Time, e model.Estimate) (bool, error) {
	var snug uint64
	var conf string
	err := s.db.QueryRowContext(ctx, "SELECT snug, confidence FROM snapshot ORDER BY t DESC LIMIT 1").Scan(&snug, &conf)
	switch {
	case errors.Is(err, sql.ErrNoRows):
	case err != nil:
		return false, fmt.Errorf("store: last snapshot: %w", err)
	case snug == e.Snug && conf == e.Confidence.String():
		return false, nil
	}
	_, err = s.db.ExecContext(ctx, "INSERT OR REPLACE INTO snapshot (t, need, headroom, cache, snug, confidence, window_s) VALUES (?, ?, ?, ?, ?, ?, ?)",
		at.Unix(), e.Need, e.Headroom, e.Cache, e.Snug, e.Confidence.String(), int64(e.Window.Seconds()))
	if err != nil {
		return false, fmt.Errorf("store: add snapshot: %w", err)
	}
	return true, nil
}

// Snapshots returns the stored estimates since a time, oldest first.
func (s *Store) Snapshots(ctx context.Context, since time.Time) ([]Snapshot, error) {
	rows, err := s.db.QueryContext(ctx, "SELECT t, need, headroom, cache, snug, confidence, window_s FROM snapshot WHERE t >= ? ORDER BY t", since.Unix())
	if err != nil {
		return nil, fmt.Errorf("store: snapshots: %w", err)
	}
	defer rows.Close()
	var out []Snapshot
	for rows.Next() {
		var sn Snapshot
		var t, win int64
		if err := rows.Scan(&t, &sn.Need, &sn.Headroom, &sn.Cache, &sn.Snug, &sn.Confidence, &win); err != nil {
			return nil, fmt.Errorf("store: snapshots: %w", err)
		}
		sn.Time, sn.Window = time.Unix(t, 0).UTC(), time.Duration(win)*time.Second
		out = append(out, sn)
	}
	return out, rows.Err()
}

// Event is something that happened on the box: a kill, a limit hit, a
// warning, a fix or an undo.
type Event struct {
	Time     time.Time
	Kind     string
	Workload string
	Detail   string
}

// AddEvent records an event.
func (s *Store) AddEvent(ctx context.Context, e Event) error {
	_, err := s.db.ExecContext(ctx, "INSERT INTO event (t, kind, workload, detail) VALUES (?, ?, ?, ?)", e.Time.Unix(), e.Kind, e.Workload, e.Detail)
	if err != nil {
		return fmt.Errorf("store: add event: %w", err)
	}
	return nil
}

// Events returns events since a time, newest first, at most limit.
func (s *Store) Events(ctx context.Context, since time.Time, limit int) ([]Event, error) {
	rows, err := s.db.QueryContext(ctx, "SELECT t, kind, workload, detail FROM event WHERE t >= ? ORDER BY t DESC, id DESC LIMIT ?", since.Unix(), limit)
	if err != nil {
		return nil, fmt.Errorf("store: events: %w", err)
	}
	defer rows.Close()
	var out []Event
	for rows.Next() {
		var e Event
		var t int64
		if err := rows.Scan(&t, &e.Kind, &e.Workload, &e.Detail); err != nil {
			return nil, fmt.Errorf("store: events: %w", err)
		}
		e.Time = time.Unix(t, 0).UTC()
		out = append(out, e)
	}
	return out, rows.Err()
}

// SetMeta stores a small piece of state, such as when watching began.
func (s *Store) SetMeta(ctx context.Context, key, value string) error {
	_, err := s.db.ExecContext(ctx, "INSERT INTO meta (key, value) VALUES (?, ?) ON CONFLICT (key) DO UPDATE SET value = excluded.value", key, value)
	if err != nil {
		return fmt.Errorf("store: set %s: %w", key, err)
	}
	return nil
}

// Meta reads a value stored with SetMeta, or "" when there is none.
func (s *Store) Meta(ctx context.Context, key string) (string, error) {
	var v string
	err := s.db.QueryRowContext(ctx, "SELECT value FROM meta WHERE key = ?", key).Scan(&v)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("store: get %s: %w", key, err)
	}
	return v, nil
}
