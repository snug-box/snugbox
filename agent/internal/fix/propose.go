// Package fix holds the fix catalogue of blueprint section 3.7: typed changes
// with preconditions, a risk level, a verification window and an undo.
// Nothing outside this catalogue can be applied.
//
// This file decides which fixes a box should be offered and with which
// values. It is pure: the audit, the watcher and snugbox fit all propose
// through it, so they never disagree.
package fix

import (
	"slices"
	"strconv"
	"time"

	"snugbox.dev/snugbox/agent/internal/collect"
	"snugbox.dev/snugbox/agent/internal/cron"
	"snugbox.dev/snugbox/agent/internal/helperapi"
	"snugbox.dev/snugbox/agent/internal/model"
	helperv1 "snugbox.dev/snugbox/gen/snugbox/helper/v1"
)

// Kind names a fix type.
type Kind string

// The fix types of blueprint section 3.7 that change something.
const (
	KindSwap          Kind = "swap"
	KindOOMProtection Kind = "oom-protection"
	KindContainer     Kind = "container-limits"
	KindService       Kind = "service-limits"
	KindHeap          Kind = "heap-caps"
	KindSchedule      Kind = "schedule"
)

// Risk is a fix's risk level.
type Risk string

// The early out-of-memory killers an oom-protection fix can turn on.
const (
	DaemonSystemdOOMD = "systemd-oomd"
	DaemonEarlyoom    = "earlyoom"
)

// Risk levels.
const (
	RiskLow    Risk = "low"
	RiskMedium Risk = "medium"
	RiskHigh   Risk = "medium to high"
)

// Multipliers and limits from blueprint section 3.7, starting values for the
// outside review.
const (
	ContainerFactor     = 1.3
	ContainerSockFactor = 1.5
	HardFactor          = 1.2
	ServiceHighFactor   = 1.15
	ServiceMaxFactor    = 1.4
	LimitStep           = 64 * model.MiB
	LimitWindow         = 7 * 24 * time.Hour
	// SockHeavyShare marks a workload whose network buffers reach this share
	// of its working set, and get the larger margin.
	SockHeavyShare = 0.10
	// ShmemShare is the share of shared memory and tmpfs above which a limit
	// needs swap, since that memory can't be reclaimed otherwise.
	ShmemShare = 0.10
	// BusyCPUShare is the share of the box's CPUs in use above which zswap
	// uses lz4, which costs less CPU than zstd.
	BusyCPUShare = 0.7
)

// Limit is a proposed memory limit for one workload.
type Limit struct {
	WorkloadID string
	Name       string
	Max        uint64
	// Soft is Docker's --memory-reservation for containers and MemoryHigh for
	// services.
	Soft uint64
	// LowConfidence is true when the workload was watched for less than seven
	// days, so the limit needs the user to accept an early estimate.
	LowConfidence bool
}

// Heap is a proposed heap cap for one workload's runtime.
type Heap struct {
	WorkloadID string
	Name       string
	Runtime    string
	// Env is the environment variable the cap goes in, and Setting its
	// value, such as JAVA_TOOL_OPTIONS and "-Xmx384m". PHP-FPM has no
	// variable; its Setting is the pool line, such as "pm.max_children = 12".
	Env     string
	Setting string
	// Bytes is the heap size, zero for MALLOC_ARENA_MAX and PHP-FPM.
	Bytes uint64
	// Limit is the memory limit the cap was sized against, set already or
	// proposed.
	Limit uint64
	// Unit is the service whose drop-in sets the cap. It is empty where the
	// cap is advice for a person: for a container, and for PHP-FPM, whose
	// setting lives in its own pool file.
	Unit string
	// Workers and WorkerBytes are PHP-FPM's worker count now and the
	// largest worker seen, which the pool line is worked out from.
	Workers     int
	WorkerBytes uint64
}

// Proposal is one fix Snugbox offers for a box.
type Proposal struct {
	Kind Kind
	Risk Risk
	// SwapSize, Compressor and Zswap describe a swap fix. Zswap is false when
	// the kernel has no zswap, so only the swap file is offered.
	SwapSize   uint64
	Compressor string
	Zswap      bool
	Shrinker   bool
	// OOMDaemon is the early out-of-memory killer an oom-protection fix turns
	// on: DaemonSystemdOOMD or DaemonEarlyoom.
	OOMDaemon string
	Limits    []Limit
	Heaps     []Heap
	// Shifts are the timers a schedule fix moves, and Cron the cron jobs it
	// suggests starting later, which a person changes in the crontab.
	Shifts []Shift
	Cron   []CronMove
	// NeedsRestart is true when the fix only takes effect after a restart,
	// which Snugbox never does without approval and a chosen time.
	NeedsRestart bool
}

