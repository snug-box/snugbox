package helperapi

import (
	"errors"
	"fmt"
	"strings"
	"testing"

	helperv1 "snugbox.dev/snugbox/gen/snugbox/helper/v1"
)

const (
	cid1 = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
	cid2 = "fedcba9876543210fedcba9876543210fedcba9876543210fedcba9876543210"
)

func apply(f *helperv1.Fix) *helperv1.Request {
	return &helperv1.Request{Kind: &helperv1.Request_Apply{Apply: &helperv1.ApplyRequest{Fix: f}}}
}

func swapFix(size uint64, c helperv1.Compressor, shrinker bool) *helperv1.Request {
	return apply(&helperv1.Fix{Kind: &helperv1.Fix_Swap{Swap: &helperv1.SwapFix{SizeBytes: size, Compressor: c, Shrinker: shrinker}}})
}

func containers(ls ...*helperv1.ContainerLimit) *helperv1.Request {
	return apply(&helperv1.Fix{Kind: &helperv1.Fix_ContainerLimits{ContainerLimits: &helperv1.ContainerLimitsFix{Limits: ls}}})
}

func services(ls ...*helperv1.ServiceLimit) *helperv1.Request {
	return apply(&helperv1.Fix{Kind: &helperv1.Fix_ServiceLimits{ServiceLimits: &helperv1.ServiceLimitsFix{Limits: ls}}})
}

func heaps(cs ...*helperv1.HeapCap) *helperv1.Request {
	return apply(&helperv1.Fix{Kind: &helperv1.Fix_HeapCaps{HeapCaps: &helperv1.HeapCapsFix{Caps: cs}}})
}

func shifts(ss ...*helperv1.TimerShift) *helperv1.Request {
	return apply(&helperv1.Fix{Kind: &helperv1.Fix_Schedule{Schedule: &helperv1.ScheduleFix{Shifts: ss}}})
}

func advance(tx string, s helperv1.State, reason string) *helperv1.Request {
	return &helperv1.Request{Kind: &helperv1.Request_Advance{Advance: &helperv1.AdvanceRequest{Transaction: tx, State: s, Reason: reason}}}
}

func undo(tx, reason string) *helperv1.Request {
	return &helperv1.Request{Kind: &helperv1.Request_Undo{Undo: &helperv1.UndoRequest{Transaction: tx, Reason: reason}}}
}

