package store

import (
	"context"
	"slices"
	"testing"
	"time"

	"snugbox.dev/snugbox/agent/internal/model"
)

const appID, backupID, shopID = "/system.slice/app.service", "/system.slice/backup.service", "/system.slice/docker-a.scope"

// runBox samples a box on which app.service runs all the time, and
// backup.service and a container only when a sample says so.
type runBox struct {
	rec   *Recorder
	d     *model.Deriver
	ws    map[string]model.Workload
	peaks map[time.Time]uint64
	n     int
}

func newRunBox(s *Store) *runBox {
	return &runBox{rec: s.NewRecorder(2, 4096), d: model.NewDeriver(4096), peaks: map[time.Time]uint64{}, ws: map[string]model.Workload{
		appID:    {ID: appID, Kind: model.KindService, Name: "app.service"},
		backupID: {ID: backupID, Kind: model.KindService, Name: "backup.service"},
		shopID:   {ID: shopID, Kind: model.KindContainer, Name: "shop-app-1"},
	}}
}

// every records a sample every 10 seconds from from to to, inclusive, with
// backup.service and the container in them when present.
func (b *runBox) every(t *testing.T, from, to time.Time, present bool) {
	t.Helper()
	for at := from; !at.After(to); at = at.Add(10 * time.Second) {
		b.n++
		use := map[string]model.Usage{appID: {Current: 300 * mib, Anon: 300 * mib}}
		if present {
			use[backupID] = model.Usage{Current: uint64(200+b.n%7) * mib, Anon: uint64(200+b.n%7) * mib}
			use[shopID] = model.Usage{Current: 100 * mib, Anon: 100 * mib}
		}
		s := model.Sample{Time: at, Box: model.BoxUsage{MemTotal: 8 << 30, MemAvailable: 6 << 30}, Workloads: use}
		p := b.d.Add(s)
		if present {
			b.peaks[at] = p.Workloads[backupID].WorkingSet
		}
		if err := b.rec.Add(context.Background(), s, p, b.ws); err != nil {
			t.Fatal(err)
		}
	}
}

// peak is backup.service's highest working set sampled from from to to.
func (b *runBox) peak(from, to time.Time) uint64 {
	var p uint64
	for at, ws := range b.peaks {
		if !at.Before(from) && !at.After(to) {
			p = max(p, ws)
		}
	}
	return p
}

func TestRuns(t *testing.T) {
	ctx := context.Background()
	s := open(t)
	b := newRunBox(s)
	at := func(minute, second int) time.Time {
		return t0.Add(time.Duration(minute)*time.Minute + time.Duration(second)*time.Second)
	}
	// backup.service runs from minute 5 to 15, missing from one sample in
	// the middle, as a slow sample can be; then from minute 20 to 22. The
	// watcher stops at minute 30 and comes back at minute 90 to find
	// backup.service running, from a start it didn't see.
	b.every(t, at(0, 0), at(4, 50), false)
	b.every(t, at(5, 0), at(9, 50), true)
	b.every(t, at(10, 0), at(10, 0), false)
	b.every(t, at(10, 10), at(15, 0), true)
	b.every(t, at(15, 10), at(19, 50), false)
	b.every(t, at(20, 0), at(22, 0), true)
	b.every(t, at(22, 10), at(30, 0), false)
	if err := b.rec.Flush(ctx); err != nil {
		t.Fatal(err)
	}
	b.rec = s.NewRecorder(2, 4096)
	b.every(t, at(90, 0), at(95, 0), true)
	if err := b.rec.Flush(ctx); err != nil {
		t.Fatal(err)
	}

	got, err := s.Runs(ctx, t0)
	if err != nil {
		t.Fatal(err)
	}
	// Only runs whose start was seen, and only services'.
	want := []model.Run{
		{Workload: backupID, Name: "backup.service", Start: at(5, 0), End: at(15, 0), Peak: b.peak(at(5, 0), at(15, 0))},
		{Workload: backupID, Name: "backup.service", Start: at(20, 0), End: at(22, 0), Peak: b.peak(at(20, 0), at(22, 0))},
	}
	if !slices.Equal(got, want) {
		t.Errorf("runs\n%+v\nwant\n%+v", got, want)
	}
	if got, _ := s.Runs(ctx, at(19, 0)); len(got) != 1 || !got[0].Start.Equal(at(20, 0)) {
		t.Errorf("runs since minute 19: %+v", got)
	}

	// A run still going is written at each flush, and grows.
	b.every(t, at(95, 10), at(100, 0), true)
	if err := b.rec.Flush(ctx); err != nil {
		t.Fatal(err)
	}
	var end int64
	var seen int
	if err := s.db.QueryRowContext(ctx, `SELECT r.end, r.start_seen FROM run r JOIN workload w ON w.id = r.wid WHERE w.key = ? AND r.start = ?`,
		backupID, at(90, 0).Unix()).Scan(&end, &seen); err != nil || end != at(100, 0).Unix() || seen != 0 {
		t.Errorf("the run found going: end %d, start seen %d, %v; want end %d, start not seen", end, seen, err, at(100, 0).Unix())
	}

	if err := s.Prune(ctx, at(15, 1).Add(HourRetention)); err != nil {
		t.Fatal(err)
	}
	if got, _ := s.Runs(ctx, t0); len(got) != 1 || !got[0].Start.Equal(at(20, 0)) {
		t.Errorf("after pruning the first run: %+v", got)
	}
}
