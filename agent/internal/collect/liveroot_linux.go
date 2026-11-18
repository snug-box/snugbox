//go:build linux

package collect

import (
	"errors"
	"io/fs"
	"os"
	"path"
	"slices"
	"strings"
	"sync"
	"syscall"
	"time"
)

// LiveRoot is the live root filesystem, for code that reads the same kernel
// files over and over: the watcher samples a few hundred every 10 seconds.
// Opening a file cost more than reading it. On a 1 GiB VPS with 50 services,
// openat, Go registering each file with its poller, and close took about 40%
// of snugboxd's CPU (perf, 1 November 2026). LiveRoot keeps open the
// per-sample files: the memory and cpu files of each cgroup, and
// /proc/meminfo, /proc/vmstat, /proc/stat and /proc/pressure/memory. It
// reads each one again from offset 0, which makes the kernel write it out
// afresh. A file whose cgroup was removed fails the read and is opened again,
// so a cgroup made again under the same name is read from its new files.
// Everything else goes through os.DirFS("/").
//
// Only kernel files can be read this way: an ordinary file replaced by a
// rename would still be read from the old one.
type LiveRoot struct {
	fs.FS
	dir string

	mu       sync.Mutex
	files    map[string]*openFile
	buf      []byte
	pruned   time.Time
	now      func() time.Time
	maxFiles int
}

type openFile struct {
	fd   int
	used time.Time
}

// What LiveRoot keeps open, and for how long a file no one reads stays open.
const (
	liveMaxFiles = 4096
	liveIdle     = 3 * time.Minute
)

// sampleFiles are the cgroup files sampled every few seconds.
var sampleFiles = map[string]bool{
	"memory.current": true, "memory.stat": true, "memory.events": true, "memory.pressure": true,
	"memory.peak": true, "memory.swap.current": true, "cpu.stat": true,
}

// boxFiles are the /proc files sampled every few seconds.
var boxFiles = map[string]bool{"proc/meminfo": true, "proc/vmstat": true, "proc/stat": true, "proc/pressure/memory": true}

// NewLiveRoot returns the live root filesystem.
func NewLiveRoot() *LiveRoot { return newLiveRoot("/") }

func newLiveRoot(dir string) *LiveRoot {
	return &LiveRoot{FS: os.DirFS(dir), dir: dir, files: map[string]*openFile{}, buf: make([]byte, 4096), now: time.Now, maxFiles: liveMaxFiles}
}

func kept(name string) bool {
	return boxFiles[name] || (strings.HasPrefix(name, "sys/fs/cgroup/") && sampleFiles[path.Base(name)])
}

// Stat makes LiveRoot an fs.StatFS, so fs.Stat looks at a file without
// opening it: opening a socket fails, and opening a FIFO waits for a writer.
func (r *LiveRoot) Stat(name string) (fs.FileInfo, error) { return fs.Stat(r.FS, name) }

// ReadFile reads a file. It makes LiveRoot an fs.ReadFileFS, so fs.ReadFile
// comes here.
func (r *LiveRoot) ReadFile(name string) ([]byte, error) {
	if !fs.ValidPath(name) {
		return nil, &fs.PathError{Op: "readfile", Path: name, Err: fs.ErrInvalid}
	}
	if !kept(name) {
		return fs.ReadFile(r.FS, name)
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	now := r.now()
	r.prune(now)
	if f, ok := r.files[name]; ok {
		f.used = now
		if data, err := r.pread(f.fd); err == nil {
			return data, nil
		}
		// Gone, or made again under the same name: open it afresh.
		_ = syscall.Close(f.fd)
		delete(r.files, name)
	}
	fd, err := syscall.Open(path.Join(r.dir, name), syscall.O_RDONLY|syscall.O_CLOEXEC|syscall.O_NOFOLLOW, 0)
	if err != nil {
		return nil, &fs.PathError{Op: "open", Path: name, Err: err}
	}
	data, err := r.pread(fd)
	if err != nil || len(r.files) >= r.maxFiles {
		_ = syscall.Close(fd)
		if err != nil {
			return nil, &fs.PathError{Op: "read", Path: name, Err: err}
		}
		return data, nil
	}
	r.files[name] = &openFile{fd: fd, used: now}
	return data, nil
}

// pread reads a whole file from offset 0 into a copy, growing the shared
// buffer until the file fits.
func (r *LiveRoot) pread(fd int) ([]byte, error) {
	for {
		n, err := syscall.Pread(fd, r.buf, 0)
		if errors.Is(err, syscall.EINTR) {
			continue
		}
		if err != nil {
			return nil, err
		}
		if n < len(r.buf) {
			return slices.Clone(r.buf[:n]), nil
		}
		r.buf = make([]byte, 2*len(r.buf))
	}
}

// prune closes, once a minute, the files no one has read for liveIdle, such
// as those of workloads that have gone.
func (r *LiveRoot) prune(now time.Time) {
	if now.Sub(r.pruned) < time.Minute {
		return
	}
	r.pruned = now
	for name, f := range r.files {
		if now.Sub(f.used) > liveIdle {
			_ = syscall.Close(f.fd)
			delete(r.files, name)
		}
	}
}

// Close closes every file LiveRoot keeps open.
func (r *LiveRoot) Close() error {
	r.mu.Lock()
	defer r.mu.Unlock()
	for name, f := range r.files {
		_ = syscall.Close(f.fd)
		delete(r.files, name)
	}
	return nil
}
