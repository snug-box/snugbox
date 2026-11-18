package watch

import (
	"context"
	"testing"
	"time"
)

// The watcher reads the crontabs and the minute history only when cron is
// installed, and passes both to the analysis.
func TestCronHistory(t *testing.T) {
	ctx := context.Background()
	for _, installed := range []bool{true, false} {
		b, _ := shop()
		b.Files = map[string]string{"etc/crontab": "0 2 * * * root /usr/local/bin/backup.sh\n"}
		if installed {
			b.Files["usr/sbin/cron"] = ""
		}
		clk := &fakeClock{now: time.Date(2026, 10, 31, 9, 0, 0, 0, time.UTC)}
		d := newTestDaemon(t, b, clk)
		run(t, d, clk, 3*time.Minute, nil)
		if err := d.rec.Flush(ctx); err != nil {
			t.Fatal(err)
		}
		jobs, minutes, err := d.cronHistory(ctx, clk.now)
		if err != nil {
			t.Fatal(err)
		}
		if installed && (len(jobs) != 1 || jobs[0].Program != "backup.sh" || len(minutes) < 2) {
			t.Errorf("with cron: %d jobs, %d minutes", len(jobs), len(minutes))
		}
		if !installed && (jobs != nil || minutes != nil) {
			t.Errorf("without cron: %d jobs, %d minutes", len(jobs), len(minutes))
		}
		if err := d.analyseNow(ctx); err != nil {
			t.Fatal(err)
		}
		if r := d.Result(); r == nil || installed && len(r.Cron) != 1 {
			t.Errorf("the analysis has no cron jobs: %+v", r)
		}
	}
}
