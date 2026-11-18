//go:build linux

package cli

import (
	"net"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"google.golang.org/protobuf/types/known/timestamppb"

	"snugbox.dev/snugbox/agent/internal/helperapi"
	"snugbox.dev/snugbox/agent/internal/localapi"
	helperv1 "snugbox.dev/snugbox/gen/snugbox/helper/v1"
)

// fakeHelper speaks the helper's protocol on a real socket, as the test's
// own user, and answers each request with what answer returns. answer runs
// under mu, so a test changes what it answers through update.
type fakeHelper struct {
	mu       sync.Mutex
	requests []*helperv1.Request
	answer   func(*helperv1.Request) []*helperv1.Response
}

func (h *harness) startHelper(t *testing.T, answer func(*helperv1.Request) []*helperv1.Response) *fakeHelper {
	t.Helper()
	ln, err := net.ListenUnix("unix", &net.UnixAddr{Name: filepath.Join(h.sockets, "h.sock"), Net: "unix"})
	if err != nil {
		t.Fatal(err)
	}
	if err := helperapi.PassCredentials(ln); err != nil {
		t.Fatal(err)
	}
	f := &fakeHelper{answer: answer}
	done := make(chan struct{})
	go func() {
		defer close(done)
		for {
			c, err := helperapi.Accept(ln)
			if err != nil {
				return
			}
			f.serve(c)
		}
	}()
	t.Cleanup(func() {
		_ = ln.Close()
		<-done
	})
	return f
}

func (f *fakeHelper) serve(c *helperapi.Conn) {
	defer c.Close()
	req := &helperv1.Request{}
	if err := helperapi.ReadFrame(c.Reader(), req, helperapi.MaxRequest); err != nil {
		return
	}
	f.mu.Lock()
	f.requests = append(f.requests, req)
	resps := f.answer(req)
	f.mu.Unlock()
	for _, resp := range resps {
		if err := helperapi.WriteFrame(c, resp, helperapi.MaxResponse); err != nil {
			return
		}
	}
}

// update runs change while no request is being answered, for state the
// answer function reads.
func (f *fakeHelper) update(change func()) {
	f.mu.Lock()
	defer f.mu.Unlock()
	change()
}

func (f *fakeHelper) seen() []*helperv1.Request {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.requests)
}

func step(s helperv1.State, detail string) *helperv1.Response {
	return &helperv1.Response{Kind: &helperv1.Response_Progress{Progress: &helperv1.Progress{State: s, Detail: detail}}}
}

func refusal(code helperv1.ErrorCode, msg string) []*helperv1.Response {
	return []*helperv1.Response{{Kind: &helperv1.Response_Error{Error: &helperv1.Error{Code: code, Message: msg}}}}
}

func listing(txs ...*helperv1.Transaction) []*helperv1.Response {
	return []*helperv1.Response{{Kind: &helperv1.Response_List{List: &helperv1.ListResponse{Transactions: txs}}}}
}

func tx(id string, s helperv1.State, since time.Time) *helperv1.Transaction {
	return &helperv1.Transaction{Id: id, State: s, Started: timestamppb.New(since), StateSince: timestamppb.New(since),
		Fix: &helperv1.Fix{Kind: &helperv1.Fix_ServiceLimits{ServiceLimits: &helperv1.ServiceLimitsFix{}}}}
}

const dropIn = "/etc/systemd/system/backup.service.d/50-snugbox-memory.conf"

// serviceLimitsHelper previews and applies a service limit the way the real
// helper reports one.
func serviceLimitsHelper(req *helperv1.Request) []*helperv1.Response {
	a := req.GetApply()
	if a == nil {
		return refusal(helperv1.ErrorCode_ERROR_CODE_INVALID, "this fake answers apply requests only")
	}
	if a.GetDryRun() {
		return []*helperv1.Response{{Kind: &helperv1.Response_Apply{Apply: &helperv1.ApplyResponse{
			Changes: []*helperv1.Change{
				{Target: dropIn, NewValue: "# snugbox transaction 20261031-090000-service-limits\n[Service]\nMemoryHigh=330M\nMemoryMax=400M\n", File: true},
				{Target: "backup.service MemoryMax", OldValue: "", NewValue: "400M"},
				{Target: "/srv/shop/compose.override.yaml", NewValue: "services:\n  app:\n    mem_limit: 1g\n", Manual: true},
			},
			Notes: []string{"backup.service is reloaded, not restarted."},
		}}}}
	}
	return []*helperv1.Response{
		step(helperv1.State_STATE_PREFLIGHT, "checking backup.service"),
		step(helperv1.State_STATE_SNAPSHOT, ""),
		step(helperv1.State_STATE_APPLYING, "writing "+dropIn),
		{Kind: &helperv1.Response_Apply{Apply: &helperv1.ApplyResponse{Transaction: "20261031-090000-service-limits"}}},
	}
}

