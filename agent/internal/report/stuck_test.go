package report

import (
	"testing"
	"time"

	"snugbox.dev/snugbox/agent/internal/audit"
)

func TestStuckWords(t *testing.T) {
	r := &audit.Result{}
	for _, tc := range []struct {
		risk audit.Risk
		want string
	}{
		{audit.Risk{Kind: audit.RiskCrashLoop, Count: 1, Names: []string{"shop-worker-1"}, Restarts: []int{14}},
			"A container keeps restarting: shop-worker-1 (14 restarts)"},
		{audit.Risk{Kind: audit.RiskCrashLoop, Count: 2, Names: []string{"shop-worker-1", "shop-cron-1"}, Restarts: []int{14, 1}},
			"2 containers keep restarting: shop-worker-1 (14 restarts), shop-cron-1 (1 restart)"},
		{audit.Risk{Kind: audit.RiskOrphanBrowsers, Count: 1, Of: 3, Bytes: 300 << 20, Setting: "chrome", Names: []string{"shop-scraper-1"}},
			"A headless Chrome is still running after the program that started it stopped: 3 processes holding 300 MiB, in shop-scraper-1"},
		{audit.Risk{Kind: audit.RiskOrphanBrowsers, Count: 2, Of: 9, Bytes: 1536 << 20, Setting: "several", Names: []string{"a", "b"}},
			"2 headless browsers are still running after the programs that started them stopped: 9 processes holding 1.5 GiB, in a, b"},
	} {
		if got := RiskText(tc.risk, r); got != tc.want {
			t.Errorf("RiskText(%s)\n got %q\nwant %q", tc.risk.Kind, got, tc.want)
		}
	}

	r.Idle = []audit.Idle{
		{Name: "shop-old-1", CPU: 30 * time.Second, Observed: 9 * 24 * time.Hour, WorkingSet: 240 << 20},
		{Name: "shop-demo-1", CPU: 5 * time.Minute, Observed: 8 * 24 * time.Hour, WorkingSet: 60 << 20},
	}
	want := "Idle for a week or more: shop-old-1 (30 seconds of CPU in 9 days), shop-demo-1 (5 minutes of CPU in 8 days). Together they hold 300 MiB."
	if got := IdleLine(r); got != want {
		t.Errorf("IdleLine\n got %q\nwant %q", got, want)
	}
	if got := IdleLine(&audit.Result{}); got != "" {
		t.Errorf("IdleLine with nothing idle: %q", got)
	}
}
