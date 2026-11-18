package helper

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"math"
	"path"
	"strings"

	"snugbox.dev/snugbox/agent/internal/docker"
	"snugbox.dev/snugbox/agent/internal/helperapi"
	"snugbox.dev/snugbox/agent/internal/kernel"
)

// Op is what a step does.
type Op string

// The steps fixes are made of.
const (
	// OpFile writes a file owned by root.
	OpFile Op = "file"
	// OpKernel writes a kernel parameter under /sys and reads it back.
	OpKernel Op = "kernel"
	// OpReload runs systemctl daemon-reload.
	OpReload Op = "reload"
	// OpEnable runs systemctl enable --now.
	OpEnable Op = "enable"
	// OpRestart runs systemctl restart.
	OpRestart Op = "restart"
	// OpDocker changes a container's memory limits.
	OpDocker Op = "docker"
	// OpOverride writes a Compose override file as the project folder's
	// owner.
	OpOverride Op = "override"
	// OpSwapFile makes and formats the swap file.
	OpSwapFile Op = "swapfile"
	// OpLimit puts a service's MemoryHigh and MemoryMax on its running
	// cgroup when the reload before it didn't, as from systemd 257.
	OpLimit Op = "limit"
)

// Step is one change and what it replaces, saved in the snapshot before any
// step runs.
type Step struct {
	Op     Op     `json:"op"`
	Target string `json:"target"`
	// Label names the target for people, such as a container's name.
	Label string `json:"label,omitempty"`
	// Old and New are file contents for OpFile and OpOverride, and values
	// for OpKernel.
	Old string `json:"old,omitempty"`
	New string `json:"new,omitempty"`
	// Existed says the file existed before, or for OpEnable that the unit
	// was enabled before.
	Existed bool `json:"existed,omitempty"`
	// Dir is the folder of an OpFile drop-in that undo takes away once
	// nothing is left in it: one the fix made, or one that held nothing but
	// Snugbox's own drop-ins. A folder a person made stays.
	Dir string `json:"dir,omitempty"`
	// Active says the unit was running before.
	Active bool        `json:"active,omitempty"`
	Mode   fs.FileMode `json:"mode,omitempty"`
	// UID and GID own an OpOverride file.
	UID int `json:"uid,omitempty"`
	GID int `json:"gid,omitempty"`
	// OldMemory and NewMemory are an OpDocker step's limits.
	OldMemory docker.MemoryUpdate `json:"old_memory,omitzero"`
	NewMemory docker.MemoryUpdate `json:"new_memory,omitzero"`
	// High and Max are the MemoryHigh and MemoryMax an OpFile drop-in of the
	// service limits fix sets, which confirm reads back from the cgroup, and
	// the values an OpLimit step puts on the unit named in Target.
	High uint64 `json:"high,omitempty"`
	Max  uint64 `json:"max,omitempty"`
	// Calendar is the OnCalendar an OpFile drop-in of the schedule fix sets
	// on the timer named in Label, which confirm reads back from systemd,
	// and Minutes how much earlier that is than the timer's own.
	Calendar string `json:"calendar,omitempty"`
	Minutes  int    `json:"minutes,omitempty"`
	// Size is the OpSwapFile size in bytes, and FS the filesystem it goes on.
	Size uint64 `json:"size,omitempty"`
	FS   string `json:"fs,omitempty"`
}

// Docker can't clear a memory limit: docker update reads 0 as "leave it" and
// refuses anything below 6 MiB. Undo writes docker.NoLimit instead, which
// the kernel reads as no limit at all, and the smallest soft limit Docker
// takes.
const (
	noDockerLimit      = docker.NoLimit
	minDockerSoftLimit = 6 << 20
)

// errChanged means something other than Snugbox changed a target between
// the check and the change.
var errChanged = errors.New("changed by something else since Snugbox checked it")

// bytesOf turns a Docker byte count, which is signed, into Snugbox's
// unsigned one; a count below zero, Docker's "unlimited", is zero.
func bytesOf(n int64) uint64 {
	if n <= 0 {
		return 0
	}
	return uint64(n)
}

// dockerBytes turns a byte count the helper's bounds keep far below 8 EiB
// into Docker's signed one.
func dockerBytes(n uint64) int64 {
	if n > math.MaxInt64 {
		return math.MaxInt64
	}
	return int64(n)
}

// readOptional reads a file that may not exist.
func readOptional(sys System, path string) (string, bool, error) {
	b, err := sys.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return "", false, nil
	}
	if err != nil {
		return "", false, err
	}
	return string(b), true, nil
}

