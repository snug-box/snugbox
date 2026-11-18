//go:build linux

package main

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"snugbox.dev/snugbox/agent/internal/helper"
	"snugbox.dev/snugbox/agent/internal/helperapi"
	helperv1 "snugbox.dev/snugbox/gen/snugbox/helper/v1"
)

// exchange runs serveConn for one connection, with snugbox as the watcher's
// user ID, writes what send writes as the client, and returns every frame
// the helper answered with.
func exchange(t *testing.T, snugbox int, handle handler, send func(net.Conn)) []*helperv1.Response {
	t.Helper()
	dir, err := os.MkdirTemp("", "sb")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	ln, err := net.ListenUnix("unix", &net.UnixAddr{Name: filepath.Join(dir, "h.sock"), Net: "unix"})
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	if err := helperapi.PassCredentials(ln); err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	go func() {
		defer close(done)
		c, err := helperapi.Accept(ln)
		if err != nil {
			return
		}
		serveConn(context.Background(), slog.New(slog.DiscardHandler), c, snugbox, handle)
	}()
	conn, err := net.Dial("unix", ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	send(conn)
	var out []*helperv1.Response
	r := bufio.NewReader(conn)
	for {
		resp := &helperv1.Response{}
		if err := helperapi.ReadFrame(r, resp, helperapi.MaxResponse); err != nil {
			break
		}
		out = append(out, resp)
	}
	<-done
	return out
}

func sendList(c net.Conn) {
	_ = helperapi.WriteFrame(c, &helperv1.Request{Kind: &helperv1.Request_List{List: &helperv1.ListRequest{}}}, helperapi.MaxRequest)
}

func mustNotRun(t *testing.T) handler {
	return func(context.Context, *helperv1.Request, helper.Caller, func(*helperv1.Progress)) *helperv1.Response {
		t.Error("the engine ran a request it should never have seen")
		return nil
	}
}

func TestStrangersAreRefused(t *testing.T) {
	if os.Getuid() == 0 {
		t.Skip("as root the test is the helper's own caller")
	}
	got := exchange(t, -1, mustNotRun(t), sendList)
	if len(got) != 1 || got[0].GetError().GetCode() != helperv1.ErrorCode_ERROR_CODE_DENIED {
		t.Fatalf("answers %v, want one DENIED", got)
	}
	if !strings.Contains(got[0].GetError().GetMessage(), "only root and the snugbox user") {
		t.Errorf("message %q", got[0].GetError().GetMessage())
	}
}

func TestTheWatcherIsServed(t *testing.T) {
	if os.Getuid() == 0 {
		t.Skip("as root the caller is root, not the watcher")
	}
	var by helper.Caller
	got := exchange(t, os.Getuid(), func(_ context.Context, req *helperv1.Request, c helper.Caller, progress func(*helperv1.Progress)) *helperv1.Response {
		by = c
		if req.GetList() == nil {
			t.Errorf("request %v", req)
		}
		progress(&helperv1.Progress{State: helperv1.State_STATE_PREFLIGHT, Detail: "reading the journal"})
		return &helperv1.Response{Kind: &helperv1.Response_List{List: &helperv1.ListResponse{}}}
	}, sendList)
	if by != helper.Watcher {
		t.Errorf("caller %v, want the watcher", by)
	}
	if len(got) != 2 || got[0].GetProgress().GetDetail() != "reading the journal" || got[1].GetList() == nil {
		t.Errorf("answers %v, want a progress step and the list", got)
	}
}

func TestABadFrameIsRefused(t *testing.T) {
	if os.Getuid() == 0 {
		t.Skip("needs an ordinary user to be the watcher")
	}
	// A length over the request cap is refused before any body is read.
	got := exchange(t, os.Getuid(), mustNotRun(t), func(c net.Conn) {
		_, _ = c.Write(binary.AppendUvarint(nil, helperapi.MaxRequest+1))
	})
	if len(got) != 1 || got[0].GetError().GetCode() != helperv1.ErrorCode_ERROR_CODE_INVALID {
		t.Fatalf("answers %v, want one INVALID", got)
	}
}

func TestAClientThatLeavesDoesNotStopTheRequest(t *testing.T) {
	if os.Getuid() == 0 {
		t.Skip("needs an ordinary user to be the watcher")
	}
	finished := false
	left := make(chan struct{})
	exchange(t, os.Getuid(), func(_ context.Context, _ *helperv1.Request, _ helper.Caller, progress func(*helperv1.Progress)) *helperv1.Response {
		<-left
		progress(&helperv1.Progress{State: helperv1.State_STATE_APPLYING, Detail: "after the client left"})
		progress(&helperv1.Progress{State: helperv1.State_STATE_VERIFYING, Detail: "still going"})
		finished = true
		return &helperv1.Response{Kind: &helperv1.Response_List{List: &helperv1.ListResponse{}}}
	}, func(c net.Conn) {
		sendList(c)
		_ = c.Close()
		close(left)
	})
	if !finished {
		t.Error("the request stopped when its client left; a fix half done must run to its end")
	}
}

func TestWriteOverride(t *testing.T) {
	dir := t.TempDir()
	var stderr bytes.Buffer
	body := "services:\n  app:\n    mem_limit: 1g\n"
	for _, args := range [][]string{{dir}, {dir, "docker-compose.yml"}, {dir, overrideName, "extra"}} {
		if code := writeOverride(args, strings.NewReader(body), &stderr); code != 2 {
			t.Errorf("writeOverride(%q) = %d, want a usage error", args, code)
		}
	}
	if code := writeOverride([]string{dir, overrideName}, strings.NewReader(strings.Repeat("x", maxOverride+1)), &stderr); code != 1 ||
		!strings.Contains(stderr.String(), "under 64 KiB") {
		t.Errorf("an oversized override: %d, %q", code, stderr.String())
	}
	if code := writeOverride([]string{dir, overrideName}, strings.NewReader(body), &stderr); code != 0 {
		t.Fatalf("writeOverride = %d: %s", code, stderr.String())
	}
	path := filepath.Join(dir, overrideName)
	got, err := os.ReadFile(path)
	if err != nil || string(got) != body {
		t.Fatalf("wrote %q, %v", got, err)
	}
	if fi, _ := os.Stat(path); fi.Mode().Perm()&0o022 != 0 {
		t.Errorf("mode %v lets others write it", fi.Mode())
	}
	// An existing file is never replaced.
	if code := writeOverride([]string{dir, overrideName}, strings.NewReader("services: {}\n"), &stderr); code != 1 {
		t.Errorf("a second write = %d, want a refusal", code)
	}
	if got, _ := os.ReadFile(path); string(got) != body {
		t.Errorf("the existing override was changed to %q", got)
	}
}

func TestWriteOverrideRefusesLinks(t *testing.T) {
	project := t.TempDir()
	linked := filepath.Join(t.TempDir(), "project")
	if err := os.Symlink(project, linked); err != nil {
		t.Fatal(err)
	}
	var stderr bytes.Buffer
	if code := writeOverride([]string{linked, overrideName}, strings.NewReader("services: {}\n"), &stderr); code != 1 {
		t.Errorf("a linked folder = %d, want a refusal", code)
	}
	target := filepath.Join(t.TempDir(), "elsewhere")
	if err := os.Symlink(target, filepath.Join(project, overrideName)); err != nil {
		t.Fatal(err)
	}
	if code := writeOverride([]string{project, overrideName}, strings.NewReader("services: {}\n"), &stderr); code != 1 {
		t.Errorf("a link at the file's name = %d, want a refusal", code)
	}
	if _, err := os.Stat(target); !os.IsNotExist(err) {
		t.Errorf("the override was written through a link: %v", err)
	}
}

func TestRemoveOverride(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, overrideName)
	body := []byte("services:\n  app:\n    mem_limit: 1g\n")
	sum := sha256.Sum256(body)
	hash := hex.EncodeToString(sum[:])
	var stdout, stderr bytes.Buffer

	for _, args := range [][]string{{dir, overrideName}, {dir, "compose.yaml", hash}, {dir, overrideName, "abc"}} {
		if code := removeOverride(args, &stdout, &stderr); code != 2 {
			t.Errorf("removeOverride(%q) = %d, want a usage error", args, code)
		}
	}
	if code := removeOverride([]string{filepath.Join(dir, "gone"), overrideName, hash}, &stdout, &stderr); code != 0 {
		t.Errorf("a folder that is gone = %d, want nothing to do", code)
	}
	if code := removeOverride([]string{dir, overrideName, hash}, &stdout, &stderr); code != 0 {
		t.Errorf("no override = %d, want nothing to do", code)
	}

	// Edited since Snugbox wrote it: left alone, with a note.
	if err := os.WriteFile(path, append(body, "    cpus: 2\n"...), 0o644); err != nil {
		t.Fatal(err)
	}
	if code := removeOverride([]string{dir, overrideName, hash}, &stdout, &stderr); code != 0 ||
		!strings.Contains(stdout.String(), "was changed after Snugbox wrote it, so it was left as it is.") {
		t.Errorf("an edited override: %d, %q", code, stdout.String())
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("an edited override was removed: %v", err)
	}

	if err := os.WriteFile(path, body, 0o644); err != nil {
		t.Fatal(err)
	}
	if code := removeOverride([]string{dir, overrideName, hash}, &stdout, &stderr); code != 0 {
		t.Fatalf("removeOverride = %d: %s", code, stderr.String())
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Errorf("the override Snugbox wrote is still there: %v", err)
	}
}
