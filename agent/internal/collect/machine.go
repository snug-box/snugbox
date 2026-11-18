package collect

import (
	"context"
	"errors"
	"io/fs"
	"path"
	"strconv"
	"strings"
	"time"

	"snugbox.dev/snugbox/agent/internal/kernel"
)

// Zswap is the state of the compressed swap cache.
type Zswap struct {
	// Available is false when the kernel was built without zswap.
	Available      bool
	Enabled        bool
	Compressor     string
	MaxPoolPercent int
	// Shrinker is the zswap shrinker setting, present from Linux 6.8.
	Shrinker    bool
	HasShrinker bool
}

// Disk is one local filesystem.
type Disk struct {
	MountPoint string
	FSType     string
	Device     string
	Size       uint64
	Used       uint64
	// DeviceSize is the size of the whole block device under the root
	// filesystem, which decides whether a provider can resize in place.
	DeviceSize uint64
}

// Machine holds the facts about a box that don't change from one sample to
// the next.
type Machine struct {
	Hostname   string
	Kernel     kernel.Release
	OS         string
	OSID       string
	OSVersion  string
	Virt       string
	CPUs       int
	PageSize   uint64
	MemTotal   uint64
	Uptime     time.Duration
	BootTime   time.Time
	CgroupV2   bool
	MemoryCtl  bool
	PSI        bool
	Peak       bool
	Reclaim    bool
	Swaps      []kernel.Swap
	ZramSwap   bool
	Zswap      Zswap
	THP        string
	THPDefrag  string
	Overcommit int
	Swappiness int
	// OOMDaemon names the early out-of-memory killer that runs, if any:
	// systemd-oomd, earlyoom or nohang.
	OOMDaemon string
	// OOMPolicy is a systemd-oomd policy someone set already for the system
	// or user slice, read when systemd-oomd is installed.
	OOMPolicy OOMPolicy
	Disks     []Disk
	// EgressPerMonth and IngressPerMonth estimate traffic a month from the
	// counters since boot of interfaces backed by a device.
	EgressPerMonth  uint64
	IngressPerMonth uint64
	DockerSocket    bool
}

// RootDisk returns the filesystem mounted at /, if it was measured.
func (m Machine) RootDisk() (Disk, bool) {
	for _, d := range m.Disks {
		if d.MountPoint == "/" {
			return d, true
		}
	}
	return Disk{}, false
}

// DiskUsed sums the used space of every local filesystem.
func (m Machine) DiskUsed() uint64 {
	var n uint64
	for _, d := range m.Disks {
		n += d.Used
	}
	return n
}

// Host does the few things a file read can't: measure filesystems with
// statfs and ask systemd-detect-virt what the machine is. LiveHost implements
// it on Linux; tests fake it.
type Host interface {
	Virtualization(ctx context.Context) (string, error)
	Disks(mounts []kernel.Mount) []Disk
}

// localFS lists the filesystem types that hold a server's own data.
var localFS = map[string]bool{"ext2": true, "ext3": true, "ext4": true, "xfs": true, "btrfs": true, "f2fs": true, "zfs": true, "jfs": true, "reiserfs": true, "bcachefs": true}

