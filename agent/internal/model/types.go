// Package model holds Snugbox's memory model: what a workload uses, what a box
// needs, and how sure Snugbox is. The rules are those of blueprint section 3.6,
// published in docs/memory-model.md so anyone can check the arithmetic.
//
// The package is pure. It reads no files and keeps no clocks, so the same
// samples always give the same answer.
package model

import "time"

// Binary units. Snugbox stores and compares memory in bytes and prints it in
// MiB and GiB.
const (
	KiB uint64 = 1 << 10
	MiB uint64 = 1 << 20
	GiB uint64 = 1 << 30
)

// Kind says what sort of thing a workload is.
type Kind string

// The workload kinds of blueprint section 3.10.
const (
	KindContainer Kind = "container"
	KindService   Kind = "service"
	KindGroup     Kind = "group"
)

// Usage is one reading of a workload's cgroup files, in bytes unless noted.
// Counters marked cumulative only ever grow while the cgroup lives; a smaller
// value means the cgroup was recreated.
type Usage struct {
	Current           uint64
	Anon              uint64
	Shmem             uint64
	ActiveFile        uint64
	InactiveFile      uint64
	KernelStack       uint64
	PageTables        uint64
	Percpu            uint64
	SlabReclaimable   uint64
	SlabUnreclaimable uint64
	Vmalloc           uint64
	Sock              uint64
	Zswap             uint64
	Swap              uint64
	// RefaultFile counts file pages that were evicted and then needed again,
	// cumulative, in pages.
	RefaultFile uint64
	// PressureSome and PressureFull are the cgroup's stall totals, cumulative.
	PressureSome time.Duration
	PressureFull time.Duration
	// OOMKills is memory.events oom_kill, cumulative.
	OOMKills uint64
	// HighEvents counts times the cgroup went over memory.high, cumulative.
	HighEvents uint64
	// Peak is memory.peak, the highest memory.current since the cgroup was
	// created. Linux 5.19 added it.
	Peak    uint64
	HasPeak bool
	// CPU is cpu.stat's usage_usec, the CPU time the cgroup has used,
	// cumulative. HasCPU is false when the file couldn't be read.
	CPU    time.Duration
	HasCPU bool
}

// Hard is memory that can only leave RAM through swap, or not at all:
// anonymous memory, shared memory and tmpfs, the kernel memory charged to the
// workload that reclaim can't drop, and the compressed zswap pool.
func (u Usage) Hard() uint64 {
	return u.Anon + u.Shmem + u.KernelStack + u.PageTables + u.Percpu +
		u.SlabUnreclaimable + u.Vmalloc + u.Sock + u.Zswap
}

// chargedKernel is the part of the workload's charge that /proc/meminfo also
// counts under Slab, PageTables and KernelStack.
func (u Usage) chargedKernel() uint64 {
	return u.SlabReclaimable + u.SlabUnreclaimable + u.PageTables + u.KernelStack
}

// BoxUsage is one reading of the box-wide files: /proc/meminfo, /proc/vmstat,
// /proc/pressure/memory and /proc/stat.
type BoxUsage struct {
	MemTotal      uint64
	MemAvailable  uint64
	Slab          uint64
	PageTables    uint64
	KernelStack   uint64
	AnonHugePages uint64
	SwapTotal     uint64
	SwapFree      uint64
	// PressureSome and PressureFull are cumulative since boot. HasPressure is
	// false on kernels without pressure stall information.
	PressureSome time.Duration
	PressureFull time.Duration
	HasPressure  bool
	// SwapIns is pswpin, pages read back from swap, cumulative since boot.
	SwapIns uint64
	// OOMKills is the vmstat oom_kill counter, cumulative since boot.
	OOMKills uint64
	// CPUBusy and CPUTotal are /proc/stat ticks, cumulative since boot.
	CPUBusy  uint64
	CPUTotal uint64
}

// Sample is everything read at one moment.
type Sample struct {
	Time      time.Time
	Box       BoxUsage
	Workloads map[string]Usage
}