// Input is what proposing needs to know about a box.
type Input struct {
	Machine   collect.Machine
	Workloads map[string]model.Workload
	Summary   model.Summary
	Window    time.Duration
	// SystemdOOMD is true when the systemd-oomd binary is installed, and
	// Earlyoom when earlyoom's is.
	SystemdOOMD bool
	Earlyoom    bool
	// FreeDisk is the free space on the filesystem that would hold the swap
	// file, and SwapFS its type.
	FreeDisk uint64
	SwapFS   string
	// Runs are the services' runs the watcher recorded, and Now the end of
	// the window, for the schedule fix. A one-off audit has none. Timers
	// are the timers that start those services, by service.
	Runs   []model.Run
	Now    time.Time
	Timers map[string]model.Timer
	// Cron are the jobs in the box's crontabs, Minutes the box's highest
	// working set in each minute of the window, and Zone the time zone cron
	// reads their schedules in, for the schedule fix's advice on cron jobs.
	// Only the watcher has them.
	Cron    []cron.Job
	Minutes []model.MinuteTotal
	Zone    *time.Location
}

// containerVirt lists virtualisation types where the kernel belongs to the
// host, so kernel settings such as swap and zswap can't be changed.
var containerVirt = []string{"lxc", "lxc-libvirt", "openvz", "systemd-nspawn", "docker", "podman", "rkt", "wsl", "proot", "pouch"}

// FullVM reports whether the box controls its own kernel.
func FullVM(virt string) bool { return !slices.Contains(containerVirt, virt) }

// Propose returns the fixes a box should be offered, in the order the report
// numbers them.
func Propose(in Input) []Proposal {
	var out []Proposal
	if p, ok := proposeSwap(in); ok {
		out = append(out, p)
	}
	containers, services := containerLimits(in), serviceLimits(in)
	if len(containers) > 0 {
		out = append(out, Proposal{Kind: KindContainer, Risk: RiskMedium, Limits: containers})
	}
	if len(services) > 0 {
		out = append(out, Proposal{Kind: KindService, Risk: RiskMedium, Limits: services})
	}
	if in.Machine.OOMDaemon == "" {
		// systemd-oomd first; earlyoom is the fallback blueprint section 3.7
		// names for boxes without it.
		switch {
		case in.Machine.OOMPolicy.Slice != "":
			// The distribution or a person gave systemd-oomd a policy and
			// the daemon is off. The helper leaves a policy it didn't write
			// alone, so there is no fix; the report says how to start it.
		case in.Machine.CgroupV2 && in.SystemdOOMD:
			out = append(out, Proposal{Kind: KindOOMProtection, Risk: RiskMedium, OOMDaemon: DaemonSystemdOOMD})
		case in.Earlyoom:
			out = append(out, Proposal{Kind: KindOOMProtection, Risk: RiskMedium, OOMDaemon: DaemonEarlyoom})
		}
	}
	if hs := heapCaps(in, append(containers, services...)); len(hs) > 0 {
		out = append(out, Proposal{Kind: KindHeap, Risk: RiskHigh, Heaps: hs, NeedsRestart: true})
	}
	if shifts, moves := scheduleShifts(in), cronMoves(in); len(shifts) > 0 || len(moves) > 0 {
		out = append(out, Proposal{Kind: KindSchedule, Risk: RiskLow, Shifts: shifts, Cron: moves})
	}
	return out
}

// swapFS lists the filesystems Snugbox makes a swap file on. Btrfs takes one
// only through btrfs filesystem mkswapfile, which the helper checks for; ZFS
// is left out because a swap file on it can deadlock.
var swapFS = map[string]bool{"ext4": true, "xfs": true, "btrfs": true}

