package helperapi

import (
	"fmt"
	"regexp"
	"strings"
	"unicode"

	helperv1 "snugbox.dev/snugbox/gen/snugbox/helper/v1"
)

// Byte units for the bounds.
const (
	MiB uint64 = 1 << 20
	GiB uint64 = 1 << 30
)

// Bounds from blueprint section 3.7. Limits move in 64 MiB steps; swap is at
// most half of RAM and never more than 4 GiB.
const (
	LimitStep    = 64 * MiB
	MinLimit     = 64 * MiB
	MinSwap      = 128 * MiB
	MaxSwap      = 4 * GiB
	MinHeap      = 32 * MiB
	MaxLimits    = 32
	MaxHeapCaps  = 16
	MaxReason    = 200
	MaxUnitName  = 256
	maxTxPattern = 64
)

// A schedule fix moves 1 to MaxShifts timers, each 1 to MaxShiftMinutes
// earlier.
const (
	MaxShifts       = 16
	MaxShiftMinutes = 120
)

// ScheduleDropIn is the file a schedule fix writes in a timer's drop-in
// folder under /etc/systemd/system. The audit looks for it to tell that a
// timer has been moved already.
const ScheduleDropIn = "50-snugbox-schedule.conf"

// Error is a refusal or a failure the helper reports to its client.
type Error struct {
	Code    helperv1.ErrorCode
	Message string
}

func (e *Error) Error() string { return e.Message }

// Errorf returns an *Error with a formatted message.
func Errorf(code helperv1.ErrorCode, format string, args ...any) *Error {
	return &Error{Code: code, Message: fmt.Sprintf(format, args...)}
}

func invalid(format string, args ...any) *Error {
	return Errorf(helperv1.ErrorCode_ERROR_CODE_INVALID, format, args...)
}

// Fix kinds as they appear in transaction IDs, the journal and the command
// line.
const (
	KindSwap      = "swap"
	KindOOM       = "oom-protection"
	KindContainer = "container-limits"
	KindService   = "service-limits"
	KindHeap      = "heap-caps"
	KindSchedule  = "schedule"
)

// KindOf names the kind of a fix, or "" when none is set.
func KindOf(f *helperv1.Fix) string {
	switch f.GetKind().(type) {
	case *helperv1.Fix_Swap:
		return KindSwap
	case *helperv1.Fix_OomProtection:
		return KindOOM
	case *helperv1.Fix_ContainerLimits:
		return KindContainer
	case *helperv1.Fix_ServiceLimits:
		return KindService
	case *helperv1.Fix_HeapCaps:
		return KindHeap
	case *helperv1.Fix_Schedule:
		return KindSchedule
	}
	return ""
}

var (
	txPattern     = regexp.MustCompile(`^[0-9]{8}-[0-9]{6}-(swap|oom-protection|container-limits|service-limits|heap-caps|schedule)$`)
	unitPattern   = regexp.MustCompile(`^[A-Za-z0-9:_.\\@-]+\.service$`)
	timerPattern  = regexp.MustCompile(`^[A-Za-z0-9:_.\\@-]+\.timer$`)
	containerPatt = regexp.MustCompile(`^[0-9a-f]{64}$`)
)

// ValidTransaction reports whether id has the form the helper gives
// transactions, such as 20261031-103015-swap.
func ValidTransaction(id string) bool { return len(id) <= maxTxPattern && txPattern.MatchString(id) }

// ProtectedUnit reports whether a service must never be limited or
// restarted by a fix: remote access, systemd's own services, the user
// session managers, the message bus, the container runtime and Snugbox
// itself. Docker stops every running container when its daemon ends unless
// live restore is on, so a limit that killed dockerd or containerd would
// take every container with it.
func ProtectedUnit(name string) bool {
	switch {
	case name == "ssh.service", name == "sshd.service", name == "dbus.service", name == "dbus-broker.service":
		return true
	case name == "docker.service", name == "containerd.service":
		return true
	case strings.HasPrefix(name, "systemd-"), strings.HasPrefix(name, "user@"), strings.HasPrefix(name, "snugbox"):
		return true
	}
	return false
}

// MaxHeap is the largest heap cap the helper sets under a memory limit, in
// whole MiB: three quarters of the limit for Java and Node.js, whose heap is
// only part of the memory they use, and nine tenths for Go. GOMEMLIMIT
// already counts all the memory the Go runtime manages, and the Go team's
// guide suggests leaving 5 to 10% for the rest (go.dev/doc/gc-guide, read on
// 31 October 2026).
func MaxHeap(rt helperv1.Runtime, limit uint64) uint64 {
	if rt == helperv1.Runtime_RUNTIME_GO {
		return limit / 10 * 9 / MiB * MiB
	}
	return limit / 4 * 3 / MiB * MiB
}