// Machine reads the box's fixed facts. Host may be nil, which leaves the
// virtualisation to file checks and the disks unmeasured.
func (c *Collector) Machine(ctx context.Context, host Host) Machine {
	m := Machine{PageSize: c.pageSize, CgroupV2: c.cgroupRoot != ""}
	m.Hostname = c.trimmed("proc/sys/kernel/hostname")
	if r, err := kernel.ParseRelease(c.trimmed("proc/sys/kernel/osrelease")); err == nil {
		m.Kernel = r
	} else {
		c.gap("kernel release", err)
	}
	c.readOS(&m)
	if n, err := kernel.ParseCPUList(c.trimmed("sys/devices/system/cpu/online")); err == nil {
		m.CPUs = n
	}
	if raw, err := c.read("proc/meminfo"); err == nil {
		mi, _ := kernel.ParseMeminfo(raw)
		m.MemTotal = mi.Get("MemTotal")
	}
	m.Uptime = c.uptime()
	if raw, err := c.read("proc/stat"); err == nil {
		if ps, err := kernel.ParseProcStat(raw); err == nil {
			m.BootTime = ps.BootTime
		}
	}
	m.PSI = c.exists("proc/pressure/memory")
	if m.CgroupV2 {
		controllers := c.trimmed(path.Join(c.cgroupRoot, "cgroup.controllers"))
		m.MemoryCtl = strings.Contains(" "+controllers+" ", " memory ")
		m.Reclaim = c.exists(path.Join(c.cgroupRoot, "memory.reclaim"))
		m.Peak = c.anyWorkloadHas("memory.peak") || m.Kernel.AtLeast(5, 19)
	}
	if raw, err := c.read("proc/swaps"); err == nil {
		if swaps, err := kernel.ParseSwaps(raw); err == nil {
			m.Swaps = swaps
			for _, s := range swaps {
				if strings.HasPrefix(s.Filename, "/dev/zram") {
					m.ZramSwap = true
				}
			}
		}
	}
	m.Zswap = c.zswap()
	m.THP = c.choice("sys/kernel/mm/transparent_hugepage/enabled")
	m.THPDefrag = c.choice("sys/kernel/mm/transparent_hugepage/defrag")
	m.Overcommit = c.intFile("proc/sys/vm/overcommit_memory", -1)
	m.Swappiness = c.intFile("proc/sys/vm/swappiness", -1)
	m.OOMDaemon = c.oomDaemon()
	m.OOMPolicy = c.oomPolicy()
	m.EgressPerMonth, m.IngressPerMonth = c.trafficPerMonth(m.Uptime)
	m.DockerSocket = c.exists("var/run/docker.sock") || c.exists("run/docker.sock")
	m.Virt = c.virtFromFiles()
	if host != nil {
		if v, err := host.Virtualization(ctx); err == nil && v != "" {
			m.Virt = v
		}
		if b, err := c.read("proc/self/mountinfo"); err == nil {
			if mounts, err := kernel.ParseMountinfo(b); err == nil {
				m.Disks = host.Disks(localMounts(mounts))
			}
		}
	}
	return m
}

func (c *Collector) trimmed(name string) string {
	b, err := c.read(name)
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(b))
}

// choice reads a sysfs choice file and returns the selected option, or ""
// when the file is missing or unreadable.
func (c *Collector) choice(name string) string {
	b, err := c.read(name)
	if err != nil {
		return ""
	}
	v, err := kernel.ParseChoice(b)
	if err != nil {
		return ""
	}
	return v
}

func (c *Collector) intFile(name string, fallback int) int {
	n, err := strconv.Atoi(c.trimmed(name))
	if err != nil {
		return fallback
	}
	return n
}

func (c *Collector) readOS(m *Machine) {
	for _, name := range []string{"etc/os-release", "usr/lib/os-release"} {
		b, err := c.read(name)
		if err != nil {
			continue
		}
		osr := kernel.ParseOSRelease(b)
		m.OS, m.OSID, m.OSVersion = osr["PRETTY_NAME"], osr["ID"], osr["VERSION_ID"]
		if m.OS == "" {
			m.OS = strings.TrimSpace(osr["NAME"] + " " + osr["VERSION_ID"])
		}
		return
	}
	c.gap("/etc/os-release", fs.ErrNotExist)
}

func (c *Collector) uptime() time.Duration {
	f := strings.Fields(c.trimmed("proc/uptime"))
	if len(f) == 0 {
		return 0
	}
	sec, err := strconv.ParseFloat(f[0], 64)
	if err != nil || sec < 0 || sec > 1e10 {
		return 0
	}
	return time.Duration(sec * float64(time.Second))
}

func (c *Collector) anyWorkloadHas(file string) bool {
	for _, dir := range c.cgroupDir {
		if c.exists(path.Join(dir, file)) {
			return true
		}
	}
	return false
}

