//go:build linux && integration

// These tests drive the installed snugbox-helper through its socket on a
// real systemd. They need root, an installed helper (sudo snugbox watch
// install) and python3, and they change the box: each makes a throwaway
// service and removes it again. Build and run them on a test machine only:
//
//	go test -tags integration -c -o helper.test ./agent/internal/helper
//	sudo ./helper.test -test.v
package helper

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"os/user"
	"path"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"google.golang.org/protobuf/encoding/protojson"

	"snugbox.dev/snugbox/agent/internal/helperapi"
	"snugbox.dev/snugbox/agent/internal/journal"
	"snugbox.dev/snugbox/agent/internal/kernel"
	helperv1 "snugbox.dev/snugbox/gen/snugbox/helper/v1"
)

const hog = "memhog-it.service"

func needRoot(t *testing.T) {
	t.Helper()
	if os.Geteuid() != 0 {
		t.Skip("needs root and an installed snugbox-helper")
	}
}

func sh(t *testing.T, args ...string) string {
	t.Helper()
	out, err := exec.Command(args[0], args[1:]...).CombinedOutput()
	if err != nil {
		t.Fatalf("%v: %v: %s", args, err, out)
	}
	return string(out)
}

// startHog runs a service that holds 100 MiB of anonymous memory.
func startHog(t *testing.T) string {
	t.Helper()
	unit := "[Service]\nExecStart=/usr/bin/python3 -c \"import time; b=bytearray(100<<20); b[::4096]=b'\\x01'*((100<<20)//4096); time.sleep(10**6)\"\n"
	if err := os.WriteFile(path.Join(UnitDir, hog), []byte(unit), 0o644); err != nil {
		t.Fatal(err)
	}
	sh(t, "systemctl", "daemon-reload")
	sh(t, "systemctl", "start", hog)
	t.Cleanup(func() {
		_ = exec.Command("systemctl", "stop", hog).Run()
		_ = os.RemoveAll(path.Join(UnitDir, hog+".d"))
		_ = os.Remove(path.Join(UnitDir, hog))
		_ = exec.Command("systemctl", "daemon-reload").Run()
	})
	cg := path.Join(CgroupFS, "system.slice", hog)
	for range 50 {
		if b, err := os.ReadFile(path.Join(cg, "memory.current")); err == nil {
			if n, _ := strconv.ParseUint(strings.TrimSpace(string(b)), 10, 64); n > 90<<20 {
				return cg
			}
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatal("the hog never reached 90 MiB")
	return ""
}

func limits(high, maxB uint64) *helperv1.Request {
	return &helperv1.Request{Kind: &helperv1.Request_Apply{Apply: &helperv1.ApplyRequest{Fix: &helperv1.Fix{Kind: &helperv1.Fix_ServiceLimits{
		ServiceLimits: &helperv1.ServiceLimitsFix{Limits: []*helperv1.ServiceLimit{{Unit: hog, HighBytes: high, MaxBytes: maxB}}}}}}}}
}

func read(t *testing.T, p string) string {
	t.Helper()
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	return strings.TrimSpace(string(b))
}

func code(err error) helperv1.ErrorCode {
	var he *helperapi.Error
	if errors.As(err, &he) {
		return he.Code
	}
	return helperv1.ErrorCode_ERROR_CODE_UNSPECIFIED
}

// The helper refuses a fix while memory is under pressure. On a small box a
// test's own hog, or the watcher starting, can stall memory for a moment,
// so do and handle ask again for up to calmWait while that is the answer.
const calmWait = 2 * time.Minute

// do sends a request to the installed helper.
func do(t *testing.T, c *helperapi.Client, req *helperv1.Request, progress ...func(*helperv1.Progress)) (*helperv1.Response, error) {
	t.Helper()
	var p func(*helperv1.Progress)
	if len(progress) > 0 {
		p = progress[0]
	}
	for deadline := time.Now().Add(calmWait); ; time.Sleep(5 * time.Second) {
		resp, err := c.Do(context.Background(), req, p)
		if code(err) != helperv1.ErrorCode_ERROR_CODE_PRESSURE || time.Now().After(deadline) {
			return resp, err
		}
		t.Logf("waiting for calm: %v", err)
	}
}

// handle sends a request to an engine in the test process, as root.
func handle(t *testing.T, e *Engine, req *helperv1.Request) *helperv1.Response {
	t.Helper()
	for deadline := time.Now().Add(calmWait); ; time.Sleep(5 * time.Second) {
		resp := e.Handle(context.Background(), req, Root, nil)
		if resp.GetError().GetCode() != helperv1.ErrorCode_ERROR_CODE_PRESSURE || time.Now().After(deadline) {
			return resp
		}
		t.Logf("waiting for calm: %s", resp.GetError().GetMessage())
	}
}

func TestLiveServiceLimits(t *testing.T) {
	needRoot(t)
	cg := startHog(t)
	ctx := context.Background()
	c := &helperapi.Client{}

	_, err := do(t, c, limits(64<<20, 128<<20))
	if code(err) != helperv1.ErrorCode_ERROR_CODE_PRECONDITION || !strings.Contains(err.Error(), "would stop it at once") {
		t.Fatalf("a limit below what the service holds: %v", err)
	}

	var steps []string
	resp, err := do(t, c, limits(192<<20, 256<<20), func(p *helperv1.Progress) { steps = append(steps, helperapi.StateName(p.GetState())) })
	if err != nil {
		t.Fatal(err)
	}
	tx := resp.GetApply().GetTransaction()
	t.Logf("applied %s, steps %v", tx, steps)
	if got := read(t, path.Join(cg, "memory.max")); got != "268435456" {
		t.Errorf("memory.max %s after the fix", got)
	}
	if got := read(t, path.Join(cg, "memory.high")); got != "201326592" {
		t.Errorf("memory.high %s after the fix", got)
	}
	drop := path.Join(UnitDir, hog+".d", "50-snugbox-memory.conf")
	if got := read(t, drop); !strings.Contains(got, "transaction "+tx) {
		t.Errorf("drop-in:\n%s", got)
	}
	if out := sh(t, "systemctl", "is-active", hog); strings.TrimSpace(out) != "active" {
		t.Errorf("the service is %s after the limit", out)
	}
	runtimeGone(t, hog)

	_, err = do(t, c, limits(192<<20, 256<<20))
	if code(err) != helperv1.ErrorCode_ERROR_CODE_BUSY {
		t.Errorf("a second fix while the first is verified: %v", err)
	}

	u, err := c.Undo(ctx, tx, "", nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("undone %s, notes %q", u.GetTransaction(), u.GetNotes())
	if got := read(t, path.Join(cg, "memory.max")); got != "max" {
		t.Errorf("memory.max %s after the undo", got)
	}
	if _, err := os.Stat(drop); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("the drop-in is still there: %v", err)
	}
	folderGone(t, path.Dir(drop))
	runtimeGone(t, hog)
}

// runtimeGone fails the test when systemctl set-property left a runtime
// setting for unit, or systemd still holds files that are gone, which
// systemctl status would warn about.
func runtimeGone(t *testing.T, unit string) {
	t.Helper()
	folderGone(t, path.Join(ControlDir, unit+".d"))
	if got := strings.TrimSpace(sh(t, "systemctl", "show", "--property=NeedDaemonReload", "--value", "--", unit)); got != "no" {
		t.Errorf("systemd needs a reload for %s: NeedDaemonReload=%s", unit, got)
	}
}

// emptyFolders takes away the empty ones of dirs, left by an earlier run of
// a test on this box, and returns the dirs that are absent now, which the
// fix makes and its undo must take away again.
func emptyFolders(t *testing.T, dirs ...string) []string {
	t.Helper()
	var absent []string
	for _, d := range dirs {
		_ = syscall.Rmdir(d)
		if _, err := os.Lstat(d); errors.Is(err, os.ErrNotExist) {
			absent = append(absent, d)
		}
	}
	return absent
}

// folderGone fails the test when dir, a folder a fix made for its drop-in,
// is still there after the fix was undone.
func folderGone(t *testing.T, dirs ...string) {
	t.Helper()
	for _, d := range dirs {
		if _, err := os.Lstat(d); !errors.Is(err, os.ErrNotExist) {
			t.Errorf("%s is still there after the undo: %v", d, err)
		}
	}
}

// Blueprint section 16.2: a link planted at every path the helper writes is
// refused.
func TestLiveLinksAreRefused(t *testing.T) {
	needRoot(t)
	startHog(t)
	c := &helperapi.Client{}
	bait := t.TempDir()
	dropDir := path.Join(UnitDir, hog+".d")

	if err := os.Symlink(bait, dropDir); err != nil {
		t.Fatal(err)
	}
	_, err := do(t, c, limits(192<<20, 256<<20))
	if code(err) != helperv1.ErrorCode_ERROR_CODE_PRECONDITION || !strings.Contains(err.Error(), "symbolic link") {
		t.Errorf("a linked drop-in folder: %v", err)
	}
	if err := os.Remove(dropDir); err != nil {
		t.Fatal(err)
	}

	if err := os.Mkdir(dropDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(path.Join(bait, "owned"), path.Join(dropDir, "50-snugbox-memory.conf")); err != nil {
		t.Fatal(err)
	}
	_, err = do(t, c, limits(192<<20, 256<<20))
	if code(err) != helperv1.ErrorCode_ERROR_CODE_PRECONDITION {
		t.Errorf("a linked drop-in file: %v", err)
	}
	if entries, _ := os.ReadDir(bait); len(entries) != 0 {
		t.Errorf("something was written through the link: %v", entries)
	}

	swap := &helperv1.Request{Kind: &helperv1.Request_Apply{Apply: &helperv1.ApplyRequest{DryRun: true, Fix: &helperv1.Fix{Kind: &helperv1.Fix_Swap{Swap: &helperv1.SwapFix{SizeBytes: 256 << 20}}}}}}
	if out, _ := exec.Command("systemd-detect-virt", "--container").Output(); strings.TrimSpace(string(out)) != "none" {
		// A container shares the host's kernel, so swap is refused whatever
		// the links.
		if _, err := do(t, c, swap); code(err) != helperv1.ErrorCode_ERROR_CODE_PRECONDITION {
			t.Errorf("swap in a container: %v", err)
		} else {
			t.Logf("swap refused: %v", err)
		}
		return
	}
	// On a virtual machine, a link where the swap unit goes.
	unit := path.Join(UnitDir, SwapUnit)
	if err := os.Symlink(path.Join(bait, "swap"), unit); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Remove(unit) })
	if _, err := do(t, c, swap); code(err) != helperv1.ErrorCode_ERROR_CODE_PRECONDITION {
		t.Errorf("a linked swap unit: %v", err)
	}
	if entries, _ := os.ReadDir(bait); len(entries) != 0 {
		t.Errorf("something was written through the link: %v", entries)
	}
}

func TestLiveSnugboxUserMayNotApply(t *testing.T) {
	needRoot(t)
	u, err := user.Lookup("snugbox")
	if err != nil {
		t.Skip("no snugbox user")
	}
	uid, _ := strconv.Atoi(u.Uid)
	gid, _ := strconv.Atoi(u.Gid)
	cmd := exec.Command(os.Args[0], "-test.run=^TestLiveChildAsks$", "-test.v")
	cmd.Env = append(os.Environ(), "SNUGBOX_TEST_CHILD=ask")
	cmd.SysProcAttr = &syscall.SysProcAttr{Credential: &syscall.Credential{Uid: uint32(uid), Gid: uint32(gid)}}
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("child: %v\n%s", err, out)
	}
	if !strings.Contains(string(out), "list ok") || !strings.Contains(string(out), "apply DENIED") {
		t.Errorf("as the snugbox user:\n%s", out)
	}
}