func rootHarness(t *testing.T) (*harness, *fakeWatcher) {
	t.Helper()
	h := newHarness(t)
	h.e.euid = 0
	w := h.startWatcher(t, &fakeWatcher{result: shopResult(t), changes: &localapi.Changes{Helper: true}})
	return h, w
}

func TestFitPreviewAndApply(t *testing.T) {
	h, w := rootHarness(t)
	helper := h.startHelper(t, serviceLimitsHelper)

	h.expect(t, ExitOK, []string{"fit", "--dry-run", "--accept-low-confidence", "service-limits"},
		"Changes:", dropIn+" (new file)", "    + [Service]", "    + MemoryMax=400M",
		"backup.service MemoryMax: none -> 400M",
		"For you to add, since Snugbox won't write these:", "/srv/shop/compose.override.yaml", "    mem_limit: 1g",
		"Note: backup.service is reloaded, not restarted.", "Nothing was changed.")
	if reqs := helper.seen(); len(reqs) != 1 || !reqs[0].GetApply().GetDryRun() {
		t.Fatalf("a dry run sent %v", reqs)
	}

	h.expect(t, ExitOK, []string{"fit", "2", "--accept-low-confidence", "--yes"},
		"  preflight  checking backup.service", "  applying   writing "+dropIn,
		"Applied as 20261031-090000-service-limits.", "sudo snugbox undo 20261031-090000-service-limits")
	if strings.Contains(h.stdout.String(), "snapshot ") {
		t.Error("a progress step with no detail was printed")
	}
	reqs := helper.seen()
	if len(reqs) != 3 {
		t.Fatalf("%d requests, want a preview, then a preview and the change", len(reqs))
	}
	applied := reqs[2].GetApply()
	if applied.GetDryRun() || !applied.GetAcceptLowConfidence() {
		t.Errorf("the change went as %v", applied)
	}
	limits := applied.GetFix().GetServiceLimits().GetLimits()
	if len(limits) != 1 || limits[0].GetUnit() != "backup.service" || limits[0].GetMaxBytes() == 0 {
		t.Errorf("limits %v", limits)
	}
	if !slices.Contains(w.asked(), "/v1/changes?refresh=1") {
		t.Errorf("the watcher was not nudged after the fix: %q", w.asked())
	}
}

func TestFitAsksBeforeApplying(t *testing.T) {
	h, _ := rootHarness(t)
	helper := h.startHelper(t, serviceLimitsHelper)
	args := []string{"fit", "service-limits", "--accept-low-confidence"}

	// A script has no terminal to answer on, so it must say --yes.
	h.expect(t, ExitUsage, args, "standard input is not a terminal, so Snugbox can't ask; add --yes")
	h.e.stdinTTY = true
	for _, answer := range []string{"no\n", "\n", ""} {
		h.e.stdin = strings.NewReader(answer)
		h.expect(t, ExitOK, args, "Apply this fix? Type yes to go ahead: ", "Nothing was changed.")
	}
	for _, r := range helper.seen() {
		if !r.GetApply().GetDryRun() {
			t.Fatal("the fix was applied without a yes")
		}
	}
	h.e.stdin = strings.NewReader(" YES \n")
	h.expect(t, ExitOK, args, "Applied as 20261031-090000-service-limits.")
}