func TestValidate(t *testing.T) {
	box := Box{MemTotal: 2 * GiB}
	zstd, lz4 := helperv1.Compressor_COMPRESSOR_ZSTD, helperv1.Compressor_COMPRESSOR_LZ4
	none := helperv1.Compressor_COMPRESSOR_UNSPECIFIED
	tx := "20261031-103015-service-limits"
	manyLimits := make([]*helperv1.ContainerLimit, MaxLimits+1)
	for i := range manyLimits {
		manyLimits[i] = &helperv1.ContainerLimit{ContainerId: strings.Repeat("a", 63) + string(rune('a'+i%6)), MaxBytes: 128 * MiB}
	}
	manyShifts := make([]*helperv1.TimerShift, MaxShifts+1)
	for i := range manyShifts {
		manyShifts[i] = &helperv1.TimerShift{Timer: fmt.Sprintf("job%d.timer", i), Service: fmt.Sprintf("job%d.service", i), Minutes: 10}
	}
	cases := []struct {
		name string
		req  *helperv1.Request
		ok   bool
	}{
		{"swap at half of RAM", swapFix(GiB, zstd, true), true},
		{"swap without zswap", swapFix(512*MiB, none, false), true},
		{"swap with lz4", swapFix(768*MiB, lz4, false), true},
		{"swap over half of RAM", swapFix(GiB+MiB, zstd, false), false},
		{"swap below the floor", swapFix(64*MiB, zstd, false), false},
		{"swap not whole MiB", swapFix(512*MiB+1, zstd, false), false},
		{"shrinker without zswap", swapFix(512*MiB, none, true), false},
		{"unknown compressor", swapFix(512*MiB, helperv1.Compressor(9), false), false},
		{"oom protection", apply(&helperv1.Fix{Kind: &helperv1.Fix_OomProtection{OomProtection: &helperv1.OOMProtectionFix{}}}), true},
		{"no fix", apply(&helperv1.Fix{}), false},
		{"no request", &helperv1.Request{}, false},

		{"container limit", containers(&helperv1.ContainerLimit{ContainerId: cid1, MaxBytes: 512 * MiB, ReservationBytes: 300 * MiB}), true},
		{"two containers", containers(&helperv1.ContainerLimit{ContainerId: cid1, MaxBytes: 512 * MiB}, &helperv1.ContainerLimit{ContainerId: cid2, MaxBytes: 64 * MiB}), true},
		{"container twice", containers(&helperv1.ContainerLimit{ContainerId: cid1, MaxBytes: 512 * MiB}, &helperv1.ContainerLimit{ContainerId: cid1, MaxBytes: 640 * MiB}), false},
		{"short container ID", containers(&helperv1.ContainerLimit{ContainerId: cid1[:12], MaxBytes: 512 * MiB}), false},
		{"container name instead of ID", containers(&helperv1.ContainerLimit{ContainerId: "../../containers/x", MaxBytes: 512 * MiB}), false},
		{"container limit off the 64 MiB step", containers(&helperv1.ContainerLimit{ContainerId: cid1, MaxBytes: 500 * MiB}), false},
		{"container limit above RAM", containers(&helperv1.ContainerLimit{ContainerId: cid1, MaxBytes: 4 * GiB}), false},
		{"container limit zero", containers(&helperv1.ContainerLimit{ContainerId: cid1}), false},
		{"reservation at the limit", containers(&helperv1.ContainerLimit{ContainerId: cid1, MaxBytes: 512 * MiB, ReservationBytes: 512 * MiB}), false},
		{"reservation not whole MiB", containers(&helperv1.ContainerLimit{ContainerId: cid1, MaxBytes: 512 * MiB, ReservationBytes: 1000}), false},
		{"too many containers", containers(manyLimits...), false},
		{"no containers", containers(), false},

		{"service limit", services(&helperv1.ServiceLimit{Unit: "app.service", HighBytes: 448 * MiB, MaxBytes: 576 * MiB}), true},
		{"templated service", services(&helperv1.ServiceLimit{Unit: "worker@2.service", HighBytes: 128 * MiB, MaxBytes: 192 * MiB}), true},
		{"escaped service", services(&helperv1.ServiceLimit{Unit: `my\x2dapp.service`, HighBytes: 128 * MiB, MaxBytes: 192 * MiB}), true},
		{"sshd", services(&helperv1.ServiceLimit{Unit: "sshd.service", HighBytes: 128 * MiB, MaxBytes: 192 * MiB}), false},
		{"ssh", services(&helperv1.ServiceLimit{Unit: "ssh.service", HighBytes: 128 * MiB, MaxBytes: 192 * MiB}), false},
		{"systemd service", services(&helperv1.ServiceLimit{Unit: "systemd-journald.service", HighBytes: 128 * MiB, MaxBytes: 192 * MiB}), false},
		{"snugbox itself", services(&helperv1.ServiceLimit{Unit: "snugboxd.service", HighBytes: 128 * MiB, MaxBytes: 192 * MiB}), false},
		{"docker", services(&helperv1.ServiceLimit{Unit: "docker.service", HighBytes: 128 * MiB, MaxBytes: 192 * MiB}), false},
		{"containerd", services(&helperv1.ServiceLimit{Unit: "containerd.service", HighBytes: 128 * MiB, MaxBytes: 192 * MiB}), false},
		{"a slice", services(&helperv1.ServiceLimit{Unit: "system.slice", HighBytes: 128 * MiB, MaxBytes: 192 * MiB}), false},
		{"a path", services(&helperv1.ServiceLimit{Unit: "../../etc/passwd.service", HighBytes: 128 * MiB, MaxBytes: 192 * MiB}), false},
		{"an option", services(&helperv1.ServiceLimit{Unit: "--now.service", HighBytes: 128 * MiB, MaxBytes: 192 * MiB}), false},
		{"max below high", services(&helperv1.ServiceLimit{Unit: "app.service", HighBytes: 192 * MiB, MaxBytes: 128 * MiB}), false},
		{"max equal to high", services(&helperv1.ServiceLimit{Unit: "app.service", HighBytes: 192 * MiB, MaxBytes: 192 * MiB}), false},
		{"service twice", services(&helperv1.ServiceLimit{Unit: "app.service", HighBytes: 128 * MiB, MaxBytes: 192 * MiB}, &helperv1.ServiceLimit{Unit: "app.service", HighBytes: 128 * MiB, MaxBytes: 256 * MiB}), false},
		{"name too long", services(&helperv1.ServiceLimit{Unit: strings.Repeat("a", MaxUnitName) + ".service", HighBytes: 128 * MiB, MaxBytes: 192 * MiB}), false},

		{"java heap", heaps(&helperv1.HeapCap{Unit: "app.service", Runtime: helperv1.Runtime_RUNTIME_JAVA, HeapBytes: 384 * MiB}), true},
		{"glibc arenas", heaps(&helperv1.HeapCap{Unit: "worker.service", Runtime: helperv1.Runtime_RUNTIME_GLIBC}), true},
		{"glibc with a size", heaps(&helperv1.HeapCap{Unit: "worker.service", Runtime: helperv1.Runtime_RUNTIME_GLIBC, HeapBytes: 64 * MiB}), false},
		{"heap below the floor", heaps(&helperv1.HeapCap{Unit: "app.service", Runtime: helperv1.Runtime_RUNTIME_NODE, HeapBytes: 16 * MiB}), false},
		{"heap without a runtime", heaps(&helperv1.HeapCap{Unit: "app.service", HeapBytes: 64 * MiB}), false},
		{"heap on sshd", heaps(&helperv1.HeapCap{Unit: "sshd.service", Runtime: helperv1.Runtime_RUNTIME_GO, HeapBytes: 64 * MiB}), false},

		{"timer moved", shifts(&helperv1.TimerShift{Timer: "backup.timer", Service: "backup.service", Minutes: 15}), true},
		{"templated timer moved", shifts(&helperv1.TimerShift{Timer: "scrub@data.timer", Service: "scrub@data.service", Minutes: MaxShiftMinutes}), true},
		{"two timers", shifts(&helperv1.TimerShift{Timer: "backup.timer", Service: "backup.service", Minutes: 15}, &helperv1.TimerShift{Timer: "dump.timer", Service: "dump.service", Minutes: 10}), true},
		{"timer twice", shifts(&helperv1.TimerShift{Timer: "backup.timer", Service: "backup.service", Minutes: 15}, &helperv1.TimerShift{Timer: "backup.timer", Service: "backup.service", Minutes: 20}), false},
		{"no timers", shifts(), false},
		{"too many timers", shifts(manyShifts...), false},
		{"timer moved no minutes", shifts(&helperv1.TimerShift{Timer: "backup.timer", Service: "backup.service"}), false},
		{"timer moved too far", shifts(&helperv1.TimerShift{Timer: "backup.timer", Service: "backup.service", Minutes: MaxShiftMinutes + 1}), false},
		{"a service as the timer", shifts(&helperv1.TimerShift{Timer: "backup.service", Service: "backup.service", Minutes: 15}), false},
		{"a path as the timer", shifts(&helperv1.TimerShift{Timer: "../../etc/cron.timer", Service: "backup.service", Minutes: 15}), false},
		{"an option as the timer", shifts(&helperv1.TimerShift{Timer: "--now.timer", Service: "backup.service", Minutes: 15}), false},
		{"timer name too long", shifts(&helperv1.TimerShift{Timer: strings.Repeat("a", MaxUnitName) + ".timer", Service: "backup.service", Minutes: 15}), false},
		{"systemd's own timer", shifts(&helperv1.TimerShift{Timer: "systemd-tmpfiles-clean.timer", Service: "systemd-tmpfiles-clean.service", Minutes: 15}), false},
		{"snugbox's own timer", shifts(&helperv1.TimerShift{Timer: "snugbox-report.timer", Service: "backup.service", Minutes: 15}), false},
		{"timer of a protected service", shifts(&helperv1.TimerShift{Timer: "restart-ssh.timer", Service: "sshd.service", Minutes: 15}), false},
		{"timer without its service", shifts(&helperv1.TimerShift{Timer: "backup.timer", Minutes: 15}), false},
		{"timer starting a target", shifts(&helperv1.TimerShift{Timer: "backup.timer", Service: "backup.target", Minutes: 15}), false},

		{"undo the last", undo("", ""), true},
		{"undo one", undo(tx, "the workload restarted 2 times"), true},
		{"undo a made-up ID", undo("../../etc", ""), false},
		{"undo with a newline in the reason", undo(tx, "line\nbreak"), false},
		{"undo with a long reason", undo(tx, strings.Repeat("r", MaxReason+1)), false},
		{"advance to watching", advance(tx, helperv1.State_STATE_WATCHING, ""), true},
		{"advance to done", advance(tx, helperv1.State_STATE_DONE, ""), true},
		{"reopen with a reason", advance(tx, helperv1.State_STATE_REOPENED, "hit MemoryHigh 40 times"), true},
		{"reopen without a reason", advance(tx, helperv1.State_STATE_REOPENED, ""), false},
		{"advance to undone", advance(tx, helperv1.State_STATE_UNDONE, ""), false},
		{"advance without an ID", advance("", helperv1.State_STATE_DONE, ""), false},
		{"list", &helperv1.Request{Kind: &helperv1.Request_List{List: &helperv1.ListRequest{}}}, true},
		{"containers", &helperv1.Request{Kind: &helperv1.Request_Containers{Containers: &helperv1.ContainersRequest{}}}, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := Validate(tc.req, box)
			if tc.ok && err != nil {
				t.Errorf("refused: %v", err)
			}
			if !tc.ok {
				var e *Error
				if !errors.As(err, &e) || e.Code != helperv1.ErrorCode_ERROR_CODE_INVALID {
					t.Errorf("Validate = %v, want an INVALID error", err)
				}
			}
		})
	}
}