func TestLiveChildAsks(t *testing.T) {
	if os.Getenv("SNUGBOX_TEST_CHILD") != "ask" {
		t.Skip("runs only as the child of TestLiveSnugboxUserMayNotApply")
	}
	c := &helperapi.Client{}
	if _, err := c.List(context.Background()); err != nil {
		t.Fatalf("list: %v", err)
	}
	// The parent reads these lines from this process's output.
	fmt.Fprintln(os.Stdout, "list ok")
	_, err := do(t, c, limits(192<<20, 256<<20))
	fmt.Fprintf(os.Stdout, "apply %s: %v\n", strings.TrimPrefix(code(err).String(), "ERROR_CODE_"), err)
}

func TestLivePauseRefuses(t *testing.T) {
	needRoot(t)
	startHog(t)
	if err := os.MkdirAll("/etc/snugbox", 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile("/etc/snugbox/paused", []byte("test\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Remove("/etc/snugbox/paused") })
	_, err := (&helperapi.Client{}).Do(context.Background(), limits(192<<20, 256<<20), nil)
	if code(err) != helperv1.ErrorCode_ERROR_CODE_PAUSED {
		t.Errorf("apply while paused: %v", err)
	}
}

// Blueprint section 16.2: snugbox pause stops everything within 5 seconds.
// A heap cap restarts a service that takes 20 seconds to stop, and the pause
// comes while it stops: the helper must give up on the restart and be
// putting the fix back within 5 seconds.
func TestLivePauseStopsARunningFix(t *testing.T) {
	needRoot(t)
	const unit = "slowstop-it.service"
	file := "[Service]\nExecStart=/usr/bin/python3 -c \"import time; time.sleep(10**6)\"\nExecStop=/bin/sleep 20\n"
	if err := os.WriteFile(path.Join(UnitDir, unit), []byte(file), 0o644); err != nil {
		t.Fatal(err)
	}
	sh(t, "systemctl", "daemon-reload")
	sh(t, "systemctl", "start", unit)
	t.Cleanup(func() {
		_ = os.Remove("/etc/snugbox/paused")
		_ = exec.Command("systemctl", "kill", unit).Run()
		_ = exec.Command("systemctl", "stop", unit).Run()
		_ = os.RemoveAll(path.Join(UnitDir, unit+".d"))
		_ = os.Remove(path.Join(UnitDir, unit))
		_ = exec.Command("systemctl", "daemon-reload").Run()
	})
	if err := os.MkdirAll("/etc/snugbox", 0o755); err != nil {
		t.Fatal(err)
	}
	pausedAt := make(chan time.Time, 1)
	// The watcher must end with the test: the cleanup above stops the unit,
	// and a pause written then would stop every later test's fix.
	done := make(chan struct{})
	var watching sync.WaitGroup
	watching.Add(1)
	t.Cleanup(func() {
		close(done)
		watching.Wait()
		_ = os.Remove("/etc/snugbox/paused")
	})
	go func() {
		defer watching.Done()
		for deadline := time.Now().Add(calmWait + time.Minute); time.Now().Before(deadline); time.Sleep(100 * time.Millisecond) {
			select {
			case <-done:
				return
			default:
			}
			state, _ := exec.Command("systemctl", "show", "-p", "ActiveState", "--value", unit).Output()
			if strings.TrimSpace(string(state)) == "deactivating" {
				_ = os.WriteFile("/etc/snugbox/paused", []byte("test\n"), 0o644)
				pausedAt <- time.Now()
				return
			}
		}
	}()
	req := &helperv1.Request{Kind: &helperv1.Request_Apply{Apply: &helperv1.ApplyRequest{Fix: &helperv1.Fix{Kind: &helperv1.Fix_HeapCaps{
		HeapCaps: &helperv1.HeapCapsFix{Caps: []*helperv1.HeapCap{{Unit: unit, Runtime: helperv1.Runtime_RUNTIME_GLIBC}}}}}}}}
	var undoing time.Time
	_, err := do(t, &helperapi.Client{}, req, func(p *helperv1.Progress) {
		if p.GetState() == helperv1.State_STATE_UNDOING && undoing.IsZero() {
			undoing = time.Now()
		}
	})
	_ = os.Remove("/etc/snugbox/paused")
	if code(err) != helperv1.ErrorCode_ERROR_CODE_FAILED || !strings.Contains(err.Error(), "pause came into force") {
		t.Fatalf("apply with a pause during the restart: %v", err)
	}
	select {
	case at := <-pausedAt:
		if took := undoing.Sub(at); undoing.IsZero() || took > 5*time.Second {
			t.Errorf("the helper began putting the fix back %v after the pause; blueprint section 16.2 allows 5 seconds", took)
		} else {
			t.Logf("putting the fix back %v after the pause", took.Round(time.Millisecond))
		}
	default:
		t.Fatal("the service never began to stop")
	}
	if _, err := os.Stat(path.Join(UnitDir, unit+".d", "50-snugbox-heap.conf")); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("the heap cap outlived the pause: %v", err)
	}
	folderGone(t, path.Join(UnitDir, unit+".d"))
}

// TestLiveRecovery cuts a fix short the way a power cut would: the journal
// ends at "applying", the drop-in is on disk, and the limit step's
// set-property call has put the limits on the running service but not yet
// removed the runtime files it leaves. The next request starts the helper,
// which puts the change back before it answers.
func TestLiveRecovery(t *testing.T) {
	needRoot(t)
	cg := startHog(t)
	// With the socket stopped too, nothing can start the helper while the
	// journal is being written behind its back.
	sh(t, "systemctl", "stop", "snugbox-helper.socket", "snugbox-helper.service")
	t.Cleanup(func() { _ = exec.Command("systemctl", "start", "snugbox-helper.socket").Run() })
	tx := "20261031-010101-service-limits"
	drop := path.Join(UnitDir, hog+".d", "50-snugbox-memory.conf")
	plan := &Plan{Kind: helperapi.KindService, AutoUndo: true, Cgroups: []string{"system.slice/" + hog}, Steps: []Step{
		{Op: OpFile, Target: drop, Label: hog, Mode: 0o644, High: 192 << 20, Max: 256 << 20,
			New: header(tx, helperapi.KindService) + "[Service]\nMemoryHigh=201326592\nMemoryMax=268435456\n"},
		{Op: OpReload},
		{Op: OpLimit, Target: hog, High: 192 << 20, Max: 256 << 20},
	}}
	manifest, err := json.Marshal(plan)
	if err != nil {
		t.Fatal(err)
	}
	live := NewLive()
	if err := live.SaveSnapshot(tx, manifest); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(path.Join(UndoDir, tx)) })
	jr, _, err := OpenJournal()
	if err != nil {
		t.Fatal(err)
	}
	fix, err := protojson.Marshal(limits(192<<20, 256<<20).GetApply().GetFix())
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(manifest)
	for _, s := range []struct {
		state string
		data  any
	}{
		{"approved", approvedData{Fix: fix}},
		{"preflight", nil},
		{"snapshot", snapshotData{Manifest: hex.EncodeToString(sum[:]), Cgroups: plan.Cgroups, AutoUndo: true}},
		{"applying", nil},
	} {
		if _, err := jr.Append(tx, s.state, "root", "", s.data); err != nil {
			t.Fatal(err)
		}
	}
	if err := jr.Close(); err != nil {
		t.Fatal(err)
	}
	if err := live.WriteFile(drop, []byte(plan.Steps[0].New), 0o644); err != nil {
		t.Fatal(err)
	}
	sh(t, "systemctl", "daemon-reload")
	// What the limit step does before it removes the runtime files.
	sh(t, "systemctl", "set-property", "--runtime", "--", hog, "MemoryHigh=201326592", "MemoryMax=268435456")
	if got := read(t, path.Join(cg, "memory.max")); got != "268435456" {
		t.Fatalf("the half-applied fix did not take: memory.max %s", got)
	}
	sh(t, "systemctl", "start", "snugbox-helper.socket")

	txs, err := (&helperapi.Client{}).List(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	var got *helperv1.Transaction
	for _, x := range txs {
		if x.GetId() == tx {
			got = x
		}
	}
	if got == nil || got.GetState() != helperv1.State_STATE_UNDONE || !strings.Contains(got.GetReason(), "stopped in the middle") {
		t.Fatalf("after the restart: %v", got)
	}
	if v := read(t, path.Join(cg, "memory.max")); v != "max" {
		t.Errorf("memory.max %s after recovery", v)
	}
	if v := read(t, path.Join(cg, "memory.high")); v != "max" {
		t.Errorf("memory.high %s after recovery", v)
	}
	if _, err := os.Stat(drop); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("the drop-in survived recovery: %v", err)
	}
	runtimeGone(t, hog)
}

