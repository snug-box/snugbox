package store

import (
	"context"
	"database/sql"
	"math"
	"path/filepath"
	"testing"
	"time"

	"snugbox.dev/snugbox/agent/internal/model"
)

func TestPressureShare(t *testing.T) {
	cases := []struct {
		prev, cur, interval time.Duration
		want                float64
		ok                  bool
	}{
		{0, 100 * time.Millisecond, 10 * time.Second, 1, true},
		{time.Second, 4 * time.Second, 10 * time.Second, 30, true},
		{0, 20 * time.Second, 10 * time.Second, 100, true},
		{5 * time.Second, time.Second, 10 * time.Second, 0, false},
		{0, time.Second, 0, 0, false},
	}
	for _, tc := range cases {
		got, ok := PressureShare(tc.prev, tc.cur, tc.interval)
		if ok != tc.ok || math.Abs(got-tc.want) > 1e-9 {
			t.Errorf("PressureShare(%v, %v, %v) = %v, %v; want %v, %v", tc.prev, tc.cur, tc.interval, got, ok, tc.want, tc.ok)
		}
	}
}

// A workload that stalls 1% of most intervals and 30% of one interval in
// fifty has a 99th percentile of 30%; one that never stalls has 0.
func TestPressureQ99(t *testing.T) {
	ctx := context.Background()
	s := open(t)
	ws := map[string]model.Workload{
		"/system.slice/app.service":  {ID: "/system.slice/app.service", Kind: model.KindService, Name: "app.service"},
		"/system.slice/calm.service": {ID: "/system.slice/calm.service", Kind: model.KindService, Name: "calm.service"},
	}
	rec := s.NewRecorder(2, 4096)
	d := model.NewDeriver(4096)
	var stall time.Duration
	for i := range 1000 {
		step := 100 * time.Millisecond
		if i%50 == 25 {
			step = 3 * time.Second
		}
		stall += step
		smp := model.Sample{
			Time: t0.Add(time.Duration(i) * 10 * time.Second),
			Box:  model.BoxUsage{MemTotal: 4 << 30, MemAvailable: 2 << 30},
			Workloads: map[string]model.Usage{
				"/system.slice/app.service":  {Current: 300 * mib, Anon: 200 * mib, PressureSome: stall},
				"/system.slice/calm.service": {Current: 100 * mib, Anon: 50 * mib},
			},
		}
		if err := rec.Add(ctx, smp, d.Add(smp), ws); err != nil {
			t.Fatal(err)
		}
	}
	if err := rec.Flush(ctx); err != nil {
		t.Fatal(err)
	}
	got, err := s.PressureQ99(ctx, []string{"/system.slice/app.service", "/system.slice/calm.service", "/system.slice/gone.service"}, t0, t0.Add(3*time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if q := got["/system.slice/app.service"]; q < 30 || q > 30*(1+2*SketchAccuracy) {
		t.Errorf("app 99th percentile %.3f%%, want 30%% within the sketch's bound and never below", q)
	}
	if q, ok := got["/system.slice/calm.service"]; !ok || q != 0 {
		t.Errorf("calm service: %v, %v; want 0", q, ok)
	}
	if _, ok := got["/system.slice/gone.service"]; ok {
		t.Error("a workload the store never saw has a percentile")
	}
}

// A database written by the first release moves to the second schema on
// open, and its old hours, which have no pressure, are skipped.
func TestMigrateFromVersion1(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "snugbox.db")
	db, err := sql.Open("sqlite", "file:"+path)
	if err != nil {
		t.Fatal(err)
	}
	for _, q := range []string{migrations[0], "PRAGMA user_version = 1",
		`INSERT INTO workload (id, key, kind, name, info, first_seen, last_seen) VALUES (1, '/system.slice/app.service', 'service', 'app.service', '{}', 0, 0)`,
		`INSERT INTO wl_1h (wid, t, samples, ws, hot, ws_max, hard_max, cur_max, shmem_max, sock_max, pressured, oom_kills) VALUES (1, ?, 1, x'', x'', 0, 0, 0, 0, 0, 0, 0)`,
	} {
		if _, err := db.ExecContext(ctx, q, t0.Unix()); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	s, err := Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	var v int
	if err := s.db.QueryRowContext(ctx, "PRAGMA user_version").Scan(&v); err != nil || v != len(migrations) {
		t.Fatalf("schema version %d, %v; want %d", v, err, len(migrations))
	}
	got, err := s.PressureQ99(ctx, []string{"/system.slice/app.service"}, t0, t0.Add(time.Hour))
	if err != nil || len(got) != 0 {
		t.Errorf("an hour from before the migration: %v, %v", got, err)
	}
}