// do runs the step. It is only called after the snapshot is on disk.
func (s *Step) do(ctx context.Context, sys System, exe string) error {
	switch s.Op {
	case OpFile:
		cur, exists, err := readOptional(sys, s.Target)
		if err != nil {
			return err
		}
		if exists != s.Existed || cur != s.Old {
			return fmt.Errorf("%s: %w", s.Target, errChanged)
		}
		return sys.WriteFile(s.Target, []byte(s.New), s.Mode)
	case OpKernel:
		if err := sys.WriteKernel(s.Target, s.New); err != nil {
			return err
		}
		return checkKernel(sys, s.Target, s.New)
	case OpReload:
		_, err := sys.Systemctl(ctx, "daemon-reload")
		return err
	case OpEnable:
		_, err := sys.Systemctl(ctx, "enable", "--now", "--", s.Target)
		return err
	case OpRestart:
		_, err := sys.Systemctl(ctx, "restart", "--", s.Target)
		return err
	case OpDocker:
		return updateContainer(ctx, sys, s.Target, s.NewMemory)
	case OpOverride:
		_, err := sys.Tool(ctx, "systemd-run", []byte(s.New), overrideArgs(s, exe, "write-override")...)
		return err
	case OpSwapFile:
		if s.FS == "btrfs" {
			// A Btrfs swap file must have copy-on-write and compression off and
			// no holes, which btrfs-progs 6.1 and newer arrange in one step
			// that also writes the swap header.
			_, err := sys.Tool(ctx, "btrfs", nil, "filesystem", "mkswapfile", "--size", fmt.Sprintf("%dm", s.Size/helperapi.MiB), "--", s.Target)
			return err
		}
		if err := sys.CreateSwapFile(s.Target, s.Size); err != nil {
			return err
		}
		_, err := sys.Tool(ctx, "mkswap", nil, "--label", "snugbox", "--", s.Target)
		return err
	case OpLimit:
		_, err := setLive(ctx, sys, s.Target, s.limits())
		return err
	}
	return fmt.Errorf("unknown step %q", s.Op)
}

// undo puts the step back. It is safe to run for a step that never ran, or
// ran already. A target someone changed after Snugbox is left as it is, and
// the returned note says so.
func (s *Step) undo(ctx context.Context, sys System, exe string) (string, error) {
	switch s.Op {
	case OpFile:
		cur, exists, err := readOptional(sys, s.Target)
		switch {
		case err != nil:
			return "", err
		case !exists && !s.Existed:
			return "", s.removeDir(sys)
		case exists && s.Existed && cur == s.Old:
			return "", nil
		case exists && cur == s.New && s.Existed:
			return "", sys.WriteFile(s.Target, []byte(s.Old), s.Mode)
		case exists && cur == s.New:
			if err := sys.RemoveFile(s.Target); err != nil {
				return "", err
			}
			return "", s.removeDir(sys)
		}
		return fmt.Sprintf("%s was changed after Snugbox wrote it, so it was left as it is.", s.Target), nil
	case OpKernel:
		if err := sys.WriteKernel(s.Target, s.Old); err != nil {
			return "", err
		}
		return "", checkKernel(sys, s.Target, s.Old)
	case OpReload, OpRestart:
		// Undo reloads and restarts once, after every other step is back.
		return "", nil
	case OpEnable:
		props, err := showUnit(ctx, sys, s.Target, "LoadState", "UnitFileState", "ActiveState")
		if err != nil {
			return "", err
		}
		if first(props, "LoadState") == "not-found" {
			return "", nil
		}
		if !s.Existed && first(props, "UnitFileState") == "enabled" {
			if _, err := sys.Systemctl(ctx, "disable", "--", s.Target); err != nil {
				return "", err
			}
		}
		if !s.Active && first(props, "ActiveState") != "inactive" {
			if _, err := sys.Systemctl(ctx, "stop", "--", s.Target); err != nil {
				return "", err
			}
		}
		return "", nil
	case OpDocker:
		return undoContainer(ctx, sys, s)
	case OpOverride:
		return sys.Tool(ctx, "systemd-run", nil, overrideArgs(s, exe, "remove-override", contentHash(s.New))...)
	case OpSwapFile:
		if active, err := swapActive(sys, s.Target); err != nil || active {
			return "", errors.Join(err, errors.New("the swap file is still in use"))
		}
		return "", sys.RemoveFile(s.Target)
	case OpLimit:
		// revert puts the unit's own limits back on its cgroup once the
		// drop-in is gone and systemd reloaded; this only clears what a
		// helper stopped in the middle of the step left behind.
		return clearRuntime(sys, s)
	}
	return "", fmt.Errorf("unknown step %q", s.Op)
}