// TestLiveHeapCap sets MALLOC_ARENA_MAX on a real service: a drop-in, a
// reload and a restart, then the same backwards on undo.
func TestLiveHeapCap(t *testing.T) {
	needRoot(t)
	startHog(t)
	ctx := context.Background()
	c := &helperapi.Client{}
	req := &helperv1.Request{Kind: &helperv1.Request_Apply{Apply: &helperv1.ApplyRequest{Fix: &helperv1.Fix{Kind: &helperv1.Fix_HeapCaps{
		HeapCaps: &helperv1.HeapCapsFix{Caps: []*helperv1.HeapCap{{Unit: hog, Runtime: helperv1.Runtime_RUNTIME_GLIBC}}}}}}}}
	before := strings.TrimSpace(sh(t, "systemctl", "show", "-p", "ExecMainPID", "--value", hog))
	resp, err := do(t, c, req)
	if err != nil {
		t.Fatal(err)
	}
	tx := resp.GetApply().GetTransaction()
	after := strings.TrimSpace(sh(t, "systemctl", "show", "-p", "ExecMainPID", "--value", hog))
	if before == after {
		t.Errorf("the service was not restarted: main PID %s before and after", before)
	}
	if env := read(t, "/proc/"+after+"/environ"); !strings.Contains(env, "MALLOC_ARENA_MAX=2") {
		t.Error("the restarted process has no MALLOC_ARENA_MAX")
	}
	if _, err := c.Undo(ctx, tx, "", nil); err != nil {
		t.Fatal(err)
	}
	back := strings.TrimSpace(sh(t, "systemctl", "show", "-p", "ExecMainPID", "--value", hog))
	if back == after {
		t.Error("the undo did not restart the service")
	}
	if strings.Contains(read(t, "/proc/"+back+"/environ"), "MALLOC_ARENA_MAX") {
		t.Error("the undo restarted the service but MALLOC_ARENA_MAX is still set")
	}
}

// vmBox is the live box with one difference: it says it is a virtual
// machine, so the swap fix runs under WSL, which systemd-detect-virt calls a
// container.
type vmBox struct{ *Live }

func (v vmBox) Tool(ctx context.Context, name string, stdin []byte, args ...string) (string, error) {
	if name == "systemd-detect-virt" {
		return "none\n", errors.New("exit status 1")
	}
	return v.Live.Tool(ctx, name, stdin, args...)
}

// containerType is what systemd-detect-virt --container says, "none" outside
// a container.
func containerType() string {
	out, _ := exec.Command("systemd-detect-virt", "--container").Output()
	return strings.TrimSpace(string(out))
}

// TestLiveSwap makes a real swap file, turns it on through its systemd
// unit, and takes it all away again on undo. systemd refuses swap units in
// a container, which is what it calls WSL, so this needs a virtual machine.
// Swap turned on by hand is turned off for the test and back on after it; a
// box with swap set up in /etc/fstab or a swap unit of its own gets the
// refusal TestLiveLeavesTheirSwapAlone checks instead.
func TestLiveSwap(t *testing.T) {
	needRoot(t)
	if v := containerType(); v != "none" {
		t.Skipf("systemd does not start swap units in a %s container; run this in a virtual machine", v)
	}
	if theirs := theirSwap(t); theirs != "" {
		t.Skipf("the box has %s, which the swap fix leaves alone; TestLiveLeavesTheirSwapAlone checks that", theirs)
	}
	swapsOff(t)
	f, err := os.OpenFile(path.Join(t.TempDir(), "journal"), os.O_RDWR|os.O_APPEND|os.O_CREATE, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	jr, _, err := journal.Open(f, time.Now)
	if err != nil {
		t.Fatal(err)
	}
	e := New(vmBox{NewLive()}, jr, Config{Exe: os.Args[0], Pause: "/etc/snugbox/paused", MemTotal: 2 << 30, Log: slog.New(slog.DiscardHandler)})
	resp := handle(t, e, &helperv1.Request{Kind: &helperv1.Request_Apply{Apply: &helperv1.ApplyRequest{
		Fix: &helperv1.Fix{Kind: &helperv1.Fix_Swap{Swap: &helperv1.SwapFix{SizeBytes: 256 << 20}}}}}})
	if er := resp.GetError(); er != nil {
		t.Fatalf("apply: %s %s", er.GetCode(), er.GetMessage())
	}
	tx := resp.GetApply().GetTransaction()
	t.Cleanup(func() { _ = os.RemoveAll(path.Join(UndoDir, tx)) })
	if swaps := read(t, "/proc/swaps"); !strings.Contains(swaps, SwapFile) {
		t.Fatalf("the swap file is not in use:\n%s", swaps)
	}
	fi, err := os.Stat(SwapFile)
	if err != nil {
		t.Fatal(err)
	}
	st, ok := fi.Sys().(*syscall.Stat_t)
	if !ok || fi.Mode().Perm() != 0o600 || st.Uid != 0 || fi.Size() != 256<<20 {
		t.Errorf("swap file mode %v size %d, %+v", fi.Mode(), fi.Size(), fi.Sys())
	}
	if got := strings.TrimSpace(sh(t, "systemctl", "is-enabled", SwapUnit)); got != "enabled" {
		t.Errorf("the swap unit is %s", got)
	}
	resp = handle(t, e, &helperv1.Request{Kind: &helperv1.Request_Undo{Undo: &helperv1.UndoRequest{Transaction: tx}}})
	if er := resp.GetError(); er != nil {
		t.Fatalf("undo: %s %s", er.GetCode(), er.GetMessage())
	}
	if strings.Contains(read(t, "/proc/swaps"), SwapFile) {
		t.Error("swap is still on after the undo")
	}
	for _, p := range []string{SwapFile, path.Join(UnitDir, SwapUnit)} {
		if _, err := os.Stat(p); !errors.Is(err, os.ErrNotExist) {
			t.Errorf("%s is still there: %v", p, err)
		}
	}
}

// boxPressure reads the box's memory pressure, as the helper's gate does.
func boxPressure(t *testing.T) kernel.Pressure {
	t.Helper()
	p, err := kernel.ParsePressure([]byte(read(t, "/proc/pressure/memory")))
	if err != nil {
		t.Fatal(err)
	}
	return p
}

// pressured is the helper's own test for a box too busy to change.
func pressured(p kernel.Pressure) bool {
	return p.Full.Avg10 > PressureFullAvg10 || p.Some.Avg60 > PressureSomeAvg60
}

// TestLiveSwapUndoWaitsForPressure is blueprint section 16.2's proof that
// undo never runs swapoff under pressure, on a real kernel: with the swap
// fix on, a service held under a small MemoryHigh thrashes until the box's
// pressure stall information shows it, and the undo must be refused with
// swap still on. Once the box is calm again, the same undo goes through.
func TestLiveSwapUndoWaitsForPressure(t *testing.T) {
	needRoot(t)
	if v := containerType(); v != "none" {
		t.Skipf("systemd does not start swap units in a %s container; run this in a virtual machine", v)
	}
	if theirs := theirSwap(t); theirs != "" {
		t.Skipf("the box has %s, which the swap fix leaves alone", theirs)
	}
	if _, err := os.Stat("/proc/pressure/memory"); err != nil {
		t.Skip("this kernel has pressure stall information off")
	}
	swapsOff(t)
	e := liveEngine(t)
	tx := apply(t, e, &helperv1.Fix{Kind: &helperv1.Fix_Swap{Swap: &helperv1.SwapFix{SizeBytes: 256 << 20}}})
	undo := &helperv1.Request{Kind: &helperv1.Request_Undo{Undo: &helperv1.UndoRequest{Transaction: tx}}}
	done := false
	t.Cleanup(func() {
		if !done {
			handle(t, e, undo)
		}
	})

	// 128 MiB touched again and again under a 32 MiB MemoryHigh: the kernel
	// throttles the service and pushes its pages to the swap file and back.
	const unit = "pressure-it.service"
	thrash := "import time\nb = bytearray(128 << 20)\nend = time.time() + 180\nwhile time.time() < end:\n    for i in range(0, len(b), 4096):\n        b[i] = (b[i] + 1) & 255\n"
	sh(t, "systemd-run", "--unit="+unit, "--property=MemoryHigh=32M", "--", "/usr/bin/python3", "-c", thrash)
	stop := func() {
		_ = exec.Command("systemctl", "stop", unit).Run()
		_ = exec.Command("systemctl", "reset-failed", unit).Run()
	}
	t.Cleanup(stop)
	deadline := time.Now().Add(90 * time.Second)
	for !pressured(boxPressure(t)) {
		if time.Now().After(deadline) {
			t.Fatalf("the thrashing service raised no pressure in 90 seconds: %+v", boxPressure(t))
		}
		time.Sleep(time.Second)
	}
	p := boxPressure(t)
	// One request, not handle's retries: the refusal is what is under test.
	resp := e.Handle(context.Background(), undo, Root, nil)
	if er := resp.GetError(); er.GetCode() != helperv1.ErrorCode_ERROR_CODE_PRESSURE {
		t.Fatalf("undo at some avg60=%.2f full avg10=%.2f wasn't refused for pressure: %v", p.Some.Avg60, p.Full.Avg10, resp)
	}
	if !strings.Contains(read(t, "/proc/swaps"), SwapFile) {
		t.Fatal("swap went off under pressure")
	}
	t.Logf("refused at some avg60=%.2f full avg10=%.2f: %s", p.Some.Avg60, p.Full.Avg10, resp.GetError().GetMessage())

	stop()
	start := time.Now()
	for pressured(boxPressure(t)) {
		if time.Since(start) > 5*time.Minute {
			t.Fatalf("the box was still under pressure 5 minutes after the service stopped: %+v", boxPressure(t))
		}
		time.Sleep(5 * time.Second)
	}
	if resp = handle(t, e, undo); resp.GetError() != nil {
		t.Fatalf("undo once calm, %s after the service stopped: %v", time.Since(start).Round(time.Second), resp)
	}
	done = true
	if strings.Contains(read(t, "/proc/swaps"), SwapFile) {
		t.Error("swap is still on after the undo")
	}
	t.Logf("undone once calm, %s after the service stopped", time.Since(start).Round(time.Second))
}

// TestLiveSwapFile makes the swap file the way the fix does, with fallocate
// and mkswap, and has the kernel take it with swapon, which proves the file
// has no holes on this filesystem. It runs in a container too. On Btrfs the
// fix makes the file with btrfs filesystem mkswapfile instead, which
// TestLiveBtrfsSwapFile and, on a Btrfs root, TestLiveSwap cover.
func TestLiveSwapFile(t *testing.T) {
	needRoot(t)
	live := NewLive()
	if _, fstype, err := live.Disk(path.Dir(SwapFile)); err != nil {
		t.Fatal(err)
	} else if fstype == "btrfs" {
		t.Skipf("%s is on Btrfs, where the fix doesn't use fallocate", path.Dir(SwapFile))
	}
	if _, err := os.Stat(SwapFile); err == nil {
		t.Fatalf("%s exists already", SwapFile)
	}
	if err := live.CreateSwapFile(SwapFile, 128<<20); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = exec.Command("swapoff", SwapFile).Run()
		_ = os.Remove(SwapFile)
	})
	if err := live.CreateSwapFile(SwapFile, 128<<20); err == nil {
		t.Error("a second swap file was made over the first")
	}
	if _, err := live.Tool(context.Background(), "mkswap", nil, "--label", "snugbox", "--", SwapFile); err != nil {
		t.Fatal(err)
	}
	sh(t, "swapon", SwapFile)
	if !strings.Contains(read(t, "/proc/swaps"), SwapFile) {
		t.Fatal("the kernel did not take the swap file")
	}
	on, err := swapActive(live, SwapFile)
	if err != nil || !on {
		t.Errorf("swapActive = %v, %v", on, err)
	}
	sh(t, "swapoff", SwapFile)
}

