//go:build linux

package helperapi

import (
	"bytes"
	"context"
	"errors"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"golang.org/x/sys/unix"

	helperv1 "snugbox.dev/snugbox/gen/snugbox/helper/v1"
)

// listen starts a test listener with SO_PASSCRED, as systemd sets it up.
func listen(t *testing.T) (*net.UnixListener, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "helper.sock")
	ln, err := net.ListenUnix("unix", &net.UnixAddr{Name: path, Net: "unix"})
	if err != nil {
		t.Fatal(err)
	}
	if err := PassCredentials(ln); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	return ln, path
}

type served struct {
	req  *helperv1.Request
	peer Peer
	err  error
}

// serveOne accepts one connection, reads one request and answers it with a
// progress step and an empty list.
func serveOne(ln *net.UnixListener) <-chan served {
	out := make(chan served, 1)
	go func() {
		c, err := Accept(ln)
		if err != nil {
			out <- served{err: err}
			return
		}
		defer c.Close()
		var req helperv1.Request
		err = ReadFrame(c.Reader(), &req, MaxRequest)
		if err == nil {
			_ = WriteFrame(c, &helperv1.Response{Kind: &helperv1.Response_Progress{Progress: &helperv1.Progress{State: helperv1.State_STATE_PREFLIGHT, Detail: "checking"}}}, MaxResponse)
			_ = WriteFrame(c, &helperv1.Response{Kind: &helperv1.Response_List{List: &helperv1.ListResponse{}}}, MaxResponse)
		}
		out <- served{req: &req, peer: c.Peer, err: err}
	}()
	return out
}

func TestRequestOverTheSocket(t *testing.T) {
	ln, path := listen(t)
	got := serveOne(ln)
	c := &Client{Socket: path, ServerUID: os.Getuid()}
	var steps []string
	resp, err := c.Do(context.Background(), &helperv1.Request{Kind: &helperv1.Request_List{List: &helperv1.ListRequest{}}}, func(p *helperv1.Progress) {
		steps = append(steps, p.GetDetail())
	})
	if err != nil {
		t.Fatal(err)
	}
	if resp.GetList() == nil || len(steps) != 1 || steps[0] != "checking" {
		t.Errorf("answer %v, steps %q", resp, steps)
	}
	s := <-got
	if s.err != nil || s.req.GetList() == nil {
		t.Fatalf("the server read %v, %v", s.req, s.err)
	}
	if s.peer.UID != os.Getuid() || s.peer.PID != os.Getpid() {
		t.Errorf("peer %+v, want uid %d pid %d", s.peer, os.Getuid(), os.Getpid())
	}
}

func TestClientRefusesAStrangeServer(t *testing.T) {
	ln, path := listen(t)
	go func() {
		if c, err := Accept(ln); err == nil {
			c.Close()
		}
	}()
	c := &Client{Socket: path, ServerUID: os.Getuid() + 1}
	_, err := c.Do(context.Background(), &helperv1.Request{Kind: &helperv1.Request_List{List: &helperv1.ListRequest{}}}, nil)
	if err == nil || !strings.Contains(err.Error(), "not the helper") {
		t.Errorf("Do = %v, want a refusal to talk to a socket another user holds", err)
	}
}

func TestNoHelper(t *testing.T) {
	c := &Client{Socket: filepath.Join(t.TempDir(), "missing.sock")}
	if _, err := c.List(context.Background()); !errors.Is(err, ErrNoHelper) {
		t.Errorf("List = %v, want ErrNoHelper", err)
	}
}