// Box holds what the bounds depend on.
type Box struct {
	MemTotal uint64
}

// Validate checks every field of req against its bounds. It returns nil or
// an *Error with ERROR_CODE_INVALID.
func Validate(req *helperv1.Request, box Box) error {
	switch k := req.GetKind().(type) {
	case *helperv1.Request_Apply:
		return validateFix(k.Apply.GetFix(), box)
	case *helperv1.Request_Undo:
		if id := k.Undo.GetTransaction(); id != "" && !ValidTransaction(id) {
			return invalid("%q is not a transaction ID", clip(id))
		}
		return validReason(k.Undo.GetReason())
	case *helperv1.Request_Advance:
		a := k.Advance
		if !ValidTransaction(a.GetTransaction()) {
			return invalid("%q is not a transaction ID", clip(a.GetTransaction()))
		}
		switch a.GetState() {
		case helperv1.State_STATE_WATCHING, helperv1.State_STATE_DONE:
		case helperv1.State_STATE_REOPENED:
			if a.GetReason() == "" {
				return invalid("reopening a fix needs a reason")
			}
		default:
			return invalid("a transaction can't be moved to %s", a.GetState())
		}
		return validReason(a.GetReason())
	case *helperv1.Request_List, *helperv1.Request_Containers:
		return nil
	}
	return invalid("the request asks for nothing")
}

func validateFix(f *helperv1.Fix, box Box) error {
	if box.MemTotal == 0 {
		return invalid("the box's memory size is unknown")
	}
	switch k := f.GetKind().(type) {
	case *helperv1.Fix_Swap:
		return validateSwap(k.Swap, box)
	case *helperv1.Fix_OomProtection:
		switch k.OomProtection.GetDaemon() {
		case helperv1.OOMDaemon_OOM_DAEMON_UNSPECIFIED, helperv1.OOMDaemon_OOM_DAEMON_SYSTEMD_OOMD, helperv1.OOMDaemon_OOM_DAEMON_EARLYOOM:
			return nil
		}
		return Errorf(helperv1.ErrorCode_ERROR_CODE_INVALID, "unknown out-of-memory daemon %d", k.OomProtection.GetDaemon())
	case *helperv1.Fix_ContainerLimits:
		return validateContainers(k.ContainerLimits, box)
	case *helperv1.Fix_ServiceLimits:
		return validateServices(k.ServiceLimits, box)
	case *helperv1.Fix_HeapCaps:
		return validateHeaps(k.HeapCaps, box)
	case *helperv1.Fix_Schedule:
		return validateSchedule(k.Schedule)
	}
	return invalid("the request names no fix")
}

// SwapCeiling is the largest swap file for a box: half of RAM, at most
// 4 GiB, in whole MiB.
func SwapCeiling(memTotal uint64) uint64 { return min(memTotal/2, MaxSwap) / MiB * MiB }

func validateSwap(s *helperv1.SwapFix, box Box) error {
	size, ceiling := s.GetSizeBytes(), SwapCeiling(box.MemTotal)
	switch {
	case size%MiB != 0:
		return invalid("the swap size must be a whole number of MiB")
	case size < MinSwap || size > ceiling:
		return invalid("the swap size must be from %d MiB to %d MiB on this box", MinSwap/MiB, ceiling/MiB)
	}
	switch s.GetCompressor() {
	case helperv1.Compressor_COMPRESSOR_UNSPECIFIED:
		if s.GetShrinker() {
			return invalid("the zswap shrinker needs zswap")
		}
	case helperv1.Compressor_COMPRESSOR_ZSTD, helperv1.Compressor_COMPRESSOR_LZ4:
	default:
		return invalid("unknown compressor %d", s.GetCompressor())
	}
	return nil
}

func validateContainers(c *helperv1.ContainerLimitsFix, box Box) error {
	ls := c.GetLimits()
	if len(ls) == 0 || len(ls) > MaxLimits {
		return invalid("a container limits fix changes 1 to %d containers", MaxLimits)
	}
	seen := make(map[string]bool, len(ls))
	for _, l := range ls {
		id := l.GetContainerId()
		if !containerPatt.MatchString(id) {
			return invalid("%q is not a full container ID", clip(id))
		}
		if seen[id] {
			return invalid("container %s is listed twice", id[:12])
		}
		seen[id] = true
		if err := validLimit("the limit of container "+id[:12], l.GetMaxBytes(), box); err != nil {
			return err
		}
		if r := l.GetReservationBytes(); r%MiB != 0 || r >= l.GetMaxBytes() {
			return invalid("the soft limit of container %s must be whole MiB below its hard limit", id[:12])
		}
	}
	return nil
}