// removeDir takes away the folder an OpFile step's drop-in went in, once
// nothing is left in it.
func (s *Step) removeDir(sys System) error {
	if s.Dir == "" {
		return nil
	}
	_, err := sys.RemoveDir(s.Dir)
	return err
}

func checkKernel(sys System, path, want string) error {
	b, err := sys.ReadFile(path)
	if err != nil {
		return err
	}
	if got := strings.TrimSpace(string(b)); got != want {
		return fmt.Errorf("%s reads %q after writing %q; the kernel did not take it", path, got, want)
	}
	return nil
}

func updateContainer(ctx context.Context, sys System, id string, mem docker.MemoryUpdate) error {
	d := sys.Docker()
	if d == nil {
		return errors.New("docker is not running")
	}
	if err := d.UpdateMemory(ctx, id, mem); err != nil {
		return err
	}
	c, err := d.Inspect(ctx, id)
	if err != nil {
		return err
	}
	if c.HostConfig.Memory != mem.Memory || (mem.MemoryReservation != 0 && c.HostConfig.MemoryReservation != mem.MemoryReservation) {
		return fmt.Errorf("container %s reports limit %d and soft limit %d after setting %d and %d", id[:12],
			c.HostConfig.Memory, c.HostConfig.MemoryReservation, mem.Memory, mem.MemoryReservation)
	}
	return nil
}

func undoContainer(ctx context.Context, sys System, s *Step) (string, error) {
	d := sys.Docker()
	if d == nil {
		return "", errors.New("docker is not running, so the container's limits can't be put back")
	}
	c, err := d.Inspect(ctx, s.Target)
	if errors.Is(err, docker.ErrNotFound) {
		return fmt.Sprintf("Container %s is gone, so it has no limit to put back.", s.Label), nil
	}
	if err != nil {
		return "", err
	}
	if c.HostConfig.Memory != s.NewMemory.Memory {
		if c.HostConfig.Memory == s.OldMemory.Memory || (s.OldMemory.Memory == 0 && c.HostConfig.Memory == noDockerLimit) {
			return "", nil
		}
		return fmt.Sprintf("The limit of container %s was changed after Snugbox set it, so it was left as it is.", s.Label), nil
	}
	back := s.OldMemory
	var notes []string
	if back.Memory == 0 {
		back.Memory = noDockerLimit
		notes = append(notes, "the largest limit, which the kernel treats as none")
	}
	if back.MemoryReservation == 0 {
		back.MemoryReservation = minDockerSoftLimit
		notes = append(notes, "a soft limit of 6 MiB, the smallest Docker takes")
	}
	if back.MemorySwap == 0 {
		back.MemorySwap = -1
	}
	var note string
	if len(notes) > 0 {
		note = fmt.Sprintf("Docker can't remove a limit once set, so container %s now has %s.", s.Label, strings.Join(notes, " and "))
	}
	return note, updateContainer(ctx, sys, s.Target, back)
}

// overrideArgs runs the helper's override writer through systemd-run as the
// project folder's owner, in a sandbox that can write only that folder.
func overrideArgs(s *Step, exe, verb string, extra ...string) []string {
	dir, name := path.Dir(s.Target), path.Base(s.Target)
	args := []string{
		"--wait", "--pipe", "--quiet", "--collect", "--service-type=exec",
		fmt.Sprintf("--uid=%d", s.UID), fmt.Sprintf("--gid=%d", s.GID),
		"--property=ProtectSystem=strict", "--property=ReadWritePaths=" + dir,
		"--property=NoNewPrivileges=yes", "--property=PrivateNetwork=yes", "--property=PrivateTmp=yes",
		"--property=CapabilityBoundingSet=", "--property=RestrictAddressFamilies=AF_UNIX",
		"--", exe, verb, dir, name,
	}
	return append(args, extra...)
}

func contentHash(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])
}

// swapActive reports whether path is an active swap area.
func swapActive(sys System, path string) (bool, error) {
	b, err := sys.ReadFile("/proc/swaps")
	if err != nil {
		return false, err
	}
	swaps, err := kernel.ParseSwaps(b)
	if err != nil {
		return false, err
	}
	for _, s := range swaps {
		if s.Filename == path {
			return true, nil
		}
	}
	return false, nil
}