// TestLiveEarlyoom turns earlyoom on through its drop-in and off again on
// undo. It needs earlyoom installed and no out-of-memory daemon running; the
// earlyoom package starts its service, so stop it first.
func TestLiveEarlyoom(t *testing.T) {
	needRoot(t)
	if _, err := exec.LookPath("earlyoom"); err != nil {
		t.Skip("earlyoom is not installed")
	}
	for _, u := range []string{"systemd-oomd", "earlyoom", "nohang"} {
		if exec.Command("systemctl", "is-active", "--quiet", u+".service").Run() == nil {
			t.Skipf("%s runs; stop it to run this test", u)
		}
	}
	before := strings.TrimSpace(sh(t, "sh", "-c", "systemctl is-enabled earlyoom.service || true"))
	made := emptyFolders(t, path.Join(UnitDir, "earlyoom.service.d"))
	f, err := os.OpenFile(path.Join(t.TempDir(), "journal"), os.O_RDWR|os.O_APPEND|os.O_CREATE, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	jr, _, err := journal.Open(f, time.Now)
	if err != nil {
		t.Fatal(err)
	}
	e := New(NewLive(), jr, Config{Exe: os.Args[0], Pause: "/etc/snugbox/paused", MemTotal: 2 << 30, Log: slog.New(slog.DiscardHandler)})
	resp := handle(t, e, &helperv1.Request{Kind: &helperv1.Request_Apply{Apply: &helperv1.ApplyRequest{
		Fix: &helperv1.Fix{Kind: &helperv1.Fix_OomProtection{OomProtection: &helperv1.OOMProtectionFix{Daemon: helperv1.OOMDaemon_OOM_DAEMON_EARLYOOM}}}}}})
	if er := resp.GetError(); er != nil {
		t.Fatalf("apply: %s %s", er.GetCode(), er.GetMessage())
	}
	tx := resp.GetApply().GetTransaction()
	t.Cleanup(func() { _ = os.RemoveAll(path.Join(UndoDir, tx)) })
	// A slow box may still be starting earlyoom: its command line is empty,
	// or systemd's "(earlyoom)" title, until the program itself runs.
	var cmdline string
	starting := func() bool {
		c := strings.TrimSpace(cmdline)
		return c == "" || strings.HasPrefix(c, "(") && strings.HasSuffix(c, ")")
	}
	for deadline := time.Now().Add(10 * time.Second); starting() && time.Now().Before(deadline); time.Sleep(200 * time.Millisecond) {
		pid := strings.TrimSpace(sh(t, "systemctl", "show", "-p", "MainPID", "--value", "earlyoom.service"))
		if b, err := os.ReadFile("/proc/" + pid + "/cmdline"); err == nil {
			cmdline = strings.ReplaceAll(string(b), "\x00", " ")
		}
	}
	if !strings.Contains(cmdline, "--avoid ^(sshd|systemd|") || !strings.Contains(cmdline, "snugbox-helper)$") {
		t.Errorf("earlyoom runs as %q, without the drop-in's arguments", cmdline)
	}
	resp = handle(t, e, &helperv1.Request{Kind: &helperv1.Request_Undo{Undo: &helperv1.UndoRequest{Transaction: tx}}})
	if er := resp.GetError(); er != nil {
		t.Fatalf("undo: %s %s", er.GetCode(), er.GetMessage())
	}
	if exec.Command("systemctl", "is-active", "--quiet", "earlyoom.service").Run() == nil {
		t.Error("earlyoom still runs after the undo")
	}
	if after := strings.TrimSpace(sh(t, "sh", "-c", "systemctl is-enabled earlyoom.service || true")); after != before {
		t.Errorf("earlyoom is %s after the undo, was %s", after, before)
	}
	if _, err := os.Stat(path.Join(UnitDir, "earlyoom.service.d", "50-snugbox-earlyoom.conf")); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("the drop-in is still there: %v", err)
	}
	folderGone(t, made...)
}

// TestLiveBtrfsSwapFile makes a swap file the way the fix does on Btrfs, on
// a throwaway Btrfs filesystem in a loop file, and has the kernel take it.
// It needs btrfs-progs 6.1 or newer.
func TestLiveBtrfsSwapFile(t *testing.T) {
	needRoot(t)
	for _, tool := range []string{"btrfs", "mkfs.btrfs"} {
		if _, err := exec.LookPath(tool); err != nil {
			t.Skipf("%s is not installed", tool)
		}
	}
	// Ubuntu 22.04 has btrfs-progs 5.16; the swap fix refuses Btrfs there,
	// which TestSwapPreconditions covers.
	if exec.Command("btrfs", "filesystem", "mkswapfile", "--help").Run() != nil {
		t.Skip("btrfs-progs is older than 6.1 and has no filesystem mkswapfile, so the swap fix refuses Btrfs here")
	}
	dir := t.TempDir()
	img, mnt := path.Join(dir, "btrfs.img"), path.Join(dir, "mnt")
	if err := os.WriteFile(img, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Truncate(img, 1<<30); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(mnt, 0o700); err != nil {
		t.Fatal(err)
	}
	sh(t, "mkfs.btrfs", "-q", img)
	sh(t, "mount", "-o", "loop", img, mnt)
	t.Cleanup(func() { _ = exec.Command("umount", mnt).Run() })

	swap := path.Join(mnt, "swapfile")
	s := &Step{Op: OpSwapFile, Target: swap, Size: 256 << 20, FS: "btrfs"}
	live := NewLive()
	if err := s.do(context.Background(), live, os.Args[0]); err != nil {
		t.Fatal(err)
	}
	fi, err := os.Stat(swap)
	if err != nil {
		t.Fatal(err)
	}
	if fi.Size() != 256<<20 || fi.Mode().Perm() != 0o600 {
		t.Errorf("swap file size %d mode %v", fi.Size(), fi.Mode())
	}
	sh(t, "swapon", swap)
	if !strings.Contains(read(t, "/proc/swaps"), swap) {
		t.Fatal("the kernel did not take the Btrfs swap file")
	}
	sh(t, "swapoff", swap)
	if note, err := s.undo(context.Background(), live, os.Args[0]); err != nil {
		t.Fatalf("undo: %v (%s)", err, note)
	}
	if _, err := os.Stat(swap); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("undo left the swap file: %v", err)
	}
}

// The journal on disk passes its own check after everything above.
func TestLiveJournalChain(t *testing.T) {
	needRoot(t)
	f, err := os.Open(Journal)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	entries, torn, err := journal.Read(f)
	if err != nil || torn != 0 {
		t.Fatalf("the journal fails its check: %v, torn %d", err, torn)
	}
	t.Logf("%d journal lines, head %.12s", len(entries), entries[len(entries)-1].Hash)
}

