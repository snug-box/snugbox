// Package doctor checks whether a box has what Snugbox needs: the kernel
// version, cgroup v2, pressure files, zswap, the kind of virtualisation and
// the permissions to read everything. Each failure says what is missing and
// what to do about it.
package doctor

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"strings"

	"snugbox.dev/snugbox/agent/internal/collect"
	"snugbox.dev/snugbox/agent/internal/docker"
	"snugbox.dev/snugbox/agent/internal/fix"
	"snugbox.dev/snugbox/agent/internal/report"
)

// Status is the outcome of one check.
type Status int

// Outcomes, mildest first.
const (
	Pass Status = iota
	Info
	Warn
	Fail
)

func (s Status) String() string {
	return [...]string{"ok", "info", "warn", "fail"}[s]
}

// Check is one line of the doctor's report.
type Check struct {
	Status Status
	Text   string
}

// Inputs are the facts the checks read.
type Inputs struct {
	FS      fs.FS
	Machine collect.Machine
	Root    bool
	// CgroupRoot is the cgroup v2 mount without its leading slash.
	CgroupRoot string
	// Docker reports the Docker engine's state: nil when the socket is
	// missing, otherwise the result of asking it for its version, which is
	// docker.ErrUnsupported for an engine too old to read.
	Docker func(ctx context.Context) (string, error)
	// Readable reports whether a root-only file can be opened.
	Readable func(path string) bool
}

// Run performs every check.
func Run(ctx context.Context, in Inputs) []Check {
	m := in.Machine
	var out []Check
	add := func(s Status, format string, args ...any) { out = append(out, Check{s, fmt.Sprintf(format, args...)}) }

	switch k := m.Kernel; {
	case k.Major == 0:
		add(Fail, "Couldn't read the kernel version from /proc/sys/kernel/osrelease.")
	case !k.AtLeast(5, 4):
		add(Fail, "Linux %s is older than 5.4, the oldest kernel Snugbox audits.", k)
	case !k.AtLeast(5, 19):
		add(Warn, "Linux %s: audits work; memory.peak and active probing need 5.19 or newer.", k)
	default:
		add(Pass, "Linux %s.", k)
	}

	switch {
	case !m.CgroupV2:
		add(Fail, "No cgroup v2. Snugbox measures workloads through cgroup v2; boot with systemd.unified_cgroup_hierarchy=1 or upgrade the distribution.")
	case !m.MemoryCtl:
		add(Fail, "cgroup v2 is mounted at /%s but the memory controller is off.", in.CgroupRoot)
	default:
		add(Pass, "cgroup v2 at /%s with the memory controller on.", in.CgroupRoot)
	}

	if m.PSI {
		add(Pass, "Pressure stall information is on.")
	} else {
		add(Warn, "Couldn't read /proc/pressure/memory, so pressure data is missing and no fix can run: fixes wait for a calm box. The kernel is older than 4.20 or has it off; boot with psi=1, on RHEL and its rebuilds with sudo grubby --update-kernel=ALL --args=psi=1, then reboot.")
	}

	switch {
	case len(m.Swaps) > 0:
		var total uint64
		for _, s := range m.Swaps {
			total += s.Size
		}
		areas := "1 area"
		if len(m.Swaps) > 1 {
			areas = fmt.Sprintf("%d areas", len(m.Swaps))
		}
		add(Info, "Swap: %s in %s.", report.Size(total), areas)
	default:
		add(Info, "No swap.")
	}
	switch z := m.Zswap; {
	case !z.Available:
		add(Info, "zswap is not built into this kernel; a swap fix would add a swap file without compression.")
	case z.Enabled:
		add(Info, "zswap is on, with %s.", z.Compressor)
	default:
		add(Info, "zswap is available and off.")
	}

	switch {
	case m.Virt == "":
		add(Info, "Couldn't tell what kind of machine this is.")
	case m.Virt == "wsl":
		add(Warn, "This is WSL. Audits and service limits work; swap and zswap belong to Windows, so Snugbox leaves them alone.")
	case !fix.FullVM(m.Virt):
		add(Warn, "This box is a container (%s): the kernel belongs to the host, so only container and service limits can be fixed.", m.Virt)
	case m.Virt == "none":
		add(Pass, "Bare metal.")
	default:
		add(Pass, "Virtual machine (%s) with its own kernel.", m.Virt)
	}

	if in.FS != nil {
		if _, err := fs.Stat(in.FS, "run/systemd/system"); err == nil {
			add(Pass, "systemd is the init system.")
		} else {
			add(Warn, "systemd is not running. Services can still be audited as process groups; service limits need systemd.")
		}
	}
	if m.OOMDaemon != "" {
		add(Pass, "Early out-of-memory protection is on: %s.", m.OOMDaemon)
	} else {
		add(Info, "No early out-of-memory protection is running.")
	}

	if in.Root {
		add(Pass, "Running as root.")
	} else {
		add(Warn, "Not running as root. Run sudo snugbox doctor to check the files only root can read.")
	}
	if in.Readable != nil {
		if in.Readable("/proc/1/smaps_rollup") {
			add(Pass, "Process memory details are readable.")
		} else {
			add(Warn, "Couldn't read /proc/1/smaps_rollup; processes outside cgroups can't be measured without root.")
		}
		if in.Readable("/dev/kmsg") {
			add(Pass, "The kernel log is readable, so kill times are known.")
		} else {
			add(Warn, "Couldn't read /dev/kmsg; out-of-memory kills are counted but their times are unknown.")
		}
	}

	if in.Docker == nil {
		add(Info, "No Docker socket at /var/run/docker.sock.")
		return out
	}
	v, err := in.Docker(ctx)
	switch {
	case err == nil:
		add(Pass, "Docker Engine %s answers, so containers get their names.", v)
	case errors.Is(err, docker.ErrUnsupported):
		add(Warn, "%s. Snugbox reads Docker Engine 25 or newer, so containers will show their IDs and can't be given limits.", upperFirst(err.Error()))
	default:
		add(Warn, "Docker didn't answer (%v); containers will show their IDs.", err)
	}
	return out
}

func upperFirst(s string) string {
	if s == "" {
		return s
	}
	return strings.ToUpper(s[:1]) + s[1:]
}