func proposeSwap(in Input) (Proposal, bool) {
	m := in.Machine
	if len(m.Swaps) > 0 || m.ZramSwap || !FullVM(m.Virt) || !swapFS[in.SwapFS] {
		return Proposal{}, false
	}
	size := helperapi.SwapCeiling(m.MemTotal)
	if size < helperapi.MinSwap || in.FreeDisk < size+size/10+2*model.GiB {
		return Proposal{}, false
	}
	p := Proposal{Kind: KindSwap, Risk: RiskLow, SwapSize: size, Zswap: m.Zswap.Available, Compressor: "zstd"}
	if m.CPUs > 0 && in.Summary.CPUQ95 > BusyCPUShare*float64(m.CPUs) {
		p.Compressor = "lz4"
	}
	p.Shrinker = m.Zswap.Available && m.Kernel.AtLeast(6, 8)
	return p, true
}

// LimitFor computes the hard and soft limits for a workload from its summary:
// the hard limit is the larger of 1.3 times the 99.9th percentile of working
// set (1.5 times when network buffers are large) and 1.2 times the highest
// hard memory, rounded up to 64 MiB; the soft limit is the 95th percentile in
// whole MiB, kept below the hard limit.
func LimitFor(w model.WorkloadSummary) (hard, soft uint64) {
	factor := ContainerFactor
	if float64(w.SockMax) >= SockHeavyShare*float64(w.WorkingSetMax) && w.SockMax > 0 {
		factor = ContainerSockFactor
	}
	hard = max(roundUp(max(scale(w.WorkingSetQ999, factor), scale(w.HardMax, HardFactor)), LimitStep), LimitStep)
	soft = min(w.WorkingSetQ95/model.MiB*model.MiB, hard-model.MiB)
	return hard, soft
}

func containerLimits(in Input) []Limit {
	var out []Limit
	for _, id := range sortedIDs(in.Workloads) {
		w := in.Workloads[id]
		s, ok := in.Summary.Workloads[id]
		if w.Kind != model.KindContainer || w.Max.Set || w.Database != "" || !ok || !shmemOK(in, s) {
			continue
		}
		hard, soft := LimitFor(s)
		out = append(out, Limit{WorkloadID: id, Name: w.Label(), Max: hard, Soft: soft, LowConfidence: in.Window < LimitWindow})
	}
	return out
}

// MinServiceLimit is the smallest service Snugbox offers a limit for. A limit
// on a service that holds a few MiB protects nothing and can break it.
const MinServiceLimit = 64 * model.MiB

func serviceLimits(in Input) []Limit {
	var out []Limit
	for _, id := range sortedIDs(in.Workloads) {
		w := in.Workloads[id]
		s, ok := in.Summary.Workloads[id]
		if w.Kind != model.KindService || w.Max.Set || w.Database != "" || helperapi.ProtectedUnit(w.Name) || !ok || !shmemOK(in, s) {
			continue
		}
		base := max(s.WorkingSetQ999, s.HardMax)
		if base < MinServiceLimit {
			continue
		}
		high := roundUp(scale(base, ServiceHighFactor), LimitStep)
		// Both round to 64 MiB steps, so on a small service they can meet;
		// MemoryMax stays a step above MemoryHigh.
		hard := max(roundUp(scale(base, ServiceMaxFactor), LimitStep), high+LimitStep)
		out = append(out, Limit{WorkloadID: id, Name: w.Name, Max: hard, Soft: high, LowConfidence: in.Window < LimitWindow})
	}
	return out
}

// shmemOK holds a limit back from a workload whose shared memory or tmpfs is
// over a tenth of its working set unless the box has swap.
func shmemOK(in Input, s model.WorkloadSummary) bool {
	if float64(s.ShmemMax) <= ShmemShare*float64(s.WorkingSetMax) {
		return true
	}
	return len(in.Machine.Swaps) > 0
}

// heapShare is the part of a workload's memory limit a Java container's heap
// may take, given as -XX:MaxRAMPercentage, leaving the rest for the
// runtime's own memory, thread stacks and buffers. It matches the
// helper's bound for Java, helperapi.MaxHeap.
const heapShare = 0.75

// ArenaThreads is the thread count from which a glibc program gets
// MALLOC_ARENA_MAX=2: glibc makes up to eight malloc arenas per CPU for
// threaded programs, and each holds on to freed memory.
const ArenaThreads = 4