func dialRaw(t *testing.T, path string) *net.UnixConn {
	t.Helper()
	c, err := net.DialUnix("unix", nil, &net.UnixAddr{Name: path, Net: "unix"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { c.Close() })
	return c
}

func frameBytes(t *testing.T) []byte {
	t.Helper()
	var buf bytes.Buffer
	if err := WriteFrame(&buf, &helperv1.Request{Kind: &helperv1.Request_List{List: &helperv1.ListRequest{}}}, MaxRequest); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func openFDs(t *testing.T) int {
	t.Helper()
	ents, err := os.ReadDir("/proc/self/fd")
	if err != nil {
		t.Fatal(err)
	}
	return len(ents)
}

func TestFileDescriptorsAreRefused(t *testing.T) {
	ln, path := listen(t)
	got := serveOne(ln)
	conn := dialRaw(t, path)
	f, err := os.Open("/proc/self/status")
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	before := openFDs(t)
	if _, _, err := conn.WriteMsgUnix(frameBytes(t), unix.UnixRights(int(f.Fd()), int(f.Fd())), nil); err != nil {
		t.Fatal(err)
	}
	s := <-got
	if !errors.Is(s.err, ErrFDs) {
		t.Fatalf("the server read %v, %v; want ErrFDs", s.req, s.err)
	}
	// The two descriptors the kernel installed in this process on receipt
	// are closed again. The server's own connection is closed by now too.
	if after := openFDs(t); after > before {
		t.Errorf("%d descriptors open after the refusal, %d before", after, before)
	}
}

func TestOwnCredentialsAreAccepted(t *testing.T) {
	ln, path := listen(t)
	got := serveOne(ln)
	conn := dialRaw(t, path)
	cred := unix.UnixCredentials(&unix.Ucred{Pid: int32(os.Getpid()), Uid: uint32(os.Getuid()), Gid: uint32(os.Getgid())})
	if _, _, err := conn.WriteMsgUnix(frameBytes(t), cred, nil); err != nil {
		t.Fatal(err)
	}
	if s := <-got; s.err != nil {
		t.Errorf("a message with the sender's own credentials was refused: %v", s.err)
	}
}

// TestAnotherSenderIsRefused hands a connection root opened to a process
// running as nobody, which then writes the request. SO_PEERCRED still says
// root; the credentials on the message say nobody, and the helper believes
// the message.
func TestAnotherSenderIsRefused(t *testing.T) {
	if os.Getuid() != 0 {
		t.Skip("needs root to start a process as another user")
	}
	ln, path := listen(t)
	got := serveOne(ln)
	conn := dialRaw(t, path)
	f, err := conn.File()
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	cmd := exec.Command(copyForNobody(t), "-test.run=^TestChildWritesARequest$")
	cmd.Env = append(os.Environ(), "SNUGBOX_TEST_CHILD=write")
	cmd.ExtraFiles = []*os.File{f}
	cmd.SysProcAttr = &syscall.SysProcAttr{Credential: &syscall.Credential{Uid: 65534, Gid: 65534}}
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("child: %v: %s", err, out)
	}
	if s := <-got; !errors.Is(s.err, ErrSender) {
		t.Errorf("the server read %v, %v; want ErrSender", s.req, s.err)
	}
}

// copyForNobody copies the test binary where nobody can run it: go test
// builds it in a folder only its owner can open.
func copyForNobody(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp("", "snugbox-helperapi-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	if err := os.Chmod(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(os.Args[0])
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "helperapi.test")
	if err := os.WriteFile(path, b, 0o755); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestChildWritesARequest(t *testing.T) {
	if os.Getenv("SNUGBOX_TEST_CHILD") != "write" {
		t.Skip("runs only as the child of TestAnotherSenderIsRefused")
	}
	f := os.NewFile(3, "conn")
	if _, err := f.Write(frameBytes(t)); err != nil {
		t.Fatal(err)
	}
}

func TestActivationListener(t *testing.T) {
	ln, path := listen(t)
	f, err := ln.File()
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	cmd := exec.Command(os.Args[0], "-test.run=^TestChildServesFromFD3$")
	cmd.Env = append(os.Environ(), "SNUGBOX_TEST_CHILD=activate", "LISTEN_FDS=1")
	cmd.ExtraFiles = []*os.File{f}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	c := &Client{Socket: path, ServerUID: os.Getuid()}
	if _, err := c.List(ctx); err != nil {
		t.Errorf("List through the activated child: %v", err)
	}
	if err := cmd.Wait(); err != nil {
		t.Errorf("child: %v", err)
	}
}

func TestChildServesFromFD3(t *testing.T) {
	if os.Getenv("SNUGBOX_TEST_CHILD") != "activate" {
		t.Skip("runs only as the child of TestActivationListener")
	}
	if err := os.Setenv("LISTEN_PID", strconv.Itoa(os.Getpid())); err != nil {
		t.Fatal(err)
	}
	ln, err := ActivationListener()
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	if os.Getenv("LISTEN_FDS") != "" {
		t.Error("LISTEN_FDS is still set, so a child of the helper would think it was activated too")
	}
	if s := <-serveOne(ln); s.err != nil {
		t.Fatal(s.err)
	}
}

func TestActivationNeedsSystemd(t *testing.T) {
	t.Setenv("LISTEN_PID", "1")
	t.Setenv("LISTEN_FDS", "1")
	if _, err := ActivationListener(); err == nil {
		t.Error("accepted sockets meant for another process")
	}
}

// The reader passes on what it was given, chunk by chunk, so a request split
// over several writes is still read whole.
func TestSplitWrites(t *testing.T) {
	ln, path := listen(t)
	got := serveOne(ln)
	conn := dialRaw(t, path)
	b := frameBytes(t)
	for i := range b {
		if _, err := conn.Write(b[i : i+1]); err != nil {
			t.Fatal(err)
		}
	}
	if s := <-got; s.err != nil {
		t.Errorf("a request written a byte at a time: %v", s.err)
	}
}
