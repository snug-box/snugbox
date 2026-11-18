// Package helper is the engine of snugbox-helper, the only part of Snugbox
// that changes the box. It runs the fix catalogue of blueprint section 3.7
// through the states of section 5.6: preflight, snapshot, apply, then
// verification and watching by snugboxd, with an undo for every step.
//
// Every change is a list of steps whose old values are saved before the
// first one runs. Undoing a step that never ran changes nothing, so a
// transaction cut short by a crash is put back by undoing all of its steps.
package helper

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"time"

	"snugbox.dev/snugbox/agent/internal/docker"
	"snugbox.dev/snugbox/agent/internal/helperapi"
)

// System is everything the helper does to the box. live_linux.go is the real
// one; tests use a fake that keeps a box in memory.
type System interface {
	Now() time.Time
	// ReadFile reads a file without following a link. A missing file gives
	// an error that wraps fs.ErrNotExist.
	ReadFile(path string) ([]byte, error)
	// WriteFile replaces path atomically with a file owned by root, making
	// missing folders with mode 0755.
	WriteFile(path string, data []byte, mode fs.FileMode) error
	// RemoveFile deletes path. A missing file is not an error.
	RemoveFile(path string) error
	// ListDir returns the names in a folder without following a link. A
	// missing folder gives an error that wraps fs.ErrNotExist.
	ListDir(dir string) ([]string, error)
	// RemoveDir deletes dir if it is an empty folder, without following a
	// link, and reports whether it did. A missing folder, or one with
	// anything in it, is left alone and is not an error.
	RemoveDir(dir string) (bool, error)
	// WriteKernel writes a value to a file under /sys.
	WriteKernel(path, value string) error
	// Systemctl runs systemctl and returns its standard output.
	Systemctl(ctx context.Context, args ...string) (string, error)
	// Tool runs mkswap, systemd-detect-virt or systemd-run with stdin as its
	// standard input. A tool that exits with a status other than 0 returns
	// its output together with the error.
	Tool(ctx context.Context, name string, stdin []byte, args ...string) (string, error)
	// CreateSwapFile makes a new file of size bytes with mode 0600 and fails
	// if anything exists at path.
	CreateSwapFile(path string, size uint64) error
	// Disk returns the free bytes and the filesystem type of the folder.
	Disk(dir string) (free uint64, fstype string, err error)
	// Owner returns the owner of a folder without following a link.
	Owner(dir string) (uid, gid int, err error)
	// SaveSnapshot keeps a transaction's snapshot where only root can read
	// it, and LoadSnapshot reads it back.
	SaveSnapshot(tx string, data []byte) error
	LoadSnapshot(tx string) ([]byte, error)
	// Docker is the Docker Engine, or nil when none answers.
	Docker() Docker
	// Kills returns when the kernel's out-of-memory killer ran, as far back
	// as its log goes.
	Kills() ([]time.Time, error)
}

// Docker is the part of the Docker Engine API the helper uses: blueprint
// section 5.4 allows reading containers and changing their memory limits.
type Docker interface {
	List(ctx context.Context) ([]docker.Summary, error)
	Inspect(ctx context.Context, id string) (docker.Container, error)
	UpdateMemory(ctx context.Context, id string, u docker.MemoryUpdate) error
}

// Paths the helper owns.
const (
	StateDir = helperapi.StateDir
	Journal  = helperapi.JournalPath
	UndoDir  = StateDir + "/undo"
	SwapFile = StateDir + "/swapfile"
	// SwapUnit is SwapFile's path escaped the way systemd-escape --path does
	// it, with .swap added.
	SwapUnit = `var-lib-snugbox\x2dhelper-swapfile.swap`
	UnitDir  = "/etc/systemd/system"
	TmpFiles = "/etc/tmpfiles.d/snugbox-zswap.conf"
	ZswapDir = "/sys/module/zswap/parameters"
	CgroupFS = "/sys/fs/cgroup"
	// ControlDir is where systemctl set-property --runtime keeps the
	// settings it makes, until the next reboot. The helper only removes the
	// ones its own calls leave there.
	ControlDir = "/run/systemd/system.control"
)

// WritePaths are the folders the helper's unit must let it write, outside
// its own state folder. Every path a step writes lies under one of them;
// TestWritePathsCoverEveryStep checks that, and TestUnitWritePaths checks
// that the unit's ReadWritePaths= is exactly this list.
var WritePaths = []string{UnitDir, "/etc/tmpfiles.d", ZswapDir, ControlDir}

// ToolError is a tool that ran and exited with a status other than 0.
type ToolError struct {
	Name   string
	Code   int
	Stderr string
}

func (e *ToolError) Error() string {
	if e.Stderr == "" {
		return fmt.Sprintf("%s exited with status %d", e.Name, e.Code)
	}
	return fmt.Sprintf("%s exited with status %d: %s", e.Name, e.Code, e.Stderr)
}

// quietFailure reports whether err is a tool exiting with status 1 and
// nothing on standard error. systemctl list-unit-files answers that way
// when no unit matches; measured on systemd 255, 31 Oct 2026.
func quietFailure(err error) bool {
	var te *ToolError
	return errors.As(err, &te) && te.Code == 1 && te.Stderr == ""
}
