package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"snugbox.dev/snugbox/agent/internal/audit"
	"snugbox.dev/snugbox/agent/internal/collect"
	"snugbox.dev/snugbox/agent/internal/doctor"
	"snugbox.dev/snugbox/agent/internal/helperapi"
	"snugbox.dev/snugbox/agent/internal/localapi"
	"snugbox.dev/snugbox/agent/internal/model"
	"snugbox.dev/snugbox/agent/internal/provider"
	"snugbox.dev/snugbox/agent/internal/testbox"
)

const mib = model.MiB

// harness runs command lines into buffers as an ordinary user. A watcher or
// a helper answers only when a test starts one.
type harness struct {
	e              *env
	stdout, stderr bytes.Buffer
	sockets        string

	mu        sync.Mutex
	samples   []audit.Options
	result    *audit.Result
	sampleErr error
}

func newHarness(t *testing.T) *harness {
	t.Helper()
	// A socket path is limited to 107 bytes, so sockets get a short folder
	// of their own instead of t.TempDir.
	dir, err := os.MkdirTemp("", "sb")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	tmp := t.TempDir()
	h := &harness{sockets: dir}
	h.e = &env{
		stdout:    &h.stdout,
		stderr:    &h.stderr,
		stdin:     strings.NewReader(""),
		euid:      1000,
		watcher:   localapi.NewClient(filepath.Join(dir, "w.sock")),
		helper:    &helperapi.Client{Socket: filepath.Join(dir, "h.sock"), ServerUID: os.Getuid()},
		pauseFile: filepath.Join(tmp, "etc", "snugbox", "paused"),
		reportDir: filepath.Join(tmp, "reports"),
	}
	h.e.sample = func(_ context.Context, o audit.Options) (*audit.Result, error) {
		h.mu.Lock()
		defer h.mu.Unlock()
		h.samples = append(h.samples, o)
		return h.result, h.sampleErr
	}
	return h
}

// run executes one command line and returns its exit code, leaving its
// output in h.stdout and h.stderr until the next run.
func (h *harness) run(args ...string) int {
	h.stdout.Reset()
	h.stderr.Reset()
	return h.e.run(context.Background(), args)
}

// expect runs args and fails the test unless the command exits with code
// and every string in want appears in its output.
func (h *harness) expect(t *testing.T, code int, args []string, want ...string) {
	t.Helper()
	got := h.run(args...)
	out := h.stdout.String() + h.stderr.String()
	missing := []string{}
	for _, w := range want {
		if !strings.Contains(out, w) {
			missing = append(missing, w)
		}
	}
	if got != code || len(missing) > 0 {
		t.Errorf("snugbox %s: exit %d (want %d), missing %q\nstdout:\n%s\nstderr:\n%s",
			strings.Join(args, " "), got, code, missing, h.stdout.String(), h.stderr.String())
	}
}

// fakeWatcher answers the local API with fixed values and records what it
// was asked.
type fakeWatcher struct {
	mu       sync.Mutex
	status   localapi.Status
	result   *audit.Result
	changes  *localapi.Changes
	report   *localapi.Report
	requests []string
}

func (w *fakeWatcher) ServeHTTP(rw http.ResponseWriter, r *http.Request) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.requests = append(w.requests, r.URL.RequestURI())
	var v any
	switch r.URL.Path {
	case "/v1/status":
		v = w.status
	case "/v1/report":
		if w.report == nil {
			http.NotFound(rw, r)
			return
		}
		v = w.report
	case "/v1/result":
		if w.result == nil {
			writeJSON(rw, http.StatusServiceUnavailable, map[string]string{"error": "the first analysis runs a few minutes after the watcher starts; try again shortly"})
			return
		}
		v = w.result
	case "/v1/changes":
		if w.changes == nil {
			writeJSON(rw, http.StatusServiceUnavailable, map[string]string{"error": "snugboxd runs without snugbox-helper, so it verifies no fixes"})
			return
		}
		v = w.changes
	default:
		http.NotFound(rw, r)
		return
	}
	writeJSON(rw, http.StatusOK, v)
}

