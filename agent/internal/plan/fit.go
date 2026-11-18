package plan

import (
	"cmp"
	"math"
	"slices"
)

// Fit rules from blueprint section 3.6 and 3.8.
const (
	// UsableShare is the part of a plan's advertised memory assumed usable
	// until the plan has been measured. ASSUMPTION: to be replaced by
	// MemTotal readings from the test fleet and enrolled boxes.
	UsableShare = 0.93
	// DiskMargin and TransferMargin are the spare room a fitting plan needs
	// over the disk and traffic the box uses.
	DiskMargin     = 1.3
	TransferMargin = 1.2
)

// SizeClasses are the common memory sizes, in GiB, the audit rounds to when
// no specific plan applies.
var SizeClasses = []uint64{1, 2, 4, 8, 16, 32, 64, 128, 256, 512}

// Usable is the memory a plan really gives: UsableShare of the advertised
// size, in bytes.
func Usable(advertised uint64) uint64 {
	return uint64(math.Floor(UsableShare * float64(advertised)))
}

// SizeClass returns the smallest common size, in bytes, whose usable memory
// holds snug, and false when even the largest is too small.
func SizeClass(snug uint64) (uint64, bool) {
	for _, gib := range SizeClasses {
		if b := gib << 30; Usable(b) >= snug {
			return b, true
		}
	}
	return 0, false
}

// ClassOf returns the smallest common size, in bytes, at least mem: the size
// a box whose kernel sees mem is most likely sold as. It returns 0 above the
// largest.
func ClassOf(mem uint64) uint64 {
	for _, gib := range SizeClasses {
		if b := gib << 30; b >= mem {
			return b
		}
	}
	return 0
}

// NextClass returns the smallest common size, in bytes, larger than size,
// and false when there is none.
func NextClass(size uint64) (uint64, bool) {
	for _, gib := range SizeClasses {
		if b := gib << 30; b > size {
			return b, true
		}
	}
	return 0, false
}

// Box is what the advisor knows about a box. Zero values mean unknown.
type Box struct {
	ProviderID string
	MemTotal   uint64
	CPUs       int
	// DiskDevice is the size of the block device under /, which stays at the
	// size the server was created with unless its disk was grown.
	DiskDevice uint64
	DiskUsed   uint64
	// CPUQ95 is the 95th percentile of CPU in use, in cores.
	CPUQ95         float64
	EgressMonthly  uint64
	IngressMonthly uint64
}

// Move says what changing to a plan takes.
type Move string

// The moves the advisor names.
const (
	MoveResize            Move = "resize in place"
	MoveResizeAfterShrink Move = "resize after shrinking the disk"
	MoveNewServer         Move = "new server"
	MoveSame              Move = "current plan"
)

// Option is a plan that fits, with what it costs and what moving takes.
type Option struct {
	Plan   Plan
	Total  Money
	Usable uint64
	Move   Move
	// Saving is the monthly difference from the current plan's price, when
	// the current plan is known.
	Saving    Money
	HasSaving bool
}

// Current returns the plan the box appears to be on: a plan of the box's
// provider with the box's vCPU count whose advertised memory is at least
// MemTotal and no more than a fifth above it. The kernel's MemTotal is lower
// than the advertised size because firmware and the kernel keep some.
func (c *Catalogue) Current(b Box) (Plan, bool) {
	prov, ok := c.Provider(b.ProviderID)
	if !ok || b.MemTotal == 0 {
		return Plan{}, false
	}
	var match []Plan
	for _, p := range prov.Plans {
		if p.VCPUs == b.CPUs && p.RAM >= b.MemTotal && float64(p.RAM) <= 1.2*float64(b.MemTotal) {
			match = append(match, p)
		}
	}
	if len(match) == 0 {
		return Plan{}, false
	}
	slices.SortFunc(match, func(a, b Plan) int { return cmp.Compare(a.Monthly, b.Monthly) })
	return match[0], true
}

// Fits reports whether a plan holds the snug size and the box's CPU, disk and
// traffic.
func (c *Catalogue) Fits(p Plan, snug uint64, b Box) bool { return c.fits(p, snug, b, nil) }