func validateServices(s *helperv1.ServiceLimitsFix, box Box) error {
	ls := s.GetLimits()
	if len(ls) == 0 || len(ls) > MaxLimits {
		return invalid("a service limits fix changes 1 to %d services", MaxLimits)
	}
	seen := make(map[string]bool, len(ls))
	for _, l := range ls {
		if err := validUnit(l.GetUnit(), seen); err != nil {
			return err
		}
		if err := validLimit("MemoryHigh of "+l.GetUnit(), l.GetHighBytes(), box); err != nil {
			return err
		}
		if err := validLimit("MemoryMax of "+l.GetUnit(), l.GetMaxBytes(), box); err != nil {
			return err
		}
		if l.GetMaxBytes() <= l.GetHighBytes() {
			return invalid("MemoryMax of %s must be above its MemoryHigh", l.GetUnit())
		}
	}
	return nil
}

func validateHeaps(h *helperv1.HeapCapsFix, box Box) error {
	cs := h.GetCaps()
	if len(cs) == 0 || len(cs) > MaxHeapCaps {
		return invalid("a heap caps fix changes 1 to %d services", MaxHeapCaps)
	}
	seen := make(map[string]bool, len(cs))
	for _, c := range cs {
		if err := validUnit(c.GetUnit(), seen); err != nil {
			return err
		}
		heap := c.GetHeapBytes()
		switch c.GetRuntime() {
		case helperv1.Runtime_RUNTIME_GLIBC:
			if heap != 0 {
				return invalid("MALLOC_ARENA_MAX for %s takes no size", c.GetUnit())
			}
		case helperv1.Runtime_RUNTIME_JAVA, helperv1.Runtime_RUNTIME_NODE, helperv1.Runtime_RUNTIME_GO:
			if heap%MiB != 0 || heap < MinHeap || heap > box.MemTotal {
				return invalid("the heap of %s must be whole MiB from %d MiB to the box's RAM", c.GetUnit(), MinHeap/MiB)
			}
		default:
			return invalid("unknown runtime %d for %s", c.GetRuntime(), c.GetUnit())
		}
	}
	return nil
}

func validateSchedule(s *helperv1.ScheduleFix) error {
	shifts := s.GetShifts()
	if len(shifts) == 0 || len(shifts) > MaxShifts {
		return invalid("a schedule fix moves 1 to %d timers", MaxShifts)
	}
	seen := make(map[string]bool, len(shifts))
	for _, sh := range shifts {
		name, service := sh.GetTimer(), sh.GetService()
		switch {
		case len(name) > MaxUnitName || !timerPattern.MatchString(name) || strings.HasPrefix(name, "-"):
			return invalid("%q is not a timer unit name", clip(name))
		case ProtectedUnit(name):
			return invalid("%s is never changed by a fix", name)
		case seen[name]:
			return invalid("%s is listed twice", name)
		case sh.GetMinutes() < 1 || sh.GetMinutes() > MaxShiftMinutes:
			return invalid("%s can be moved 1 to %d minutes earlier", name, MaxShiftMinutes)
		case len(service) > MaxUnitName || !unitPattern.MatchString(service) || strings.HasPrefix(service, "-"):
			return invalid("%q is not a service unit name", clip(service))
		case ProtectedUnit(service):
			return invalid("%s starts %s, which no fix may touch", name, service)
		}
		seen[name] = true
	}
	return nil
}

func validUnit(name string, seen map[string]bool) error {
	switch {
	case len(name) > MaxUnitName || !unitPattern.MatchString(name) || strings.HasPrefix(name, "-"):
		return invalid("%q is not a service unit name", clip(name))
	case ProtectedUnit(name):
		return invalid("%s is never changed by a fix", name)
	case seen[name]:
		return invalid("%s is listed twice", name)
	}
	seen[name] = true
	return nil
}

func validLimit(what string, b uint64, box Box) error {
	if b%LimitStep != 0 || b < MinLimit || b > box.MemTotal {
		return invalid("%s must be a multiple of 64 MiB from 64 MiB to the box's RAM", what)
	}
	return nil
}

func validReason(r string) error {
	if len(r) > MaxReason {
		return invalid("the reason is longer than %d bytes", MaxReason)
	}
	for _, c := range r {
		if !unicode.IsPrint(c) {
			return invalid("the reason holds a character that can't be printed")
		}
	}
	return nil
}

// clip shortens a refused value before it goes into a message.
func clip(s string) string {
	const n = 80
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}