func (w *fakeWatcher) asked() []string {
	w.mu.Lock()
	defer w.mu.Unlock()
	return append([]string(nil), w.requests...)
}

func writeJSON(rw http.ResponseWriter, status int, v any) {
	rw.Header().Set("Content-Type", "application/json")
	rw.WriteHeader(status)
	_ = json.NewEncoder(rw).Encode(v)
}

// startWatcher serves w where the harness's watcher client looks for it.
func (h *harness) startWatcher(t *testing.T, w *fakeWatcher) *fakeWatcher {
	t.Helper()
	ln, err := net.Listen("unix", filepath.Join(h.sockets, "w.sock"))
	if err != nil {
		t.Skipf("unix sockets unavailable: %v", err)
	}
	srv := &http.Server{Handler: w, ReadHeaderTimeout: time.Second}
	go func() { _ = srv.Serve(ln) }()
	t.Cleanup(func() { _ = srv.Close() })
	return w
}

type fixedProvider provider.Info

func (p fixedProvider) Detect(context.Context) provider.Info { return provider.Info(p) }

type containerNames map[string]collect.ContainerMeta

func (n containerNames) Containers(context.Context, []string) (map[string]collect.ContainerMeta, error) {
	return n, nil
}

var (
	appID = strings.Repeat("a", 64)
	dbID  = strings.Repeat("b", 64)
)

// shopResult audits a made-up shop server for a minute: a Node container, a
// PostgreSQL container, a backup service and sshd on an 8 GiB droplet. It
// finds risks, and limits worked out from too little history to trust.
func shopResult(t *testing.T) *audit.Result {
	t.Helper()
	b := testbox.New()
	app := b.Add(&testbox.Workload{Path: "system.slice/docker-" + appID + ".scope", Programs: []string{"node"},
		Cmdline: "node\x00server.js\x00", Anon: 1100 * mib, ActiveFile: 100 * mib, InactiveFile: 300 * mib})
	b.Add(&testbox.Workload{Path: "system.slice/docker-" + dbID + ".scope", Programs: []string{"postgres"},
		Anon: 300 * mib, Shmem: 128 * mib, ActiveFile: 200 * mib, InactiveFile: 400 * mib})
	b.Add(&testbox.Workload{Path: "system.slice/backup.service", Programs: []string{"restic"}, Anon: 300 * mib})
	b.Add(&testbox.Workload{Path: "system.slice/ssh.service", Programs: []string{"sshd"}, Anon: 6 * mib})
	b.OOMD = true
	c, err := collect.New(collect.Options{FS: b, PageSize: 4096, Containers: containerNames{
		appID: {Name: "shop-app-1"},
		dbID:  {Name: "shop-db-1", Image: "postgres:18"},
	}})
	if err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 10, 31, 9, 0, 0, 0, time.UTC)
	var step uint64
	deps := audit.Deps{
		Collector: c,
		Prober:    fixedProvider{Provider: provider.DigitalOcean, Region: "fra1", FromMetadata: true},
		KernelLog: func(time.Time) (collect.KernelLog, error) {
			return collect.KernelLog{}, errors.New("no kernel log in tests")
		},
		Now: func() time.Time { return now },
		Sleep: func(_ context.Context, d time.Duration) error {
			now = now.Add(d)
			step++
			b.Update(func(*testbox.Box) {
				app.Anon = 1100*mib + step%5*20*mib
				b.CPUBusy += 200
				b.CPUIdle += 600
			})
			return nil
		},
		FS:   b,
		Root: true,
	}
	r, err := audit.Run(context.Background(), deps, audit.Options{Duration: time.Minute})
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func TestVersion(t *testing.T) {
	h := newHarness(t)
	for _, args := range [][]string{{"version"}, {"--version"}} {
		if code := h.run(args...); code != ExitOK || !strings.HasPrefix(h.stdout.String(), "snugbox ") || !strings.Contains(h.stdout.String(), "(commit ") {
			t.Errorf("snugbox %s: exit %d, %q", args[0], code, h.stdout.String())
		}
	}
}

