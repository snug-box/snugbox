package collect

import (
	"time"

	"snugbox.dev/snugbox/agent/internal/kernel"
)

// LoggedKill is an out-of-memory kill found in the kernel log.
type LoggedKill struct {
	At time.Time
	kernel.OOMKill
}

// KernelLog is what the kernel log still holds about out-of-memory kills.
type KernelLog struct {
	Kills []LoggedKill
	// Since is the time of the oldest record. Kills before it have scrolled
	// out of the ring buffer.
	Since time.Time
}

// KillTimes returns the times of the logged kills.
func (l KernelLog) KillTimes() []time.Time {
	out := make([]time.Time, len(l.Kills))
	for i, k := range l.Kills {
		out[i] = k.At
	}
	return out
}
