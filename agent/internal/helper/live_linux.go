//go:build linux

package helper

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"math"
	"os"
	"os/exec"
	"path"
	"strings"
	"time"

	"golang.org/x/sys/unix"

	"snugbox.dev/snugbox/agent/internal/collect"
	"snugbox.dev/snugbox/agent/internal/docker"
	"snugbox.dev/snugbox/agent/internal/journal"
	"snugbox.dev/snugbox/agent/internal/kernel"
	"snugbox.dev/snugbox/agent/internal/safefs"
)

const (
	maxFile      = 1 << 20
	maxSnapshot  = 16 << 20
	toolTimeout  = 3 * time.Minute
	maxToolBytes = 1 << 20
)

// Live is the box the helper runs on.
type Live struct {
	docker *docker.Client
}

// NewLive returns the real box. Docker is used only when its socket exists.
func NewLive() *Live {
	l := &Live{}
	if _, err := os.Stat(docker.DefaultSocket); err == nil {
		l.docker = docker.New(docker.DefaultSocket)
	}
	return l
}

// Now returns the time.
func (*Live) Now() time.Time { return time.Now() }

// kernelFile splits a path under /proc or /sys into the filesystem's root and
// the name inside it, which os.Root then keeps from leading anywhere else.
func kernelFile(p string) (root, name string, ok bool) {
	for _, r := range []string{"/proc", "/sys"} {
		if rest, found := strings.CutPrefix(p, r+"/"); found && rest != "" && path.Clean(rest) == rest && !strings.HasPrefix(rest, "../") {
			return r, rest, true
		}
	}
	return "", "", false
}

func openKernel(root, name string, flag int) (*os.File, error) {
	r, err := os.OpenRoot(root)
	if err != nil {
		return nil, err
	}
	f, err := r.OpenFile(name, flag, 0)
	return f, errors.Join(err, r.Close())
}

// ReadFile reads a file without following a link at any step. Kernel files
// report no size, so /proc and /sys are read up to a limit instead.
func (*Live) ReadFile(p string) ([]byte, error) {
	if !path.IsAbs(p) {
		return nil, fmt.Errorf("%q is not an absolute path", p)
	}
	if root, name, ok := kernelFile(p); ok {
		f, err := openKernel(root, name, os.O_RDONLY)
		if err != nil {
			return nil, err
		}
		defer f.Close()
		return io.ReadAll(io.LimitReader(f, maxFile))
	}
	dir, err := safefs.OpenDir(path.Dir(p), false, 0)
	if err != nil {
		return nil, err
	}
	defer dir.Close()
	return safefs.Read(dir, path.Base(p), maxFile)
}

// WriteFile replaces p atomically with a file owned by root.
func (*Live) WriteFile(p string, data []byte, mode fs.FileMode) error {
	dir, err := safefs.OpenDir(path.Dir(p), true, 0o755)
	if err != nil {
		return err
	}
	return errors.Join(safefs.Replace(dir, path.Base(p), data, mode, safefs.Owner{UID: 0, GID: 0}), dir.Close())
}

// RemoveFile deletes p; a missing file or folder is not an error.
func (*Live) RemoveFile(p string) error {
	dir, err := safefs.OpenDir(path.Dir(p), false, 0)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	return errors.Join(safefs.Remove(dir, path.Base(p)), dir.Close())
}

// ListDir returns the names in dir, refusing a link at any step.
func (*Live) ListDir(dir string) ([]string, error) {
	d, err := safefs.OpenDir(dir, false, 0)
	if err != nil {
		return nil, err
	}
	names, err := d.Readdirnames(-1)
	return names, errors.Join(err, d.Close())
}