func TestBareCommandsPrintHelp(t *testing.T) {
	h := newHarness(t)
	h.expect(t, ExitOK, nil, "Start with a read-only audit:", "sudo snugbox audit")
	h.expect(t, ExitOK, []string{"watch"}, "snugboxd samples the box every 10 seconds", "uninstall")
}

func TestUsageErrorsExitWith2(t *testing.T) {
	h := newHarness(t)
	for _, c := range []struct {
		args []string
		want []string
	}{
		{[]string{"audti"}, []string{`"audti" is not a command of snugbox; did you mean audit?`, "See 'snugbox --help'."}},
		{[]string{"watch", "instal"}, []string{"did you mean install?", "See 'snugbox watch --help'."}},
		{[]string{"--bogus"}, []string{"unknown flag: --bogus"}},
		{[]string{"audit", "--bogus"}, []string{"unknown flag: --bogus", "See 'snugbox audit --help'."}},
		{[]string{"audit", "extra"}, []string{"See 'snugbox audit --help'."}},
		{[]string{"audit", "--for", "soon"}, []string{`invalid argument "soon"`}},
		{[]string{"audit", "--json", "--markdown"}, []string{"choose one of --json and --markdown"}},
		{[]string{"watch", "install", "now"}, []string{"See 'snugbox watch install --help'."}},
		{[]string{"fit", "--list", "--status"}, []string{"--list, --status and --keep each stand alone"}},
		{[]string{"fit", "--status", "2"}, []string{"each stand alone"}},
		{[]string{"fit", "1", "2"}, []string{"accepts at most 1 arg"}},
		{[]string{"undo", "--last", "20261031-090000-swap"}, []string{"name a transaction or use --last, not both"}},
		{[]string{"version", "now"}, []string{"See 'snugbox version --help'."}},
	} {
		h.expect(t, ExitUsage, c.args, c.want...)
	}
}

func TestCommandsThatChangeTheBoxNeedRoot(t *testing.T) {
	h := newHarness(t)
	for _, args := range [][]string{
		{"pause"}, {"resume"}, {"undo"}, {"undo", "--last"}, {"fit", "--keep", "20261031-090000-swap"},
		{"watch", "install"}, {"watch", "uninstall"},
	} {
		h.expect(t, ExitNeedsRoot, args, "this command changes the system; run it with sudo")
	}
	if _, err := os.Stat(h.e.pauseFile); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("pause without root left %s: %v", h.e.pauseFile, err)
	}
}

func TestAuditUsesTheWatcher(t *testing.T) {
	h := newHarness(t)
	w := h.startWatcher(t, &fakeWatcher{result: shopResult(t)})

	// Risks were found, so the audit exits with 3 for monitoring scripts.
	h.expect(t, ExitRisks, []string{"audit", "--json"})
	var doc struct {
		Schema string `json:"schema"`
		Host   string `json:"host"`
	}
	if err := json.Unmarshal(h.stdout.Bytes(), &doc); err != nil || doc.Schema == "" || doc.Host != "web-1" {
		t.Errorf("--json wrote %v, %+v:\n%s", err, doc, h.stdout.String())
	}
	h.expect(t, ExitRisks, []string{"audit", "--markdown"}, "# Snugbox audit of web-1")
	if len(h.samples) != 0 {
		t.Errorf("the audit sampled %d times although the watcher answered", len(h.samples))
	}

	// --for asks for a fresh sample even while the watcher runs.
	h.result = shopResult(t)
	h.expect(t, ExitRisks, []string{"audit", "--for", "10m", "--offline", "--json"})
	if len(h.samples) != 1 || h.samples[0].Duration != 10*time.Minute || !h.samples[0].Offline {
		t.Errorf("samples %+v, want one of 10 minutes, offline", h.samples)
	}
	if got := w.asked(); len(got) != 2 {
		t.Errorf("the watcher was asked %q; --for must not ask it", got)
	}
}