// liveTimer makes a timer that starts a oneshot service of the same name on
// calendar, enabled as a package ships one: timers.target wants it, so
// systemd keeps what it knows about the timer, its last run among it, for
// as long as the box runs. It returns when the service has run so far.
func liveTimer(t *testing.T, name, calendar string, persistent bool) func() []time.Time {
	t.Helper()
	log := path.Join(t.TempDir(), name)
	files := map[string]string{
		name + ".service": "[Service]\nType=oneshot\nExecStart=/bin/sh -c 'date +%%s >> " + log + "'\n",
		name + ".timer":   fmt.Sprintf("[Timer]\nOnCalendar=%s\nAccuracySec=1s\nPersistent=%t\n\n[Install]\nWantedBy=timers.target\n", calendar, persistent),
	}
	for file, body := range files {
		if err := os.WriteFile(path.Join(UnitDir, file), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	t.Cleanup(func() {
		_ = exec.Command("systemctl", "disable", "--now", name+".timer").Run()
		_ = os.RemoveAll(path.Join(UnitDir, name+".timer.d"))
		_ = os.Remove(path.Join(UnitDir, name+".timer"))
		_ = os.Remove(path.Join(UnitDir, name+".service"))
		_ = os.Remove("/var/lib/systemd/timers/stamp-" + name + ".timer")
		_ = exec.Command("systemctl", "daemon-reload").Run()
	})
	sh(t, "systemctl", "daemon-reload")
	sh(t, "systemctl", "enable", "--now", name+".timer")
	return func() []time.Time {
		b, _ := os.ReadFile(log)
		var out []time.Time
		for line := range strings.Lines(string(b)) {
			s, err := strconv.ParseInt(strings.TrimSpace(line), 10, 64)
			if err != nil {
				continue
			}
			// When the clock is stepped back, as WSL does to keep up with
			// its host, systemd lets a timer that has just fired fire again
			// as the clock passes the same second. That counts as one run.
			if at := time.Unix(s, 0).UTC(); len(out) == 0 || !at.Equal(out[len(out)-1]) {
				out = append(out, at)
			}
		}
		return out
	}
}

// timerNext reads a timer's one calendar and when it fires next, in UTC.
func timerNext(t *testing.T, timer string) (string, time.Time) {
	t.Helper()
	spec, next, ok := calendarTrigger(strings.TrimSpace(sh(t, "env", "TZ=UTC", "systemctl", "show", "--property=TimersCalendar", "--", timer)))
	if !ok {
		t.Fatalf("%s has no calendar", timer)
	}
	at, err := time.ParseInLocation(systemdUTC, next, time.UTC)
	if err != nil {
		t.Fatalf("%s fires next at %q: %v", timer, next, err)
	}
	return spec, at
}

// liveEngine is an engine in the test process, on the live box, with a journal of its own.
func liveEngine(t *testing.T) *Engine {
	t.Helper()
	f, err := os.OpenFile(path.Join(t.TempDir(), "journal"), os.O_RDWR|os.O_APPEND|os.O_CREATE, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	jr, _, err := journal.Open(f, time.Now)
	if err != nil {
		t.Fatal(err)
	}
	return New(NewLive(), jr, Config{Exe: os.Args[0], Pause: "/etc/snugbox/paused", MemTotal: 2 << 30, Log: slog.New(slog.DiscardHandler)})
}

func applySchedule(t *testing.T, e *Engine, shifts ...*helperv1.TimerShift) *helperv1.Response {
	t.Helper()
	fix := &helperv1.Fix{Kind: &helperv1.Fix_Schedule{Schedule: &helperv1.ScheduleFix{Shifts: shifts}}}
	return handle(t, e, &helperv1.Request{Kind: &helperv1.Request_Apply{Apply: &helperv1.ApplyRequest{Fix: fix}}})
}

func undoSchedule(t *testing.T, e *Engine, tx string) *helperv1.UndoResponse {
	t.Helper()
	resp := handle(t, e, &helperv1.Request{Kind: &helperv1.Request_Undo{Undo: &helperv1.UndoRequest{Transaction: tx}}})
	if er := resp.GetError(); er != nil {
		t.Fatalf("undo: %s %s", er.GetCode(), er.GetMessage())
	}
	return resp.GetUndo()
}

// TestLiveSchedule moves a real timer 15 minutes earlier and back, and checks
// what systemd itself says it will do. The timer fires on weekdays, half
// past the hour twelve hours from now, so the move is never refused for a
// job about to run.
func TestLiveSchedule(t *testing.T) {
	needRoot(t)
	hour := time.Now().UTC().Add(12 * time.Hour).Hour()
	liveTimer(t, "backup-it", fmt.Sprintf("Mon..Fri %02d:30 UTC", hour), false)
	before := fmt.Sprintf("Mon..Fri *-*-* %02d:30:00 UTC", hour)
	if got, _ := timerNext(t, "backup-it.timer"); got != before {
		t.Fatalf("systemd reads the test timer as %q", got)
	}
	e := liveEngine(t)
	resp := applySchedule(t, e, &helperv1.TimerShift{Timer: "backup-it.timer", Service: "backup-it.service", Minutes: 15})
	if er := resp.GetError(); er != nil {
		t.Fatalf("apply: %s %s", er.GetCode(), er.GetMessage())
	}
	tx := resp.GetApply().GetTransaction()
	t.Cleanup(func() { _ = os.RemoveAll(path.Join(UndoDir, tx)) })
	spec, next := timerNext(t, "backup-it.timer")
	if want := fmt.Sprintf("Mon..Fri *-*-* %02d:15:00 UTC", hour); spec != want {
		t.Errorf("after the fix the timer fires at %q, want %q", spec, want)
	}
	if next.Hour() != hour || next.Minute() != 15 {
		t.Errorf("systemd's next run is %s, not at %02d:15", next, hour)
	}

	undoSchedule(t, e, tx)
	if got, _ := timerNext(t, "backup-it.timer"); got != before {
		t.Errorf("after the undo the timer fires at %q", got)
	}
	if _, err := os.Stat(path.Join(UnitDir, "backup-it.timer.d", helperapi.ScheduleDropIn)); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("the drop-in is still there: %v", err)
	}
	folderGone(t, path.Join(UnitDir, "backup-it.timer.d"))
}

// TestLiveScheduleRunsNoExtraJob moves two enabled timers that have just
// fired, one of them persistent, a minute earlier, to a time that has
// passed already, and checks that systemd runs neither job again, after
// the move or after its undo.
func TestLiveScheduleRunsNoExtraJob(t *testing.T) {
	needRoot(t)
	// Long enough ahead for both timers to be loaded and running before
	// that time, even on a slow box.
	at := time.Now().UTC().Add(20 * time.Second).Truncate(time.Second)
	calendar := "*-*-* " + at.Format(time.TimeOnly) + " UTC"
	runs := map[string]func() []time.Time{"move-it": liveTimer(t, "move-it", calendar, false), "move-pit": liveTimer(t, "move-pit", calendar, true)}
	for deadline := time.Now().Add(60 * time.Second); len(runs["move-it"]()) < 1 || len(runs["move-pit"]()) < 1; time.Sleep(time.Second) {
		if time.Now().After(deadline) {
			t.Fatal("the test timers never fired")
		}
	}
	// A run the move set off would then start in a second of its own.
	time.Sleep(2 * time.Second)

	e := liveEngine(t)
	resp := applySchedule(t, e, &helperv1.TimerShift{Timer: "move-it.timer", Service: "move-it.service", Minutes: 1},
		&helperv1.TimerShift{Timer: "move-pit.timer", Service: "move-pit.service", Minutes: 1})
	if er := resp.GetError(); er != nil {
		t.Fatalf("apply: %s %s", er.GetCode(), er.GetMessage())
	}
	tx := resp.GetApply().GetTransaction()
	t.Cleanup(func() { _ = os.RemoveAll(path.Join(UndoDir, tx)) })
	time.Sleep(5 * time.Second)
	for name, ran := range runs {
		if got := len(ran()); got != 1 {
			t.Errorf("%s ran %d times; the move ran it again", name, got)
		}
		if _, next := timerNext(t, name+".timer"); !next.Equal(at.Add(24*time.Hour - time.Minute)) {
			t.Errorf("%s next fires at %s after the move, want %s", name, next, at.Add(24*time.Hour-time.Minute))
		}
	}

	// The jobs last ran at their old time, so putting it back runs nothing.
	if notes := undoSchedule(t, e, tx).GetNotes(); len(notes) != 0 {
		t.Errorf("undo notes %q", notes)
	}
	time.Sleep(3 * time.Second)
	for name, ran := range runs {
		if got := len(ran()); got != 1 {
			t.Errorf("%s ran %d times; the undo ran it again", name, got)
		}
		if _, next := timerNext(t, name+".timer"); !next.Equal(at.Add(24 * time.Hour)) {
			t.Errorf("%s next fires at %s after the undo, want %s", name, next, at.Add(24*time.Hour))
		}
	}
}

// TestLiveScheduleRefusesAJobDueSoon asks to move a job due in 30 seconds a
// minute earlier, which would start it at once.
func TestLiveScheduleRefusesAJobDueSoon(t *testing.T) {
	needRoot(t)
	at := time.Now().UTC().Add(30 * time.Second).Truncate(time.Second)
	runs := liveTimer(t, "soon-it", "*-*-* "+at.Format(time.TimeOnly)+" UTC", false)
	resp := applySchedule(t, liveEngine(t), &helperv1.TimerShift{Timer: "soon-it.timer", Service: "soon-it.service", Minutes: 1})
	if er := resp.GetError(); er.GetCode() != helperv1.ErrorCode_ERROR_CODE_PRECONDITION || !strings.Contains(er.GetMessage(), "it would start it at once") {
		t.Fatalf("apply: %v", resp)
	}
	if _, err := os.Stat(path.Join(UnitDir, "soon-it.timer.d")); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("a refused move left a drop-in folder: %v", err)
	}
	if n := len(runs()); n != 0 {
		t.Errorf("the job ran %d times before its time", n)
	}
}

// TestLiveScheduleUndoAfterAnEarlyRun moves a job a minute earlier, lets it
// run at its new time, and undoes the move once the old time has passed
// too: systemd counts from the early run and starts the job again at once,
// and the undo says so. It takes about two minutes.
func TestLiveScheduleUndoAfterAnEarlyRun(t *testing.T) {
	needRoot(t)
	// The new time must still be more than a minute away when the move is
	// made, or the helper refuses it.
	old := time.Now().UTC().Add(2*time.Minute + 15*time.Second).Truncate(time.Second)
	runs := liveTimer(t, "early-it", "*-*-* "+old.Format(time.TimeOnly)+" UTC", false)
	e := liveEngine(t)
	resp := applySchedule(t, e, &helperv1.TimerShift{Timer: "early-it.timer", Service: "early-it.service", Minutes: 1})
	if er := resp.GetError(); er != nil {
		t.Fatalf("apply: %s %s", er.GetCode(), er.GetMessage())
	}
	tx := resp.GetApply().GetTransaction()
	t.Cleanup(func() { _ = os.RemoveAll(path.Join(UndoDir, tx)) })
	// Wait by the wall clock, which systemd's calendars follow.
	for time.Now().Before(old.Add(5 * time.Second)) {
		time.Sleep(200 * time.Millisecond)
	}
	if got := runs(); len(got) != 1 || old.Sub(got[0]) < 58*time.Second || old.Sub(got[0]) > 62*time.Second {
		t.Fatalf("the job ran at %v by its old time %s; want once, a minute before", got, old)
	}

	undone := time.Now().Truncate(time.Second)
	notes := undoSchedule(t, e, tx).GetNotes()
	if want := "systemd started early-it.service at once when the move was put back"; len(notes) != 1 || !strings.HasPrefix(notes[0], want) {
		t.Errorf("undo notes %q, want one starting %q", notes, want)
	}
	time.Sleep(3 * time.Second)
	if got := runs(); len(got) != 2 || got[1].Before(undone) || got[1].After(undone.Add(3*time.Second)) {
		t.Errorf("the job ran at %v; want a second run as the undo finished at %s, and no more", got, undone.Format(time.TimeOnly))
	}
}

// swapsOff turns off every swap area the box has for the test, and back on
// after it.
func swapsOff(t *testing.T) {
	t.Helper()
	var theirs []string
	for _, line := range strings.Split(read(t, "/proc/swaps"), "\n")[1:] {
		if f := strings.Fields(line); len(f) > 0 {
			theirs = append(theirs, f[0])
		}
	}
	for _, s := range theirs {
		sh(t, "swapoff", s)
	}
	t.Cleanup(func() {
		for _, s := range theirs {
			_ = exec.Command("swapon", s).Run()
		}
	})
}

// theirSwap names a swap setup of the box's own that the swap fix leaves
// alone: a swap line in /etc/fstab, or a swap unit file, such as the
// dev-zram0.swap Fedora's zram-generator makes. "" means none, which leaves
// only swap turned on by hand, the kind swapsOff can turn off for a test.
func theirSwap(t *testing.T) string {
	t.Helper()
	if raw, err := os.ReadFile("/etc/fstab"); err == nil && fstabHasSwap(string(raw)) {
		return "a swap line in /etc/fstab"
	}
	out, _ := exec.Command("systemctl", "list-unit-files", "--type=swap", "--no-legend").Output()
	for line := range strings.Lines(string(out)) {
		if f := strings.Fields(line); len(f) > 0 && f[0] != SwapUnit {
			return "the swap unit " + f[0]
		}
	}
	return ""
}

// TestLiveLeavesTheirSwapAlone asks for a swap fix on a box that has swap of
// its own, such as Fedora's swap on zram or a swap file in /etc/fstab, and
// checks that the helper refuses and makes nothing.
func TestLiveLeavesTheirSwapAlone(t *testing.T) {
	needRoot(t)
	if v := containerType(); v != "none" {
		t.Skipf("in a %s container the swap fix refuses for the container instead", v)
	}
	theirs := theirSwap(t)
	if theirs == "" && strings.Count(strings.TrimSpace(read(t, "/proc/swaps")), "\n") == 0 {
		t.Skip("the box has no swap of its own")
	}
	resp := handle(t, liveEngine(t), &helperv1.Request{Kind: &helperv1.Request_Apply{Apply: &helperv1.ApplyRequest{
		Fix: &helperv1.Fix{Kind: &helperv1.Fix_Swap{Swap: &helperv1.SwapFix{SizeBytes: 256 << 20}}}}}})
	er := resp.GetError()
	// Each of the three refusals for swap the box has says so this way.
	if er.GetCode() != helperv1.ErrorCode_ERROR_CODE_PRECONDITION || !strings.Contains(er.GetMessage(), "Snugbox leaves") {
		t.Fatalf("the swap fix wasn't refused for the box's own swap (%s): %v", theirs, resp)
	}
	for _, p := range []string{SwapFile, path.Join(UnitDir, SwapUnit)} {
		if _, err := os.Stat(p); !errors.Is(err, os.ErrNotExist) {
			t.Errorf("the refused fix left %s: %v", p, err)
		}
	}
	t.Logf("refused: %s", er.GetMessage())
}

func apply(t *testing.T, e *Engine, fix *helperv1.Fix) string {
	t.Helper()
	resp := handle(t, e, &helperv1.Request{Kind: &helperv1.Request_Apply{Apply: &helperv1.ApplyRequest{Fix: fix}}})
	if er := resp.GetError(); er != nil {
		t.Fatalf("apply: %s %s", er.GetCode(), er.GetMessage())
	}
	tx := resp.GetApply().GetTransaction()
	t.Cleanup(func() { _ = os.RemoveAll(path.Join(UndoDir, tx)) })
	return tx
}

// TestLiveZswap makes the swap file with zswap, zstd and the shrinker on,
// checks that the kernel took each setting and that the tmpfiles line sets
// it again at boot, and that the undo puts every setting back. It needs a
// virtual machine whose kernel has zswap, with zswap off.
func TestLiveZswap(t *testing.T) {
	needRoot(t)
	if v := containerType(); v != "none" {
		t.Skipf("systemd does not start swap units in a %s container; run this in a virtual machine", v)
	}
	if _, err := os.Stat(path.Join(ZswapDir, "shrinker_enabled")); err != nil {
		t.Skip("this kernel has no zswap shrinker")
	}
	param := func(name string) string { return read(t, path.Join(ZswapDir, name)) }
	if param("enabled") == "Y" {
		t.Skip("zswap is on already")
	}
	before := map[string]string{"enabled": param("enabled"), "compressor": param("compressor"), "shrinker_enabled": param("shrinker_enabled")}
	if theirs := theirSwap(t); theirs != "" {
		t.Skipf("the box has %s, which the swap fix leaves alone; TestLiveLeavesTheirSwapAlone checks that", theirs)
	}
	swapsOff(t)
	e := liveEngine(t)
	tx := apply(t, e, &helperv1.Fix{Kind: &helperv1.Fix_Swap{Swap: &helperv1.SwapFix{SizeBytes: 256 << 20, Compressor: helperv1.Compressor_COMPRESSOR_ZSTD, Shrinker: true}}})
	for name, want := range map[string]string{"enabled": "Y", "compressor": "zstd", "shrinker_enabled": "Y"} {
		if got := param(name); got != want {
			t.Errorf("zswap %s is %s, want %s", name, got, want)
		}
	}
	// What tmpfiles does at boot: turned off by hand, zswap comes back on.
	if err := os.WriteFile(path.Join(ZswapDir, "enabled"), []byte("N"), 0o644); err != nil {
		t.Fatal(err)
	}
	sh(t, "systemd-tmpfiles", "--create", TmpFiles)
	if got := param("enabled"); got != "Y" {
		t.Errorf("after systemd-tmpfiles zswap enabled is %s", got)
	}

	resp := handle(t, e, &helperv1.Request{Kind: &helperv1.Request_Undo{Undo: &helperv1.UndoRequest{Transaction: tx}}})
	if er := resp.GetError(); er != nil {
		t.Fatalf("undo: %s %s", er.GetCode(), er.GetMessage())
	}
	for name, want := range before {
		if got := param(name); got != want {
			t.Errorf("zswap %s is %s after the undo, was %s", name, got, want)
		}
	}
	for _, p := range []string{TmpFiles, SwapFile, path.Join(UnitDir, SwapUnit)} {
		if _, err := os.Stat(p); !errors.Is(err, os.ErrNotExist) {
			t.Errorf("%s is still there: %v", p, err)
		}
	}
}

// theirOOMPolicy says which slice has a systemd-oomd policy the fix leaves
// alone, such as the one Fedora's systemd-oomd-defaults gives system.slice,
// or "" when neither has one.
func theirOOMPolicy(t *testing.T) string {
	t.Helper()
	for _, slice := range []string{"system.slice", "user.slice"} {
		if v := strings.TrimSpace(sh(t, "systemctl", "show", "-p", "ManagedOOMMemoryPressure", "--value", "--", slice)); v != "auto" && v != "" {
			return slice + " has ManagedOOMMemoryPressure=" + v
		}
	}
	return ""
}

// needOOMDStopped skips a test unless systemd-oomd is installed and no
// out-of-memory daemon runs, which is how the box looks when the fix is
// offered.
func needOOMDStopped(t *testing.T) {
	t.Helper()
	if strings.TrimSpace(sh(t, "systemctl", "show", "-p", "LoadState", "--value", "systemd-oomd.service")) != "loaded" {
		t.Skip("systemd-oomd is not installed")
	}
	for _, u := range []string{"systemd-oomd", "earlyoom", "nohang"} {
		if exec.Command("systemctl", "is-active", "--quiet", u+".service").Run() == nil {
			t.Skipf("%s runs; stop it to run this test", u)
		}
	}
}

// TestLiveOOMDLeavesTheirPolicyAlone asks for the systemd-oomd fix where a
// slice has a policy already, as Fedora ships system.slice, and checks that
// the helper refuses, writes no drop-in and doesn't start systemd-oomd.
func TestLiveOOMDLeavesTheirPolicyAlone(t *testing.T) {
	needRoot(t)
	needOOMDStopped(t)
	policy := theirOOMPolicy(t)
	if policy == "" {
		t.Skip("neither slice has a systemd-oomd policy of the box's own")
	}
	resp := handle(t, liveEngine(t), &helperv1.Request{Kind: &helperv1.Request_Apply{Apply: &helperv1.ApplyRequest{
		Fix: &helperv1.Fix{Kind: &helperv1.Fix_OomProtection{OomProtection: &helperv1.OOMProtectionFix{Daemon: helperv1.OOMDaemon_OOM_DAEMON_SYSTEMD_OOMD}}}}}})
	if er := resp.GetError(); er.GetCode() != helperv1.ErrorCode_ERROR_CODE_PRECONDITION || !strings.Contains(er.GetMessage(), "Snugbox leaves that policy alone") {
		t.Fatalf("the fix wasn't refused where %s: %v", policy, resp)
	}
	for _, d := range []string{"system.slice.d", "user.slice.d", "ssh.service.d", "sshd.service.d"} {
		if p := path.Join(UnitDir, d, "50-snugbox-oomd.conf"); fileExists(p) {
			t.Errorf("the refused fix wrote %s", p)
		}
	}
	if exec.Command("systemctl", "is-active", "--quiet", "systemd-oomd.service").Run() == nil {
		t.Error("the refused fix started systemd-oomd")
	}
	t.Logf("refused where %s", policy)
}

func fileExists(p string) bool {
	_, err := os.Lstat(p)
	return err == nil
}

// TestLiveOOMD turns systemd-oomd on for the system and user slices, with
// ssh left out of its choices, and back off. It needs the systemd-oomd
// package, no out-of-memory daemon running, and neither slice with a policy
// of the box's own.
func TestLiveOOMD(t *testing.T) {
	needRoot(t)
	needOOMDStopped(t)
	if policy := theirOOMPolicy(t); policy != "" {
		t.Skipf("%s already, which the fix leaves alone; TestLiveOOMDLeavesTheirPolicyAlone checks that", policy)
	}
	enabled := func() string {
		return strings.TrimSpace(sh(t, "sh", "-c", "systemctl is-enabled systemd-oomd.service || true"))
	}
	prop := func(unit, name string) string {
		return strings.TrimSpace(sh(t, "systemctl", "show", "-p", name, "--value", "--", unit))
	}
	before := enabled()
	made := emptyFolders(t, path.Join(UnitDir, "system.slice.d"), path.Join(UnitDir, "user.slice.d"),
		path.Join(UnitDir, "ssh.service.d"), path.Join(UnitDir, "sshd.service.d"))
	e := liveEngine(t)
	tx := apply(t, e, &helperv1.Fix{Kind: &helperv1.Fix_OomProtection{OomProtection: &helperv1.OOMProtectionFix{Daemon: helperv1.OOMDaemon_OOM_DAEMON_SYSTEMD_OOMD}}})
	if exec.Command("systemctl", "is-active", "--quiet", "systemd-oomd.service").Run() != nil {
		t.Error("systemd-oomd is not running after the fix")
	}
	for _, slice := range []string{"system.slice", "user.slice"} {
		if got := prop(slice, "ManagedOOMMemoryPressure"); got != "kill" {
			t.Errorf("%s has ManagedOOMMemoryPressure=%s", slice, got)
		}
	}
	// Debian and Ubuntu call it ssh.service, RHEL and its rebuilds
	// sshd.service; the fix covers whichever is loaded.
	ssh := 0
	for _, u := range []string{"ssh.service", "sshd.service"} {
		if prop(u, "LoadState") != "loaded" {
			continue
		}
		ssh++
		if got := prop(u, "ManagedOOMPreference"); got != "omit" {
			t.Errorf("%s has ManagedOOMPreference=%s", u, got)
		}
	}
	if ssh == 0 {
		t.Error("no ssh unit is loaded, so nothing shows ssh is left out of systemd-oomd's choices")
	}
	if dump := sh(t, "oomctl", "dump"); !strings.Contains(dump, "/system.slice") || !strings.Contains(dump, "/user.slice") {
		t.Errorf("systemd-oomd doesn't watch both slices:\n%s", dump)
	}

	resp := handle(t, e, &helperv1.Request{Kind: &helperv1.Request_Undo{Undo: &helperv1.UndoRequest{Transaction: tx}}})
	if er := resp.GetError(); er != nil {
		t.Fatalf("undo: %s %s", er.GetCode(), er.GetMessage())
	}
	if exec.Command("systemctl", "is-active", "--quiet", "systemd-oomd.service").Run() == nil && before != "enabled" {
		t.Error("systemd-oomd still runs after the undo")
	}
	if after := enabled(); after != before {
		t.Errorf("systemd-oomd is %s after the undo, was %s", after, before)
	}
	for _, slice := range []string{"system.slice", "user.slice"} {
		if got := prop(slice, "ManagedOOMMemoryPressure"); got != "auto" {
			t.Errorf("%s has ManagedOOMMemoryPressure=%s after the undo", slice, got)
		}
	}
	folderGone(t, made...)
}

func needDocker(t *testing.T) {
	t.Helper()
	if exec.Command("docker", "info").Run() != nil {
		t.Skip("Docker is not running")
	}
}

// dockerRun starts a container from busybox that sleeps, and removes it
// after the test. It returns the container's full ID.
func dockerRun(t *testing.T, name string) string {
	t.Helper()
	_ = exec.Command("docker", "rm", "-f", name).Run()
	out, err := exec.Command("docker", "run", "-d", "--quiet", "--name", name, "busybox:1.37", "sleep", "3600").Output()
	if err != nil {
		t.Fatalf("docker run: %v", err)
	}
	t.Cleanup(func() { _ = exec.Command("docker", "rm", "-f", name).Run() })
	return strings.TrimSpace(string(out))
}

func liveContainerFix(persist bool, id string) *helperv1.Request {
	return &helperv1.Request{Kind: &helperv1.Request_Apply{Apply: &helperv1.ApplyRequest{Fix: &helperv1.Fix{Kind: &helperv1.Fix_ContainerLimits{
		ContainerLimits: &helperv1.ContainerLimitsFix{Persist: persist, Limits: []*helperv1.ContainerLimit{{ContainerId: id, MaxBytes: 256 << 20, ReservationBytes: 128 << 20}}}}}}}}
}

// TestLiveContainerLimits limits a running container through the installed
// helper and puts it back. Docker can't take a limit away, so the undo
// raises it to the largest value, which the kernel reads as none.
func TestLiveContainerLimits(t *testing.T) {
	needRoot(t)
	needDocker(t)
	id := dockerRun(t, "snugbox-it")
	c := &helperapi.Client{}
	resp, err := do(t, c, liveContainerFix(false, id))
	if err != nil {
		t.Fatalf("apply: %v", err)
	}
	tx := resp.GetApply().GetTransaction()
	limits := func() string {
		return strings.TrimSpace(sh(t, "docker", "inspect", "-f", "{{.HostConfig.Memory}} {{.HostConfig.MemoryReservation}}", id))
	}
	cg := path.Join(CgroupFS, "system.slice", "docker-"+id+".scope")
	if got := limits(); got != "268435456 134217728" {
		t.Errorf("Docker reports limits %s", got)
	}
	if got := read(t, path.Join(cg, "memory.max")); got != "268435456" {
		t.Errorf("memory.max %s", got)
	}
	if _, err := do(t, c, &helperv1.Request{Kind: &helperv1.Request_Undo{Undo: &helperv1.UndoRequest{Transaction: tx}}}); err != nil {
		t.Fatalf("undo: %v", err)
	}
	if got := limits(); got != "9223372036854775807 6291456" {
		t.Errorf("after the undo Docker reports limits %s", got)
	}
	if got := read(t, path.Join(cg, "memory.max")); got != "max" {
		t.Errorf("memory.max %s after the undo", got)
	}
}

// TestLiveComposeOverride limits a Compose service and keeps the limit in
// compose.override.yaml, which the helper writes as the project folder's
// owner, then removes it again on undo.
func TestLiveComposeOverride(t *testing.T) {
	needRoot(t)
	needDocker(t)
	if exec.Command("docker", "compose", "version").Run() != nil {
		t.Skip("the Docker Compose plugin is not installed")
	}
	dir, err := os.MkdirTemp("/srv", "snugbox-it-")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	project := path.Base(dir)
	compose := path.Join(dir, "compose.yaml")
	if err := os.WriteFile(compose, []byte("services:\n  app:\n    image: busybox:1.37\n    command: sleep 3600\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	sh(t, "chown", "-R", "nobody:nogroup", dir)
	t.Cleanup(func() {
		_ = exec.Command("docker", "compose", "--project-directory", dir, "-p", project, "down", "--timeout", "1").Run()
		_ = os.RemoveAll(dir)
	})
	sh(t, "docker", "compose", "--project-directory", dir, "-p", project, "up", "-d")
	id := strings.TrimSpace(sh(t, "docker", "inspect", "-f", "{{.Id}}", project+"-app-1"))

	c := &helperapi.Client{}
	resp, err := do(t, c, liveContainerFix(true, id))
	if err != nil {
		t.Fatalf("apply: %v", err)
	}
	tx := resp.GetApply().GetTransaction()
	override := path.Join(dir, "compose.override.yaml")
	fi, err := os.Stat(override)
	if err != nil {
		t.Fatalf("no override: %v", err)
	}
	if st, ok := fi.Sys().(*syscall.Stat_t); !ok || st.Uid != 65534 {
		t.Errorf("the override is owned by %+v, not nobody", fi.Sys())
	}
	if body := read(t, override); !strings.Contains(body, "  app:\n    mem_limit: 256m\n    mem_reservation: 128m") || !strings.Contains(body, tx) {
		t.Errorf("override:\n%s", body)
	}
	if merged := sh(t, "docker", "compose", "--project-directory", dir, "-p", project, "config"); !strings.Contains(merged, "mem_limit") || !strings.Contains(merged, "268435456") {
		t.Errorf("Compose doesn't read the override as a 256 MiB limit:\n%s", merged)
	}
	if _, err := do(t, c, &helperv1.Request{Kind: &helperv1.Request_Undo{Undo: &helperv1.UndoRequest{Transaction: tx}}}); err != nil {
		t.Fatalf("undo: %v", err)
	}
	if _, err := os.Stat(override); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("the override is still there: %v", err)
	}
}

// rebootState is what the first run of TestLiveReboot leaves for the second.
type rebootState struct {
	Swap, Cut string
	Zswap     bool // the swap fix turned zswap on, so the second run checks it came back
}

const rebootStateFile = "/var/tmp/snugbox-reboot-it.json"

// TestLiveReboot reboots the box in the middle of two fixes, as section 9.3
// asks, in two runs with the reboot between them. With
// SNUGBOX_LIVE_REBOOT=before it applies a swap fix through the installed
// helper, with zswap where the kernel has it, which leaves it in its
// verification window, unless the box has swap of its own, then cuts a
// service limit short after its drop-in was written and its limits set on
// the running service, the way a power cut would, and stops the helper.
// With SNUGBOX_LIVE_REBOOT=after, once the box is back, it checks that swap
// and zswap came back on by themselves, that the swap fix is still being
// verified, and that the first request to the helper put the cut-short
// limit back. Then it undoes the swap fix.
func TestLiveReboot(t *testing.T) {
	needRoot(t)
	phase := os.Getenv("SNUGBOX_LIVE_REBOOT")
	if phase == "" {
		t.Skip("run with SNUGBOX_LIVE_REBOOT=before, reboot, then run with SNUGBOX_LIVE_REBOOT=after")
	}
	if v := containerType(); v != "none" {
		t.Skipf("a %s container can't be rebooted", v)
	}
	const unit = "reboot-it.service"
	cg := path.Join(CgroupFS, "system.slice", unit)
	drop := path.Join(UnitDir, unit+".d", "50-snugbox-memory.conf")
	c := &helperapi.Client{}
	switch phase {
	case "before":
		body := "[Service]\nExecStart=/usr/bin/python3 -c \"import time; b=bytearray(100<<20); b[::4096]=b'\\x01'*((100<<20)//4096); time.sleep(10**6)\"\n\n[Install]\nWantedBy=multi-user.target\n"
		if err := os.WriteFile(path.Join(UnitDir, unit), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
		sh(t, "systemctl", "daemon-reload")
		sh(t, "systemctl", "enable", "--now", unit)
		state := rebootState{Cut: time.Now().UTC().Format("20060102-150405") + "-service-limits"}
		if theirs := theirSwap(t); theirs != "" || strings.Count(strings.TrimSpace(read(t, "/proc/swaps")), "\n") > 0 {
			// The swap fix leaves the box's own swap alone, which
			// TestLiveLeavesTheirSwapAlone checks; only the limit goes
			// through the reboot here.
			t.Logf("the box has swap of its own (%s), so no swap fix goes through the reboot", theirs)
		} else {
			// Ask for zswap only where the kernel has it; on a kernel
			// without it the fix engine makes a plain swap file, so the
			// test does too.
			swap := &helperv1.SwapFix{SizeBytes: 256 << 20}
			if _, err := os.Stat(path.Join(ZswapDir, "enabled")); err == nil {
				swap.Compressor = helperv1.Compressor_COMPRESSOR_ZSTD
				if _, err := os.Stat(path.Join(ZswapDir, "shrinker_enabled")); err == nil {
					swap.Shrinker = true
				}
			}
			resp, err := do(t, c, &helperv1.Request{Kind: &helperv1.Request_Apply{Apply: &helperv1.ApplyRequest{Fix: &helperv1.Fix{
				Kind: &helperv1.Fix_Swap{Swap: swap}}}}})
			if err != nil {
				t.Fatalf("swap fix: %v", err)
			}
			state.Swap, state.Zswap = resp.GetApply().GetTransaction(), swap.GetCompressor() != helperv1.Compressor_COMPRESSOR_UNSPECIFIED
		}
		// Nothing may start the helper before the reboot.
		sh(t, "systemctl", "stop", "snugbox-helper.socket", "snugbox-helper.service")
		plan := &Plan{Kind: helperapi.KindService, AutoUndo: true, Cgroups: []string{"system.slice/" + unit}, Steps: []Step{
			{Op: OpFile, Target: drop, Label: unit, Mode: 0o644, High: 192 << 20, Max: 256 << 20,
				New: header(state.Cut, helperapi.KindService) + "[Service]\nMemoryHigh=201326592\nMemoryMax=268435456\n"},
			{Op: OpReload},
			{Op: OpLimit, Target: unit, High: 192 << 20, Max: 256 << 20},
		}}
		manifest, err := json.Marshal(plan)
		if err != nil {
			t.Fatal(err)
		}
		live := NewLive()
		if err := live.SaveSnapshot(state.Cut, manifest); err != nil {
			t.Fatal(err)
		}
		jr, _, err := OpenJournal()
		if err != nil {
			t.Fatal(err)
		}
		fix, err := protojson.Marshal(&helperv1.Fix{Kind: &helperv1.Fix_ServiceLimits{ServiceLimits: &helperv1.ServiceLimitsFix{
			Limits: []*helperv1.ServiceLimit{{Unit: unit, HighBytes: 192 << 20, MaxBytes: 256 << 20}}}}})
		if err != nil {
			t.Fatal(err)
		}
		sum := sha256.Sum256(manifest)
		for _, s := range []struct {
			state string
			data  any
		}{
			{"approved", approvedData{Fix: fix}},
			{"preflight", nil},
			{"snapshot", snapshotData{Manifest: hex.EncodeToString(sum[:]), Cgroups: plan.Cgroups, AutoUndo: true}},
			{"applying", nil},
		} {
			if _, err := jr.Append(state.Cut, s.state, "root", "", s.data); err != nil {
				t.Fatal(err)
			}
		}
		if err := jr.Close(); err != nil {
			t.Fatal(err)
		}
		if err := live.WriteFile(drop, []byte(plan.Steps[0].New), 0o644); err != nil {
			t.Fatal(err)
		}
		sh(t, "systemctl", "daemon-reload")
		sh(t, "systemctl", "set-property", "--runtime", "--", unit, "MemoryHigh=201326592", "MemoryMax=268435456")
		if got := read(t, path.Join(cg, "memory.max")); got != "268435456" {
			t.Fatalf("the half-applied limit did not take: memory.max %s", got)
		}
		raw, err := json.Marshal(state)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(rebootStateFile, raw, 0o600); err != nil {
			t.Fatal(err)
		}
		t.Logf("swap fix %s verifying, %s cut short; reboot now", state.Swap, state.Cut)
	case "after":
		raw, err := os.ReadFile(rebootStateFile)
		if err != nil {
			t.Fatalf("no state from the first run: %v", err)
		}
		var state rebootState
		if err := json.Unmarshal(raw, &state); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() {
			_ = exec.Command("systemctl", "disable", "--now", unit).Run()
			_ = os.RemoveAll(path.Join(UnitDir, unit+".d"))
			_ = os.Remove(path.Join(UnitDir, unit))
			_ = exec.Command("systemctl", "daemon-reload").Run()
			_ = os.RemoveAll(path.Join(UndoDir, state.Cut))
			if state.Swap != "" {
				_ = os.RemoveAll(path.Join(UndoDir, state.Swap))
			}
			_ = os.Remove(rebootStateFile)
		})
		if state.Swap != "" && !strings.Contains(read(t, "/proc/swaps"), SwapFile) {
			t.Error("the swap file is not in use after the reboot")
		}
		if state.Zswap {
			for name, want := range map[string]string{"enabled": "Y", "compressor": "zstd", "shrinker_enabled": "Y"} {
				if _, err := os.Stat(path.Join(ZswapDir, name)); err != nil {
					continue
				}
				if got := read(t, path.Join(ZswapDir, name)); got != want {
					t.Errorf("after the reboot zswap %s is %s, want %s", name, got, want)
				}
			}
		}
		txs, err := c.List(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		states := map[string]*helperv1.Transaction{}
		for _, x := range txs {
			states[x.GetId()] = x
		}
		if got := states[state.Swap]; state.Swap != "" && got.GetState() != helperv1.State_STATE_VERIFYING {
			t.Errorf("the swap fix is %v after the reboot", got)
		}
		if got := states[state.Cut]; got.GetState() != helperv1.State_STATE_UNDONE || !strings.Contains(got.GetReason(), "stopped in the middle") {
			t.Errorf("the cut-short limit is %v after the reboot", got)
		}
		if got := read(t, path.Join(cg, "memory.max")); got != "max" {
			t.Errorf("memory.max %s after recovery", got)
		}
		runtimeGone(t, unit)
		if _, err := os.Stat(drop); !errors.Is(err, os.ErrNotExist) {
			t.Errorf("the cut-short drop-in survived the reboot: %v", err)
		}
		if state.Swap == "" {
			return
		}
		if _, err := do(t, c, &helperv1.Request{Kind: &helperv1.Request_Undo{Undo: &helperv1.UndoRequest{Transaction: state.Swap}}}); err != nil {
			t.Fatalf("undo of the swap fix: %v", err)
		}
		if strings.Contains(read(t, "/proc/swaps"), SwapFile) {
			t.Error("swap is still on after the undo")
		}
	default:
		t.Fatalf("SNUGBOX_LIVE_REBOOT is %q, not before or after", phase)
	}
}