func (c *Collector) zswap() Zswap {
	const dir = "sys/module/zswap/parameters"
	var z Zswap
	enabled, err := c.read(path.Join(dir, "enabled"))
	if err != nil {
		return z
	}
	z.Available = true
	z.Enabled = strings.TrimSpace(string(enabled)) == "Y"
	z.Compressor = c.trimmed(path.Join(dir, "compressor"))
	z.MaxPoolPercent = c.intFile(path.Join(dir, "max_pool_percent"), 0)
	if b, err := c.read(path.Join(dir, "shrinker_enabled")); err == nil {
		z.HasShrinker, z.Shrinker = true, strings.TrimSpace(string(b)) == "Y"
	}
	return z
}

// oomDaemon looks for the cgroup of a running early out-of-memory killer.
func (c *Collector) oomDaemon() string {
	if c.cgroupRoot == "" {
		return ""
	}
	for _, unit := range []string{"systemd-oomd", "earlyoom", "nohang"} {
		dir := path.Join(c.cgroupRoot, "system.slice", unit+".service")
		if len(c.readPIDs(path.Join(dir, "cgroup.procs"))) > 0 {
			return unit
		}
	}
	return ""
}

// trafficPerMonth scales the bytes sent and received since boot to an average
// month, 30.44 days. Only interfaces with a device link count, which leaves
// out loopback, bridges and the veth pairs of containers.
func (c *Collector) trafficPerMonth(uptime time.Duration) (out, in uint64) {
	raw, err := c.read("proc/net/dev")
	if err != nil || uptime < time.Hour {
		return 0, 0
	}
	counters, err := kernel.ParseNetDev(raw)
	if err != nil {
		return 0, 0
	}
	var tx, rx uint64
	for name, n := range counters {
		if name != "lo" && c.exists(path.Join("sys/class/net", name, "device")) {
			tx += n.TxBytes
			rx += n.RxBytes
		}
	}
	const month = 30.44 * 24 * float64(time.Hour)
	f := month / float64(uptime)
	return uint64(float64(tx) * f), uint64(float64(rx) * f)
}

// virtFromFiles detects the common virtualisation types without
// systemd-detect-virt.
func (c *Collector) virtFromFiles() string {
	switch {
	case c.exists("proc/vz") && !c.exists("proc/bc"):
		return "openvz"
	case c.trimmed("run/systemd/container") != "":
		return c.trimmed("run/systemd/container")
	}
	vendor := strings.ToLower(c.trimmed("sys/class/dmi/id/sys_vendor"))
	product := strings.ToLower(c.trimmed("sys/class/dmi/id/product_name"))
	switch {
	case strings.Contains(vendor, "amazon"):
		return "amazon"
	case strings.Contains(vendor, "qemu"), strings.Contains(product, "kvm"), strings.Contains(vendor, "hetzner"),
		strings.Contains(vendor, "digitalocean"), strings.Contains(vendor, "vultr"), strings.Contains(vendor, "linode"),
		strings.Contains(vendor, "akamai"):
		return "kvm"
	case strings.Contains(vendor, "microsoft"):
		return "microsoft"
	case strings.Contains(vendor, "vmware"):
		return "vmware"
	case strings.Contains(vendor, "xen"):
		return "xen"
	}
	if cpuinfo, err := c.read("proc/cpuinfo"); err == nil && strings.Contains(string(cpuinfo), " hypervisor") {
		return "vm-other"
	}
	if vendor == "" {
		return ""
	}
	return "none"
}

func localMounts(mounts []kernel.Mount) []kernel.Mount {
	var out []kernel.Mount
	seen := make(map[string]bool)
	for _, m := range mounts {
		if !localFS[m.FSType] || seen[m.Source] {
			continue
		}
		seen[m.Source] = true
		out = append(out, m)
	}
	return out
}

// ErrNoHost is returned by host operations on systems where they can't run.
var ErrNoHost = errors.New("not available on this operating system")