func TestAuditSamplesWithoutTheWatcher(t *testing.T) {
	h := newHarness(t)
	h.result = shopResult(t)
	h.expect(t, ExitRisks, []string{"audit", "--json"})
	if h.stderr.Len() != 0 {
		t.Errorf("no watcher is not worth a message, got %q", h.stderr.String())
	}
	if len(h.samples) != 1 || h.samples[0].Duration != audit.DefaultDuration || h.samples[0].Offline {
		t.Errorf("samples %+v, want one default sample", h.samples)
	}
	if h.samples[0].Progress != nil {
		t.Error("a JSON audit printed a countdown")
	}
}

func TestAuditSaysWhyTheWatcherWasNotUsed(t *testing.T) {
	h := newHarness(t)
	h.startWatcher(t, &fakeWatcher{})
	h.result = shopResult(t)
	h.expect(t, ExitRisks, []string{"audit", "--json"},
		"the watcher's history couldn't be used (the first analysis runs a few minutes after the watcher starts; try again shortly); sampling instead.")
	if len(h.samples) != 1 {
		t.Errorf("%d samples, want 1", len(h.samples))
	}
}

func TestAuditStoppedByCtrlC(t *testing.T) {
	h := newHarness(t)
	h.sampleErr = context.Canceled
	h.expect(t, ExitError, []string{"audit"}, "stopped before the sample finished; nothing was changed")
	h.sampleErr = errors.New("cgroup v2 is not mounted")
	h.expect(t, ExitError, []string{"audit"}, "snugbox: cgroup v2 is not mounted")
}

func TestAuditTerminalReport(t *testing.T) {
	h := newHarness(t)
	h.result = shopResult(t)
	h.expect(t, ExitRisks, []string{"audit"}, "web-1")
	if strings.Contains(h.stdout.String(), "\x1b[") {
		t.Error("colour codes in output that is not a terminal")
	}
	saved, _ := filepath.Glob(filepath.Join(h.e.reportDir, "audit-*.md"))
	if saveFile(h.e.reportDir, "probe.md", nil) == nil {
		// This platform can save reports, so a root audit must have.
		if len(saved) != 1 || !strings.Contains(h.stdout.String(), saved[0]) {
			t.Errorf("saved %q; the report must name the file it saved", saved)
		}
		return
	}
	if !strings.Contains(h.stderr.String(), "the Markdown report was not saved") {
		t.Errorf("an unsaved report must say so, stderr %q", h.stderr.String())
	}
}

func TestShareCardHasNoNames(t *testing.T) {
	h := newHarness(t)
	t.Chdir(t.TempDir())
	h.result = shopResult(t)
	h.expect(t, ExitRisks, []string{"audit", "--json", "--share"}, "Wrote snugbox-card.svg and snugbox-card.txt with every workload name removed. Nothing was uploaded.")
	for _, name := range []string{"snugbox-card.svg", "snugbox-card.txt"} {
		b, err := os.ReadFile(name)
		if err != nil {
			t.Fatal(err)
		}
		for _, secret := range []string{"shop-app-1", "shop-db-1", "backup.service", "web-1", "server.js"} {
			if bytes.Contains(b, []byte(secret)) {
				t.Errorf("%s names %q", name, secret)
			}
		}
	}
}

func TestPlan(t *testing.T) {
	h := newHarness(t)
	h.result = shopResult(t)
	h.expect(t, ExitOK, []string{"plan", "--provider", "Hetzner", "--for", "2m"}, "Hetzner")
	if len(h.samples) != 1 || h.samples[0].Duration != 2*time.Minute {
		t.Errorf("samples %+v", h.samples)
	}
	if strings.Contains(h.stdout.String(), "DigitalOcean") {
		t.Errorf("--provider hetzner listed another provider:\n%s", h.stdout.String())
	}
}