// RemoveDir deletes dir when it is an empty folder; a missing parent is not
// an error either.
func (*Live) RemoveDir(dir string) (bool, error) {
	parent, err := safefs.OpenDir(path.Dir(dir), false, 0)
	if errors.Is(err, fs.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	removed, err := safefs.RemoveDir(parent, path.Base(dir))
	return removed, errors.Join(err, parent.Close())
}

// WriteKernel writes a kernel parameter under /sys.
func (*Live) WriteKernel(p, value string) error {
	root, name, ok := kernelFile(p)
	if !ok || root != "/sys" {
		return fmt.Errorf("%s is not a kernel parameter", p)
	}
	f, err := openKernel(root, name, os.O_WRONLY)
	if err != nil {
		return err
	}
	_, err = f.WriteString(value)
	return errors.Join(err, f.Close())
}

// tools are the only programs the helper runs, each looked up only in the
// system folders so nothing earlier in a PATH can stand in for it. btrfs
// makes swap files on Btrfs, through its filesystem mkswapfile command only.
var tools = map[string]bool{"systemctl": true, "mkswap": true, "systemd-detect-virt": true, "systemd-run": true, "btrfs": true}

func toolPath(name string) (string, error) {
	if !tools[name] {
		return "", fmt.Errorf("%s is not a tool the helper runs", name)
	}
	for _, dir := range []string{"/usr/bin", "/usr/sbin", "/bin", "/sbin"} {
		p := path.Join(dir, name)
		if fi, err := os.Stat(p); err == nil && fi.Mode().IsRegular() {
			return p, nil
		}
	}
	return "", fmt.Errorf("%s is not installed", name)
}

// limited keeps the first n bytes written to it and drops the rest.
type limited struct {
	buf bytes.Buffer
	n   int
}

func (l *limited) Write(p []byte) (int, error) {
	if room := l.n - l.buf.Len(); room > 0 {
		l.buf.Write(p[:min(len(p), room)])
	}
	return len(p), nil
}

// Tool runs one of the helper's tools with a fixed environment.
func (*Live) Tool(ctx context.Context, name string, stdin []byte, args ...string) (string, error) {
	bin, err := toolPath(name)
	if err != nil {
		return "", err
	}
	ctx, cancel := context.WithTimeout(ctx, toolTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, bin, args...) //nolint:gosec // bin is one of the five system tools above, found in system folders
	// TZ=UTC makes systemctl print every time in UTC, whatever the box's
	// time zone; its --timestamp=utc leaves some properties in local time.
	cmd.Env = []string{"PATH=/usr/sbin:/usr/bin:/sbin:/bin", "LC_ALL=C", "TZ=UTC", "SYSTEMD_COLORS=0", "SYSTEMD_PAGER="}
	cmd.Dir = "/"
	if stdin != nil {
		cmd.Stdin = bytes.NewReader(stdin)
	}
	out, errOut := &limited{n: maxToolBytes}, &limited{n: 4096}
	cmd.Stdout, cmd.Stderr = out, errOut
	if err := cmd.Run(); err != nil {
		var exit *exec.ExitError
		if errors.As(err, &exit) {
			return out.buf.String(), &ToolError{Name: name, Code: exit.ExitCode(), Stderr: strings.TrimSpace(errOut.buf.String())}
		}
		return out.buf.String(), fmt.Errorf("%s: %w", name, err)
	}
	return out.buf.String(), nil
}

// Systemctl runs systemctl.
func (l *Live) Systemctl(ctx context.Context, args ...string) (string, error) {
	return l.Tool(ctx, "systemctl", nil, append([]string{"--no-pager", "--no-ask-password"}, args...)...)
}

// CreateSwapFile makes the swap file with mode 0600, owned by root.
func (*Live) CreateSwapFile(p string, size uint64) error {
	if size > math.MaxInt64 {
		return fmt.Errorf("a swap file of %d bytes is too large", size)
	}
	dir, err := safefs.OpenDir(path.Dir(p), false, 0)
	if err != nil {
		return err
	}
	return errors.Join(safefs.CreateSized(dir, path.Base(p), int64(size), 0o600), dir.Close())
}

// Filesystem magic numbers from statfs(2).
var fsTypes = map[int64]string{0xEF53: "ext4", 0x58465342: "xfs", 0x9123683E: "btrfs", 0x2FC12FC1: "zfs", 0x01021994: "tmpfs", 0x794C7630: "overlay"}

// Disk measures the filesystem that holds dir.
func (*Live) Disk(dir string) (uint64, string, error) {
	var st unix.Statfs_t
	if err := unix.Statfs(dir, &st); err != nil {
		return 0, "", err
	}
	name, ok := fsTypes[int64(st.Type)] //nolint:unconvert // the field's type differs between architectures
	if !ok {
		name = fmt.Sprintf("filesystem type %#x", st.Type)
	}
	return st.Bavail * uint64(st.Bsize), name, nil //nolint:gosec // block sizes are small and positive
}

// Owner returns who owns dir, refusing a link at any step.
func (*Live) Owner(dir string) (int, int, error) {
	d, err := safefs.OpenDir(dir, false, 0)
	if err != nil {
		return 0, 0, err
	}
	defer d.Close()
	var st unix.Stat_t
	if err := unix.Fstat(int(d.Fd()), &st); err != nil {
		return 0, 0, err
	}
	return int(st.Uid), int(st.Gid), nil
}

// SaveSnapshot writes undo/<tx>/manifest.json, readable only by root.
func (*Live) SaveSnapshot(tx string, data []byte) error {
	d, err := safefs.OpenDir(path.Join(UndoDir, tx), true, 0o700)
	if err != nil {
		return err
	}
	return errors.Join(safefs.CreateNew(d, "manifest.json", data, 0o600), d.Close())
}

// LoadSnapshot reads undo/<tx>/manifest.json.
func (*Live) LoadSnapshot(tx string) ([]byte, error) {
	d, err := safefs.OpenDir(path.Join(UndoDir, tx), false, 0)
	if err != nil {
		return nil, err
	}
	defer d.Close()
	return safefs.Read(d, "manifest.json", maxSnapshot)
}

// Docker returns the engine client, or nil without Docker.
func (l *Live) Docker() Docker {
	if l.docker == nil {
		return nil
	}
	return l.docker
}

// Kills reads the kernel log for out-of-memory kills.
func (l *Live) Kills() ([]time.Time, error) {
	raw, err := l.ReadFile("/proc/stat")
	if err != nil {
		return nil, err
	}
	ps, err := kernel.ParseProcStat(raw)
	if err != nil {
		return nil, err
	}
	log, err := collect.ReadKernelLog(ps.BootTime)
	if err != nil {
		return nil, err
	}
	return log.KillTimes(), nil
}

// OpenJournal opens the journal in the helper's state folder, making both
// if needed, and verifies it.
func OpenJournal() (*journal.Journal, int, error) {
	d, err := safefs.OpenDir(StateDir, true, 0o700)
	if err != nil {
		return nil, 0, err
	}
	defer d.Close()
	f, err := safefs.OpenAppend(d, path.Base(Journal), 0o600)
	if err != nil {
		return nil, 0, err
	}
	j, torn, err := journal.Open(f, time.Now)
	if err != nil {
		return nil, 0, errors.Join(err, f.Close())
	}
	return j, torn, nil
}