// usable is the memory p gives the box. UsableShare stands in for plans
// nobody has measured; the box's own plan is measured, by its MemTotal.
func usable(p Plan, b Box, current *Plan) uint64 {
	if current != nil && b.MemTotal > 0 && p.ProviderID == current.ProviderID && p.ID == current.ID {
		return b.MemTotal
	}
	return Usable(p.RAM)
}

func (c *Catalogue) fits(p Plan, snug uint64, b Box, current *Plan) bool {
	if usable(p, b, current) < snug || float64(p.VCPUs) < b.CPUQ95 {
		return false
	}
	if b.DiskUsed > 0 && float64(p.Disk) < DiskMargin*float64(b.DiskUsed) {
		return false
	}
	traffic := b.EgressMonthly
	if prov, ok := c.Provider(p.ProviderID); ok && prov.TransferCountsInbound {
		traffic += b.IngressMonthly
	}
	return traffic == 0 || float64(p.Transfer) >= TransferMargin*float64(traffic)
}

// Advise returns, for each provider, the cheapest available plan that fits,
// sorted by monthly cost, and separately any cheaper plans that fit but are
// listed as unavailable to order. current is the box's plan when known.
func (c *Catalogue) Advise(snug uint64, b Box, current *Plan) (options, unavailable []Option) {
	for _, prov := range c.Providers {
		var best *Option
		var cheaperUnavailable []Option
		for _, p := range prov.Plans {
			if !c.fits(p, snug, b, current) {
				continue
			}
			o := c.option(p, b, current)
			if !p.Available {
				cheaperUnavailable = append(cheaperUnavailable, o)
				continue
			}
			if best == nil || o.Total < best.Total {
				best = &o
			}
		}
		if best != nil {
			options = append(options, *best)
		}
		for _, o := range cheaperUnavailable {
			if best == nil || o.Total < best.Total {
				unavailable = append(unavailable, o)
			}
		}
	}
	byCost := func(a, b Option) int {
		return cmp.Or(cmp.Compare(a.Total, b.Total), cmp.Compare(a.Plan.ProviderID, b.Plan.ProviderID))
	}
	slices.SortFunc(options, byCost)
	slices.SortFunc(unavailable, byCost)
	return options, unavailable
}

// All returns every plan that fits, available or not, cheapest first. The
// advisor shows one plan per provider; snugbox plan shows them all.
func (c *Catalogue) All(snug uint64, b Box, current *Plan) []Option {
	var out []Option
	for _, prov := range c.Providers {
		for _, p := range prov.Plans {
			if c.fits(p, snug, b, current) {
				out = append(out, c.option(p, b, current))
			}
		}
	}
	slices.SortFunc(out, func(a, b Option) int {
		return cmp.Or(cmp.Compare(a.Total, b.Total), cmp.Compare(a.Plan.ProviderID, b.Plan.ProviderID), cmp.Compare(a.Plan.ID, b.Plan.ID))
	})
	return out
}

func (c *Catalogue) option(p Plan, b Box, current *Plan) Option {
	o := Option{Plan: p, Total: c.Total(p), Usable: usable(p, b, current), Move: c.move(p, b, current)}
	if current != nil {
		o.Saving, o.HasSaving = c.Total(*current)-o.Total, true
	}
	return o
}

func (c *Catalogue) move(p Plan, b Box, current *Plan) Move {
	if current == nil || current.ProviderID != p.ProviderID {
		return MoveNewServer
	}
	if current.ID == p.ID {
		return MoveSame
	}
	prov, _ := c.Provider(p.ProviderID)
	disk := b.DiskDevice
	if disk == 0 {
		disk = current.Disk
	}
	switch prov.Resize {
	case DiskAtLeastCurrent:
		if p.Disk >= disk {
			return MoveResize
		}
		return MoveNewServer
	case ShrinkDiskFirst:
		if p.Disk < disk {
			return MoveResizeAfterShrink
		}
		return MoveResize
	default:
		if p.RAM < current.RAM || prov.ID == "aws" {
			return MoveNewServer
		}
		return MoveResize
	}
}