func TestFitNeedsTheWatcher(t *testing.T) {
	h := newHarness(t)
	for _, args := range [][]string{{"fit"}, {"fit", "--dry-run", "1"}} {
		h.expect(t, ExitError, args, "snugbox fit needs the watcher, which checks a fix for 30 minutes and undoes a limit that hurts")
	}
}

func TestFitList(t *testing.T) {
	h := newHarness(t)
	h.startWatcher(t, &fakeWatcher{result: shopResult(t)})
	h.expect(t, ExitOK, []string{"fit", "--list"},
		"Fixes for web-1, from 60 seconds of watching:",
		"  1  Limits for shop-app-1", "snugbox fit container-limits",
		"  2  Limits for backup.service", "snugbox fit service-limits",
		"  3  Early out-of-memory protection, systemd-oomd", "snugbox fit oom-protection",
		"  4  Heap caps for shop-app-1, after a restart", "snugbox fit heap-caps",
		"Preview one with snugbox fit --dry-run <number>.")
	if h.run("fit") != ExitOK || !strings.Contains(h.stdout.String(), "  4  Heap caps") {
		t.Errorf("fit with no arguments must list too:\n%s", h.stdout.String())
	}
}

func TestFitListWhenNothingIsProposed(t *testing.T) {
	h := newHarness(t)
	r := shopResult(t)
	r.Fixes, r.Risks = nil, nil
	h.startWatcher(t, &fakeWatcher{result: r})
	h.expect(t, ExitOK, []string{"fit"}, "None needed. This box is snug.")
}

func TestFitPicksByNumberOrKind(t *testing.T) {
	h := newHarness(t)
	h.startWatcher(t, &fakeWatcher{result: shopResult(t)})
	h.expect(t, ExitUsage, []string{"fit", "9"}, "there is no fix 9; snugbox fit --list shows 4")
	h.expect(t, ExitUsage, []string{"fit", "0"}, "there is no fix 0")
	h.expect(t, ExitUsage, []string{"fit", "bogus"}, `"bogus" is not a fix`, "swap, oom-protection, container-limits, service-limits, heap-caps")
	h.expect(t, ExitUsage, []string{"fit", "swap"}, "the watcher proposes no swap fix for this box right now")
	h.expect(t, ExitUsage, []string{"fit", "service-limits", "--only", "nginx.service"}, "the fix covers none of [nginx.service]")
}

func TestFitRefusesYoungLimits(t *testing.T) {
	h := newHarness(t)
	h.startWatcher(t, &fakeWatcher{result: shopResult(t)})
	h.expect(t, ExitUsage, []string{"fit", "1"},
		"these limits come from 60 seconds of history, and Snugbox wants 7 days before it trusts a limit",
		"--accept-low-confidence")
	// Accepted, the command goes on to the root check.
	h.expect(t, ExitNeedsRoot, []string{"fit", "1", "--accept-low-confidence"}, "run it with sudo")
}

func TestFitDryRunWithoutRootDescribes(t *testing.T) {
	h := newHarness(t)
	h.startWatcher(t, &fakeWatcher{result: shopResult(t)})
	h.expect(t, ExitOK, []string{"fit", "--dry-run", "container-limits"},
		"Limits for shop-app-1 (", "Container shop-app-1: limit ", ", soft limit ",
		"Run it with sudo to see every file and old value the helper would change. Nothing was changed.")
	h.expect(t, ExitOK, []string{"fit", "--dry-run", "2"}, "backup.service: MemoryHigh ", ", MemoryMax ")
	h.expect(t, ExitOK, []string{"fit", "--dry-run", "oom-protection"}, "Turns on systemd-oomd", "Keeps sshd out of its reach")
}

func TestFitAdviceOnlyFix(t *testing.T) {
	h := newHarness(t)
	h.startWatcher(t, &fakeWatcher{result: shopResult(t)})
	// A heap cap for a container means making it again, which Snugbox
	// leaves to the person.
	h.expect(t, ExitOK, []string{"fit", "heap-caps"},
		"Container shop-app-1: set NODE_OPTIONS=--max-old-space-size=", "make the container again.",
		"Nothing here can be applied by Snugbox; the lines above are for you.")
}

