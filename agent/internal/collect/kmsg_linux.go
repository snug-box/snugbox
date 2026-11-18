//go:build linux

package collect

import (
	"errors"
	"os"
	"syscall"
	"time"

	"snugbox.dev/snugbox/agent/internal/kernel"
)

// maxKmsgRecords bounds one read of the kernel log. The default ring buffer
// holds far fewer records than this.
const maxKmsgRecords = 1 << 17

// ReadKernelLog reads the out-of-memory kills still in the kernel's ring
// buffer through /dev/kmsg, and the time of the oldest record. Reading needs
// root or CAP_SYSLOG when kernel.dmesg_restrict is set, as it is on Ubuntu.
func ReadKernelLog(boot time.Time) (KernelLog, error) {
	f, err := os.OpenFile("/dev/kmsg", os.O_RDONLY|syscall.O_NONBLOCK, 0)
	if err != nil {
		return KernelLog{}, err
	}
	defer f.Close()
	var log KernelLog
	buf := make([]byte, 16<<10)
	for range maxKmsgRecords {
		n, err := syscall.Read(int(f.Fd()), buf)
		switch {
		case errors.Is(err, syscall.EAGAIN):
			return log, nil
		case errors.Is(err, syscall.EPIPE):
			continue
		case err != nil:
			return log, err
		case n == 0:
			return log, nil
		}
		rec, err := kernel.ParseKmsgRecord(buf[:n])
		if err != nil {
			continue
		}
		at := boot.Add(rec.SinceBoot)
		if log.Since.IsZero() {
			log.Since = at
		}
		if k, ok := kernel.ParseOOMKill(rec.Message); ok {
			log.Kills = append(log.Kills, LoggedKill{At: at, OOMKill: k})
		}
	}
	return log, nil
}
