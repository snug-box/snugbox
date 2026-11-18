//go:build linux

package collect

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

// fs.Stat on LiveRoot doesn't open the file, so a FIFO, which an open
// would wait on until a writer came, is only looked at, and readUnit
// passes it by.
func TestLiveRootStatsWithoutOpening(t *testing.T) {
	dir := t.TempDir()
	if err := syscall.Mkfifo(filepath.Join(dir, "fifo.conf"), 0o600); err != nil {
		t.Fatal(err)
	}
	r := newLiveRoot(dir)
	t.Cleanup(func() { _ = r.Close() })
	done := make(chan error, 1)
	go func() {
		info, err := fs.Stat(r, "fifo.conf")
		if err == nil && info.Mode()&fs.ModeNamedPipe == 0 {
			err = fmt.Errorf("fifo.conf has mode %v", info.Mode())
		}
		if _, ok := readUnit(r, "fifo.conf"); ok && err == nil {
			err = errors.New("readUnit read a FIFO")
		}
		done <- err
	}()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("looking at a FIFO waited for a writer")
	}
}

// A kernel file read again from offset 0 is written out afresh: the
// context switch count in /proc/stat moves on between two reads of the one
// open file.
func TestLiveRootReadsKernelFilesAfresh(t *testing.T) {
	r := NewLiveRoot()
	t.Cleanup(func() { _ = r.Close() })
	ctxt := func() string {
		b, err := fs.ReadFile(r, "proc/stat")
		if err != nil {
			t.Fatal(err)
		}
		for line := range strings.Lines(string(b)) {
			if strings.HasPrefix(line, "ctxt ") {
				return strings.TrimSpace(line)
			}
		}
		t.Fatalf("no ctxt line in /proc/stat:\n%s", b)
		return ""
	}
	first := ctxt()
	for deadline := time.Now().Add(5 * time.Second); ctxt() == first; time.Sleep(10 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatalf("/proc/stat read again still says %q", first)
		}
	}
	if n := len(r.files); n != 1 {
		t.Errorf("%d files kept open for one", n)
	}
}

func TestLiveRootKeepsOnlySampleFiles(t *testing.T) {
	dir := t.TempDir()
	for name, body := range map[string]string{
		"proc/meminfo": "MemTotal: 1 kB\n",
		"sys/fs/cgroup/system.slice/a.service/memory.current": "4096\n",
		"sys/fs/cgroup/system.slice/a.service/cgroup.procs":   "1\n",
		"etc/os-release": "ID=ubuntu\n",
	} {
		p := filepath.Join(dir, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	r := newLiveRoot(dir)
	t.Cleanup(func() { _ = r.Close() })
	clock := time.Date(2026, 11, 1, 3, 0, 0, 0, time.UTC)
	r.now = func() time.Time { return clock }
	for _, name := range []string{"proc/meminfo", "sys/fs/cgroup/system.slice/a.service/memory.current", "sys/fs/cgroup/system.slice/a.service/cgroup.procs", "etc/os-release"} {
		if _, err := fs.ReadFile(r, name); err != nil {
			t.Fatalf("%s: %v", name, err)
		}
	}
	if len(r.files) != 2 || r.files["proc/meminfo"] == nil || r.files["sys/fs/cgroup/system.slice/a.service/memory.current"] == nil {
		t.Errorf("kept open %v; want /proc/meminfo and memory.current only", r.files)
	}

	// The same file read again is the same open file, with what it holds now.
	current := filepath.Join(dir, "sys/fs/cgroup/system.slice/a.service/memory.current")
	if err := os.WriteFile(current, []byte("8192\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if b, err := fs.ReadFile(r, "sys/fs/cgroup/system.slice/a.service/memory.current"); err != nil || string(b) != "8192\n" {
		t.Errorf("read again: %q, %v", b, err)
	}

	// A file no one reads for three minutes is closed at the next read.
	clock = clock.Add(liveIdle + time.Minute)
	if _, err := fs.ReadFile(r, "proc/meminfo"); err != nil {
		t.Fatal(err)
	}
	if len(r.files) != 1 || r.files["proc/meminfo"] == nil {
		t.Errorf("after the idle time kept open %v; want /proc/meminfo only", r.files)
	}

	// A file that isn't there is an error that says so, and nothing is kept.
	if _, err := fs.ReadFile(r, "sys/fs/cgroup/system.slice/gone.service/memory.current"); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("a missing file: %v", err)
	}
	if _, err := fs.ReadFile(r, "../etc/passwd"); err == nil {
		t.Error("a path out of the root was read")
	}

	// Past the limit on open files, files are read and closed again.
	r.maxFiles = 1
	if b, err := fs.ReadFile(r, "sys/fs/cgroup/system.slice/a.service/memory.current"); err != nil || string(b) != "8192\n" || len(r.files) != 1 {
		t.Errorf("past the limit: %q, %v, kept open %v", b, err, r.files)
	}
}