func TestFitStatus(t *testing.T) {
	h := newHarness(t)
	now := time.Now()
	h.startWatcher(t, &fakeWatcher{changes: &localapi.Changes{Helper: true, Read: now, Changes: []localapi.Change{
		{ID: "20261031-100000-service-limits", Kind: "service-limits", State: "verifying", Since: now.Add(-5 * time.Minute), VerifyEnds: now.Add(25 * time.Minute)},
		{ID: "20261031-090000-container-limits", Kind: "container-limits", State: "reopened", Since: now.Add(-time.Hour), Reason: "pressure rose to 4% of the last minute"},
		{ID: "20261030-090000-swap", Kind: "swap", State: "watching", Since: now.Add(-25 * time.Hour), Counted: true},
		{ID: "20261029-090000-oom-protection", Kind: "oom-protection", State: "watching", Since: now.Add(-2 * time.Hour), Trouble: "a kill while paused"},
		{ID: "20261028-090000-heap-caps", Kind: "heap-caps", State: "undone", Since: now.Add(-72 * time.Hour), Reason: "undone by hand"},
	}}})
	h.expect(t, ExitOK, []string{"fit", "--status"},
		"20261031-100000-service-limits  service-limits, verifying since ", "Verified until ", "25 minutes from now.",
		"Reopened: pressure rose to 4% of the last minute",
		"Keep it with sudo snugbox fit --keep 20261031-090000-container-limits, or put it back with sudo snugbox undo 20261031-090000-container-limits.",
		"A full day has passed without trouble, so it counts as done",
		"Trouble: a kill while paused",
		"undone by hand")
}

func TestFitStatusEmptyAndHelperTrouble(t *testing.T) {
	h := newHarness(t)
	h.startWatcher(t, &fakeWatcher{changes: &localapi.Changes{Error: "snugbox-helper is not installed or its socket is not running"}})
	h.expect(t, ExitOK, []string{"fit", "--status"}, "No fix has been applied on this box.",
		"the watcher can't reach snugbox-helper: snugbox-helper is not installed")
}

func TestFitStatusWithoutTheWatcher(t *testing.T) {
	h := newHarness(t)
	// Only root may read the helper's journal directly.
	h.expect(t, ExitError, []string{"fit", "--status"}, localapi.ErrNotRunning.Error())
}

func TestReport(t *testing.T) {
	h := newHarness(t)
	h.expect(t, ExitError, []string{"report"}, "snugbox report reads the watcher's history; start the watcher with sudo snugbox watch install")

	now := time.Now()
	w := h.startWatcher(t, &fakeWatcher{result: shopResult(t), report: &localapi.Report{
		From: now.Add(-7 * 24 * time.Hour), To: now, WatchStart: now.Add(-30 * 24 * time.Hour),
		Then: &localapi.Snapshot{Snug: 3 << 30, Confidence: "high"}, Now: &localapi.Snapshot{Snug: 2 << 30, Confidence: "high"},
		Events:  []localapi.Event{{Time: now.Add(-time.Hour), Kind: "oom_kill", Workload: "shop-app-1"}},
		Changes: localapi.Changes{Helper: true, Changes: []localapi.Change{{ID: "20261031-100000-swap", Kind: "swap", State: "watching", Started: now.Add(-48 * time.Hour), Since: now.Add(-47 * time.Hour), Counted: true}}},
	}})
	h.expect(t, ExitOK, []string{"report", "--week"}, "SINCE", "the last 7 days.", "It needed 3 GiB at the start.", "This box pays for",
		"Nothing yet. A saving counts once", "20261031-100000-swap  swap, watching since", "kept, a day or more without trouble",
		"1 out-of-memory kill: shop-app-1 1.")
	asked := w.asked()
	i := slices.IndexFunc(asked, func(q string) bool { return strings.HasPrefix(q, "/v1/report?since=") })
	if i < 0 {
		t.Fatalf("the watcher was asked %q", asked)
	}
	since, err := time.Parse(time.RFC3339, strings.TrimPrefix(asked[i], "/v1/report?since="))
	if err != nil || since.Sub(now.Add(-7*24*time.Hour)).Abs() > time.Minute {
		t.Errorf("--week asked since %v (%v), want about %v", since, err, now.Add(-7*24*time.Hour))
	}
	h.expect(t, ExitUsage, []string{"report", "now"}, "See 'snugbox report --help'.")
}