func TestValidateNeedsRAM(t *testing.T) {
	if err := Validate(swapFix(512*MiB, helperv1.Compressor_COMPRESSOR_ZSTD, false), Box{}); err == nil {
		t.Error("a fix was accepted without knowing the box's RAM")
	}
}

func TestSwapCeiling(t *testing.T) {
	for _, tc := range []struct{ mem, want uint64 }{
		{2 * GiB, GiB},
		{16 * GiB, 4 * GiB},
		{1000 * MiB, 500 * MiB},
		{1001*MiB + 7, 500 * MiB},
	} {
		if got := SwapCeiling(tc.mem); got != tc.want {
			t.Errorf("SwapCeiling(%d) = %d, want %d", tc.mem, got, tc.want)
		}
	}
}

func TestMaxHeap(t *testing.T) {
	for _, tc := range []struct {
		rt          helperv1.Runtime
		limit, want uint64
	}{
		{helperv1.Runtime_RUNTIME_JAVA, 512 * MiB, 384 * MiB},
		{helperv1.Runtime_RUNTIME_NODE, 1664 * MiB, 1248 * MiB},
		{helperv1.Runtime_RUNTIME_GO, 512 * MiB, 460 * MiB},
		{helperv1.Runtime_RUNTIME_GO, 448 * MiB, 403 * MiB},
		{helperv1.Runtime_RUNTIME_JAVA, 100*MiB + 1, 75 * MiB},
	} {
		if got := MaxHeap(tc.rt, tc.limit); got != tc.want {
			t.Errorf("MaxHeap(%s, %d MiB) = %d bytes, want %d MiB", tc.rt, tc.limit/MiB, got, tc.want/MiB)
		}
	}
}

func TestKindOfAndIDs(t *testing.T) {
	for _, k := range []string{KindSwap, KindOOM, KindContainer, KindService, KindHeap, KindSchedule} {
		if !ValidTransaction("20261031-103015-" + k) {
			t.Errorf("an ID for %s is refused", k)
		}
	}
	for _, id := range []string{"", "20261031-103015", "20261031-103015-swap-2", "2026103-103015-swap", "20261031-103015-reboot"} {
		if ValidTransaction(id) {
			t.Errorf("%q passed as a transaction ID", id)
		}
	}
	if KindOf(swapFix(GiB, helperv1.Compressor_COMPRESSOR_ZSTD, false).GetApply().GetFix()) != KindSwap || KindOf(&helperv1.Fix{}) != "" ||
		KindOf(shifts(&helperv1.TimerShift{}).GetApply().GetFix()) != KindSchedule {
		t.Error("KindOf named the wrong kind")
	}
}
