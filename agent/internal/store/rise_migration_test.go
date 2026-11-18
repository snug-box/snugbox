package store

import (
	"context"
	"testing"
	"time"

	"snugbox.dev/snugbox/agent/internal/model"
)

func TestUpgradeReplaysOnlyCompleteLegacyMinutes(t *testing.T) {
	ctx := context.Background()
	s := open(t)
	rec := s.NewRecorder(1, 4096)
	d := model.NewDeriver(4096)
	ws := map[string]model.Workload{"app": {ID: "app"}, "job": {ID: "job", Kind: model.KindService}}
	for i := range 24 {
		usage := map[string]model.Usage{"app": {Anon: mib, Current: mib}}
		if i >= 12 {
			usage["job"] = model.Usage{Anon: 8 * model.GiB, Current: 8 * model.GiB, Peak: 8 * model.GiB, HasPeak: true}
			usage["app"] = model.Usage{Anon: 21 * mib, Current: 21 * mib}
		}
		smp := model.Sample{Time: t0.Add(time.Duration(i) * 10 * time.Second), Workloads: usage}
		if err := rec.Add(ctx, smp, d.Add(smp), ws); err != nil {
			t.Fatal(err)
		}
	}
	if err := rec.Flush(ctx); err != nil {
		t.Fatal(err)
	}
	start := t0.Unix() + 120
	// The old recorder used subsecond timestamps. Its largest rise may
	// have been slightly lower than the replay from whole seconds.
	legacy := 8*model.GiB + 20*mib - 1
	for _, retainWorkload := range []bool{true, false} {
		if !retainWorkload {
			if _, err := s.db.Exec("DELETE FROM wl_10s WHERE t = ?", start); err != nil {
				t.Fatal(err)
			}
		}
		if _, err := s.db.Exec("UPDATE box_1m SET rise_max = ? WHERE t = ?", legacy, start); err != nil {
			t.Fatal(err)
		}
		if _, err := s.db.Exec("UPDATE box_1m SET rise_max = 987654 WHERE t < ?", start); err != nil {
			t.Fatal(err)
		}
		if _, err := s.db.Exec("PRAGMA user_version = 6"); err != nil {
			t.Fatal(err)
		}
		if err := s.migrate(ctx); err != nil {
			t.Fatal(err)
		}
		var got, earlier uint64
		if err := s.db.QueryRow("SELECT rise_max FROM box_1m WHERE t = ?", start).Scan(&got); err != nil {
			t.Fatal(err)
		}
		if err := s.db.QueryRow("SELECT MIN(rise_max) FROM box_1m WHERE t < ?", start).Scan(&earlier); err != nil {
			t.Fatal(err)
		}
		if (retainWorkload && got != 20*mib) || got < 20*mib || got > 200*mib || earlier != 987654 {
			t.Fatalf("retained workload %t: rise %d, earlier %d", retainWorkload, got, earlier)
		}
	}
	sum, _, err := s.Summary(ctx, t0, t0.Add(230*time.Second))
	if err != nil || !sum.Workloads["job"].PeakAtEnd {
		t.Fatalf("current peak after upgrade: %+v, %v", sum.Workloads["job"], err)
	}
}