func TestWatchStatus(t *testing.T) {
	h := newHarness(t)
	h.expect(t, ExitError, []string{"watch", "status"}, "snugboxd is not running; start it with sudo snugbox watch install")

	started := time.Now().Add(-2 * time.Hour)
	w := h.startWatcher(t, &fakeWatcher{status: localapi.Status{Version: "1.2.3", Started: started, Paused: true,
		Interval: 10 * time.Second, LastSample: time.Now(), Workloads: 4, StoreBytes: 3 << 20, WatchStart: started}})
	h.expect(t, ExitOK, []string{"watch", "status"}, "snugboxd 1.2.3, running since ", "every 10 seconds, 4 workloads.",
		"Paused: no fixes run and nothing goes to a hub until sudo snugbox resume.", "The first estimate is not ready yet.")

	w.mu.Lock()
	w.status.Analysed, w.status.Paused = true, false
	w.result = shopResult(t)
	w.mu.Unlock()
	h.expect(t, ExitOK, []string{"watch", "status"}, "Confidence low: ")
	if strings.Contains(h.stdout.String(), "Paused") {
		t.Error("watch status says paused after a resume")
	}
}

func TestErrorsWithoutAnExitCode(t *testing.T) {
	var e bytes.Buffer
	h := newHarness(t)
	h.e.stderr = &e
	if code := h.e.run(context.Background(), []string{"fit", "--status"}); code != ExitError || !strings.HasPrefix(e.String(), "snugbox: ") {
		t.Errorf("exit %d, stderr %q", code, e.String())
	}
	if (&exitError{code: ExitRisks}).Error() != "" || !errors.Is(withCode(1, os.ErrClosed), os.ErrClosed) {
		t.Error("exitError")
	}
}

func TestDoctorReport(t *testing.T) {
	h := newHarness(t)
	checks := []doctor.Check{
		{Status: doctor.Pass, Text: "Linux 6.8."},
		{Status: doctor.Warn, Text: "Not running as root."},
		{Status: doctor.Info, Text: "No swap."},
	}
	if err := printDoctor(h.e, "web-1", checks); err != nil {
		t.Fatalf("no check failed, yet printDoctor = %v", err)
	}
	for _, s := range []string{"snugbox doctor on web-1\n\n", "  ok    Linux 6.8.\n", "  warn  Not running as root.\n", "  info  No swap.\n",
		"3 checks: 1 ok, 1 for information, 1 warning, 0 failed.\n"} {
		if !strings.Contains(h.stdout.String(), s) {
			t.Errorf("the report lacks %q:\n%s", s, h.stdout.String())
		}
	}

	h.stdout.Reset()
	h.e.color = true
	err := printDoctor(h.e, "", append(checks, doctor.Check{Status: doctor.Fail, Text: "No cgroup v2."}))
	var ee *exitError
	if !errors.As(err, &ee) || ee.code != ExitError {
		t.Fatalf("a failed check gives %v, want exit code 1", err)
	}
	for _, s := range []string{"snugbox doctor on this box", "\x1b[31mfail \x1b[0m No cgroup v2.", "4 checks: 1 ok, 1 for information, 1 warning, 1 failed."} {
		if !strings.Contains(h.stdout.String(), s) {
			t.Errorf("the report lacks %q:\n%q", s, h.stdout.String())
		}
	}
}