// heapCaps proposes heap caps for Java, Node.js and Go workloads that have a
// memory limit, either set already or proposed, and MALLOC_ARENA_MAX for
// threaded Python and Ruby. Java and Node.js programs started with heap
// flags of their own are left alone. A heap cap is relative to a limit;
// without one it would be relative to the whole box. Services get the
// setting through a drop-in; for a container it is advice, since changing a
// container's environment means making the container again. Units a fix may
// never touch are skipped, as the helper would refuse them.
func heapCaps(in Input, proposed []Limit) []Heap {
	limits := make(map[string]uint64)
	for _, l := range proposed {
		limits[l.WorkloadID] = l.Max
	}
	var out []Heap
	for _, id := range sortedIDs(in.Workloads) {
		w := in.Workloads[id]
		switch {
		case w.Kind == model.KindService && helperapi.ProtectedUnit(w.Name):
			continue
		case w.Kind != model.KindService && w.Kind != model.KindContainer:
			continue
		}
		limit := limits[id]
		if w.Max.Set {
			limit = w.Max.Bytes
		}
		h := Heap{WorkloadID: id, Name: w.Label(), Runtime: w.Runtime, Limit: limit}
		if w.Kind == model.KindService {
			h.Unit = w.Name
		}
		heap := helperapi.MaxHeap(helperv1.Runtime_RUNTIME_JAVA, limit)
		switch {
		case (w.Runtime == "java" || w.Runtime == "node") && (len(w.HeapFlags) > 0 || heap < helperapi.MinHeap):
			continue
		case w.Runtime == "java" && h.Unit != "":
			// Outside a container the JVM sizes its heap from the whole
			// box, not from the service's MemoryMax, so the cap is a size.
			h.Env, h.Setting, h.Bytes = "JAVA_TOOL_OPTIONS", "-Xmx"+strconv.FormatUint(heap/model.MiB, 10)+"m", heap
		case w.Runtime == "java":
			h.Env, h.Setting = "JAVA_TOOL_OPTIONS", "-XX:MaxRAMPercentage="+strconv.Itoa(int(heapShare*100))
		case w.Runtime == "node":
			h.Env, h.Setting, h.Bytes = "NODE_OPTIONS", "--max-old-space-size="+strconv.FormatUint(heap/model.MiB, 10), heap
		case w.Runtime == "go":
			heap = helperapi.MaxHeap(helperv1.Runtime_RUNTIME_GO, limit)
			if heap < helperapi.MinHeap {
				continue
			}
			h.Env, h.Setting, h.Bytes = "GOMEMLIMIT", strconv.FormatUint(heap/model.MiB, 10)+"MiB", heap
		case (w.Runtime == "python" || w.Runtime == "ruby") && w.Threads >= ArenaThreads:
			h.Env, h.Setting = "MALLOC_ARENA_MAX", "2"
		case w.Runtime == "php-fpm":
			n := FPMWorkers(limit, w.WorkerBytes)
			if n == 0 {
				continue
			}
			// Advice only: the line belongs in PHP-FPM's own pool file,
			// which Snugbox never edits (docs/adr/0012).
			h.Unit, h.Setting = "", "pm.max_children = "+strconv.FormatUint(n, 10)
			h.Workers, h.WorkerBytes = w.Workers, w.WorkerBytes
		default:
			continue
		}
		out = append(out, h)
	}
	return out
}

// FPMShare is the part of a PHP-FPM pool's memory limit its workers may fill,
// leaving the rest for the master and for a request that grows.
const FPMShare = 0.9

// FPMWorkers is how many PHP-FPM workers of the largest size seen fit in the
// pool's limit, or zero when either is unknown or not one fits.
func FPMWorkers(limit, worker uint64) uint64 {
	if limit == 0 || worker == 0 {
		return 0
	}
	return uint64(float64(limit)*FPMShare) / worker
}

func sortedIDs(ws map[string]model.Workload) []string {
	ids := make([]string, 0, len(ws))
	for id := range ws {
		ids = append(ids, id)
	}
	slices.Sort(ids)
	return ids
}

func scale(b uint64, f float64) uint64 {
	v := float64(b) * f
	n := uint64(v)
	if float64(n) < v {
		n++
	}
	return n
}

func roundUp(b, step uint64) uint64 {
	if b%step == 0 {
		return b
	}
	return (b/step + 1) * step
}
