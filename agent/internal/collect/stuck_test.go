package collect

import (
	"context"
	"fmt"
	"testing"
	"time"
)

type staticContainers map[string]ContainerMeta

func (s staticContainers) Containers(context.Context, []string) (map[string]ContainerMeta, error) {
	return s, nil
}

// A container is in a crash loop when Docker restarted it at least three
// times and it is waiting to start again or started again within ten
// minutes, even when it has no cgroup at the moment.
func TestCrashLoops(t *testing.T) {
	now := time.Date(2026, 10, 31, 12, 0, 0, 0, time.UTC)
	id := func(c byte) string { return fmt.Sprintf("%064x", c) }
	src := staticContainers{
		cid:   {Name: "shop-app-1", Restarts: 2, Started: now.Add(-time.Minute)},
		id(1): {Name: "shop-worker-1", Restarts: 14, Started: now.Add(-2 * time.Minute)},
		id(2): {Name: "shop-cron-1", Restarts: 5, Started: now.Add(-3 * time.Hour), Restarting: true},
		id(3): {Name: "shop-old-1", Restarts: 9, Started: now.Add(-3 * time.Hour)},
		id(4): {Restarts: 3, Started: now.Add(-time.Minute)},
	}
	c, err := New(Options{FS: fixture(), PageSize: 4096, Containers: src})
	if err != nil {
		t.Fatal(err)
	}
	if err := c.Discover(context.Background()); err != nil {
		t.Fatal(err)
	}
	got := c.CrashLoops(now)
	want := []CrashLoop{
		{Name: id(4)[:12], Restarts: 3, Started: now.Add(-time.Minute)},
		{Name: "shop-cron-1", Restarts: 5, Started: now.Add(-3 * time.Hour), Restarting: true},
		{Name: "shop-worker-1", Restarts: 14, Started: now.Add(-2 * time.Minute)},
	}
	if len(got) != len(want) {
		t.Fatalf("crash loops %+v, want %+v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("crash loop %d = %+v, want %+v", i, got[i], want[i])
		}
	}
}

// A headless browser adopted by an init, in a workload where the program
// that should own it still runs, is left behind; its renderers count with
// it. A browser that its own program still owns, one started without
// --headless, and a workload that runs nothing but a browser are not.
func TestOrphanBrowsers(t *testing.T) {
	f := fixture()
	proc := func(pid, ppid int, name, cmdline string, pss uint64) {
		f[fmt.Sprintf("proc/%d/status", pid)] = file(fmt.Sprintf("Name:\t%s\nTgid:\t%d\nPPid:\t%d\nUid:\t0\t0\t0\t0\nVmRSS:\t%d kB\n", name, pid, ppid, pss))
		f[fmt.Sprintf("proc/%d/cmdline", pid)] = file(cmdline)
		f[fmt.Sprintf("proc/%d/smaps_rollup", pid)] = smapsPss(pss)
	}
	cgroupFiles(f, "sys/fs/cgroup/system.slice/scraper.service", "800\n801\n802\n803\n804\n805\n", 900<<20, 10<<20, 5<<20)
	proc(800, 1, "tini", "tini\x00--\x00node\x00scrape.js\x00", 1024)
	proc(801, 800, "node", "node\x00scrape.js\x00", 80<<10)
	proc(802, 800, "chrome", "/opt/chrome/chrome\x00--headless=new\x00--remote-debugging-port=0\x00", 120<<10)
	proc(803, 802, "chrome", "/opt/chrome/chrome\x00--type=renderer\x00", 200<<10)
	proc(804, 800, "chrome", "/opt/chrome/chrome\x00--kiosk\x00", 90<<10)
	proc(805, 801, "chrome", "/opt/chrome/chrome\x00--headless\x00", 100<<10)
	cgroupFiles(f, "sys/fs/cgroup/system.slice/browser.service", "900\n901\n", 300<<20, 10<<20, 5<<20)
	proc(900, 1, "dumb-init", "dumb-init\x00chromium\x00", 1024)
	proc(901, 900, "chromium", "chromium\x00--headless\x00", 150<<10)

	c, err := New(Options{FS: f, PageSize: 4096})
	if err != nil {
		t.Fatal(err)
	}
	if err := c.Discover(context.Background()); err != nil {
		t.Fatal(err)
	}
	ws := c.Workloads()
	s := ws["/system.slice/scraper.service"]
	if s.OrphanBrowsers != 1 || s.OrphanProcesses != 2 || s.OrphanBytes != 320<<20 || s.OrphanProgram != "chrome" {
		t.Errorf("scraper: %d browsers, %d processes, %d MiB, %q; want 1, 2 and 320 MiB of chrome",
			s.OrphanBrowsers, s.OrphanProcesses, s.OrphanBytes>>20, s.OrphanProgram)
	}
	if b := ws["/system.slice/browser.service"]; b.OrphanBrowsers != 0 {
		t.Errorf("a service that runs only a browser has %d orphans", b.OrphanBrowsers)
	}
}
