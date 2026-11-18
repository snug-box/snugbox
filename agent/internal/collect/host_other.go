//go:build !linux

package collect

import (
	"context"
	"time"

	"snugbox.dev/snugbox/agent/internal/kernel"
)

// LiveHost is the Host of the running system. Snugbox measures Linux only, so
// elsewhere it reports that nothing is available; the build exists so the
// code can be developed and its pure parts tested on any machine.
type LiveHost struct{}

// Virtualization is unavailable outside Linux.
func (LiveHost) Virtualization(context.Context) (string, error) { return "", ErrNoHost }

// Disks is unavailable outside Linux.
func (LiveHost) Disks([]kernel.Mount) []Disk { return nil }

// ReadKernelLog is unavailable outside Linux.
func ReadKernelLog(time.Time) (KernelLog, error) { return KernelLog{}, ErrNoHost }