func TestFitRefusedWhilePaused(t *testing.T) {
	h, w := rootHarness(t)
	w.mu.Lock()
	w.status.Paused = true
	w.mu.Unlock()
	helper := h.startHelper(t, serviceLimitsHelper)
	h.expect(t, ExitError, []string{"fit", "service-limits", "--accept-low-confidence", "--yes"},
		"snugbox pause is in force, so no fix runs; run sudo snugbox resume first")
	if n := len(helper.seen()); n != 0 {
		t.Errorf("the helper got %d requests during a pause", n)
	}
}

func TestHelperRefusalsAreExplained(t *testing.T) {
	h, _ := rootHarness(t)
	args := []string{"fit", "service-limits", "--accept-low-confidence", "--yes"}
	h.expect(t, ExitError, args, "snugbox-helper is not installed or its socket is not running")

	var code helperv1.ErrorCode
	helper := h.startHelper(t, func(*helperv1.Request) []*helperv1.Response { return refusal(code, "refused for the test") })
	for c, hint := range map[helperv1.ErrorCode]string{
		helperv1.ErrorCode_ERROR_CODE_BUSY:     "See snugbox fit --status.",
		helperv1.ErrorCode_ERROR_CODE_STUCK:    "https://snugbox.dev/docs/fixes/stuck/",
		helperv1.ErrorCode_ERROR_CODE_PRESSURE: "",
	} {
		helper.update(func() { code = c })
		h.expect(t, ExitError, args, "snugbox: refused for the test", hint)
	}
}

func TestUndo(t *testing.T) {
	h, w := rootHarness(t)
	now := time.Now()
	txs := []*helperv1.Transaction{tx("20261030-090000-swap", helperv1.State_STATE_UNDONE, now)}
	helper := h.startHelper(t, func(req *helperv1.Request) []*helperv1.Response {
		if req.GetList() != nil {
			return listing(txs...)
		}
		u := req.GetUndo()
		id := u.GetTransaction()
		if id == "" {
			id = "20261031-090000-service-limits"
		}
		return []*helperv1.Response{step(helperv1.State_STATE_UNDOING, "removing "+dropIn),
			{Kind: &helperv1.Response_Undo{Undo: &helperv1.UndoResponse{Transaction: id, Notes: []string{"backup.service has no limit again."}}}}}
	})

	// The package's pre-removal script reads exit status 0 as "no fix in
	// place" and 2 as "fixes listed".
	h.expect(t, ExitOK, []string{"undo"}, "No fix is in place, so there is nothing to undo.")
	helper.update(func() {
		txs = append([]*helperv1.Transaction{tx("20261031-090000-service-limits", helperv1.State_STATE_VERIFYING, now)}, txs...)
	})
	h.expect(t, ExitUsage, []string{"undo"}, "Fixes in place, newest first:", "  20261031-090000-service-limits  service-limits, verifying",
		"name one of these, or use --last")
	if strings.Contains(h.stdout.String(), "20261030-090000-swap") {
		t.Error("an undone fix was listed as in place")
	}

	h.expect(t, ExitOK, []string{"undo", "--last"}, "  undoing    removing "+dropIn, "Note: backup.service has no limit again.",
		"Put back. 20261031-090000-service-limits is undone.")
	h.expect(t, ExitOK, []string{"undo", "20261030-090000-swap"}, "Put back. 20261030-090000-swap is undone.")
	h.expect(t, ExitUsage, []string{"undo", "../../etc/passwd"}, `"../../etc/passwd" is not a transaction ID`)
	var undos []string
	for _, r := range helper.seen() {
		if u := r.GetUndo(); u != nil {
			undos = append(undos, u.GetTransaction())
		}
	}
	if !slices.Equal(undos, []string{"", "20261030-090000-swap"}) {
		t.Errorf("undo requests %q", undos)
	}
	if n := strings.Count(strings.Join(w.asked(), " "), "refresh=1"); n != 2 {
		t.Errorf("the watcher was nudged %d times, want once per undo", n)
	}
}

func TestFitKeep(t *testing.T) {
	h, _ := rootHarness(t)
	helper := h.startHelper(t, func(req *helperv1.Request) []*helperv1.Response {
		a := req.GetAdvance()
		return []*helperv1.Response{{Kind: &helperv1.Response_Advance{Advance: &helperv1.AdvanceResponse{
			Transaction: tx(a.GetTransaction(), a.GetState(), time.Now())}}}}
	})
	h.expect(t, ExitOK, []string{"fit", "--keep", "20261031-090000-container-limits"}, "Kept. 20261031-090000-container-limits counts as done.")
	h.expect(t, ExitUsage, []string{"fit", "--keep", "yesterday"}, `"yesterday" is not a transaction ID`)
	reqs := helper.seen()
	if len(reqs) != 1 || reqs[0].GetAdvance().GetState() != helperv1.State_STATE_DONE {
		t.Errorf("requests %v", reqs)
	}
}

