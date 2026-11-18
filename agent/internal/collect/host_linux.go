//go:build linux

package collect

import (
	"bytes"
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"golang.org/x/sys/unix"

	"snugbox.dev/snugbox/agent/internal/kernel"
)

// LiveHost is the Host of the running Linux system.
type LiveHost struct{}

var detectVirtPaths = []string{"/usr/bin/systemd-detect-virt", "/bin/systemd-detect-virt"}

// Virtualization runs systemd-detect-virt, which prints "none" and exits 1 on
// bare metal.
func (LiveHost) Virtualization(ctx context.Context) (string, error) {
	for _, p := range detectVirtPaths {
		if _, err := os.Stat(p); err != nil {
			continue
		}
		ctx, cancel := context.WithTimeout(ctx, 2*time.Second)
		defer cancel()
		out, err := exec.CommandContext(ctx, p).Output() //nolint:gosec // p is one of the fixed paths in detectVirtPaths
		v := strings.TrimSpace(string(out))
		if err != nil {
			var exitErr *exec.ExitError
			if errors.As(err, &exitErr) && v == "none" {
				return v, nil
			}
			return "", err
		}
		return v, nil
	}
	return "", ErrNoHost
}

// Disks measures each mount with statfs and, for the root filesystem, the
// size of the block device under it.
func (LiveHost) Disks(mounts []kernel.Mount) []Disk {
	var out []Disk
	for _, m := range mounts {
		var st unix.Statfs_t
		if err := unix.Statfs(m.MountPoint, &st); err != nil || st.Bsize <= 0 {
			continue
		}
		bs := uint64(st.Bsize)
		d := Disk{
			MountPoint: m.MountPoint,
			FSType:     m.FSType,
			Device:     m.Source,
			Size:       st.Blocks * bs,
			Used:       (st.Blocks - st.Bfree) * bs,
		}
		if m.MountPoint == "/" {
			d.DeviceSize = blockDeviceSize(m.Device)
		}
		out = append(out, d)
	}
	return out
}

// blockDeviceSize returns the size in bytes of the whole disk behind a
// major:minor device number: the parent disk when the device is a partition.
func blockDeviceSize(majMin string) uint64 {
	dir, err := filepath.EvalSymlinks(filepath.Join("/sys/dev/block", majMin))
	if err != nil {
		return 0
	}
	if _, err := os.Stat(filepath.Join(dir, "partition")); err == nil {
		dir = filepath.Dir(dir)
	}
	b, err := os.ReadFile(filepath.Join(dir, "size"))
	if err != nil {
		return 0
	}
	sectors, err := strconv.ParseUint(string(bytes.TrimSpace(b)), 10, 64)
	if err != nil {
		return 0
	}
	return sectors * 512
}