func TestFitStatusFromTheJournal(t *testing.T) {
	h := newHarness(t)
	h.e.euid = 0
	since := time.Now().Add(-10 * time.Minute)
	h.startHelper(t, func(*helperv1.Request) []*helperv1.Response {
		return listing(tx("20261031-090000-service-limits", helperv1.State_STATE_VERIFYING, since))
	})
	h.expect(t, ExitOK, []string{"fit", "--status"},
		"snugboxd is not running, so nothing is being verified; this is the helper's journal.",
		"20261031-090000-service-limits  service-limits, verifying since ", "20 minutes from now.")
}

func TestWatchUninstallKeepsFixesSafe(t *testing.T) {
	h := newHarness(t)
	h.e.euid = 0
	now := time.Now()
	txs := []*helperv1.Transaction{tx("20261031-090000-service-limits", helperv1.State_STATE_WATCHING, now)}
	helper := h.startHelper(t, func(req *helperv1.Request) []*helperv1.Response {
		if req.GetList() == nil || txs == nil {
			return refusal(helperv1.ErrorCode_ERROR_CODE_INTERNAL, "journal unreadable")
		}
		return listing(txs...)
	})
	h.expect(t, ExitError, []string{"watch", "uninstall"},
		"a fix is in place (20261031-090000-service-limits), and only the helper can put it back", "--keep-fixes")
	helper.update(func() { txs = append(txs, tx("20261030-090000-swap", helperv1.State_STATE_DONE, now)) })
	h.expect(t, ExitError, []string{"watch", "uninstall", "--purge"}, "2 fixes are in place (20261031-090000-service-limits, 20261030-090000-swap)")
	helper.update(func() { txs = nil })
	h.expect(t, ExitError, []string{"watch", "uninstall"}, "can't ask the helper which fixes are in place (journal unreadable); --keep-fixes removes Snugbox anyway")
}

func TestPauseAndResume(t *testing.T) {
	h := newHarness(t)
	h.e.euid = 0
	h.expect(t, ExitOK, []string{"pause"}, "Paused. No fix runs and nothing goes to a hub until sudo snugbox resume. Sampling goes on.")
	b, err := os.ReadFile(h.e.pauseFile)
	if err != nil || !strings.HasPrefix(string(b), "paused at ") {
		t.Fatalf("pause file %q, %v", b, err)
	}
	h.expect(t, ExitOK, []string{"pause"}, "Already paused.")
	h.expect(t, ExitOK, []string{"resume"}, "Resumed. Fixes and hub traffic can run again.")
	if _, err := os.Stat(h.e.pauseFile); !os.IsNotExist(err) {
		t.Errorf("resume left the pause file: %v", err)
	}
	h.expect(t, ExitOK, []string{"resume"}, "Not paused.")

	// A link planted at the flag's name is never written through.
	target := filepath.Join(t.TempDir(), "target")
	if err := os.Symlink(target, h.e.pauseFile); err != nil {
		t.Fatal(err)
	}
	h.run("pause")
	if _, err := os.Stat(target); !os.IsNotExist(err) {
		t.Errorf("pause wrote through a link: %v", err)
	}
}

func TestShareCardRefusesALink(t *testing.T) {
	h := newHarness(t)
	dir := t.TempDir()
	t.Chdir(dir)
	target := filepath.Join(t.TempDir(), "elsewhere.svg")
	if err := os.Symlink(target, filepath.Join(dir, "snugbox-card.svg")); err != nil {
		t.Fatal(err)
	}
	h.result = shopResult(t)
	h.expect(t, ExitError, []string{"audit", "--json", "--share"}, "share card: ")
	if _, err := os.Stat(target); !os.IsNotExist(err) {
		t.Errorf("the card was written through a link: %v", err)
	}
}
