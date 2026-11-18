package helper

import (
	"errors"
	"fmt"
	"io/fs"
	"time"

	"snugbox.dev/snugbox/agent/internal/helperapi"
	"snugbox.dev/snugbox/agent/internal/kernel"
	helperv1 "snugbox.dev/snugbox/gen/snugbox/helper/v1"
)

// When the box counts as under memory pressure, for rule 1 of blueprint
// section 3.7. Starting values for the outside memory review: any stall of
// every task in the last 10 seconds, or tasks stalled on memory for more than
// 1% of the last minute.
const (
	PressureFullAvg10 = 0.1
	PressureSomeAvg60 = 1.0
	// KillQuiet is how long after an out-of-memory kill nothing runs.
	KillQuiet = 10 * time.Minute
	// SwapoffMargin is how much more free memory than swap holds must be
	// free before swap is turned off, blueprint section 3.7.
	SwapoffMargin = 1.2
)

func pressure(code helperv1.ErrorCode, format string, args ...any) error {
	return helperapi.Errorf(code, format, args...)
}

// calm returns nil when a fix or an automatic undo may run: no pause, no
// memory pressure and no out-of-memory kill in the last 10 minutes.
func (e *Engine) calm(withPause bool) error {
	if withPause && e.paused() {
		return pressure(helperv1.ErrorCode_ERROR_CODE_PAUSED, "snugbox pause is in force; run sudo snugbox resume first")
	}
	b, err := e.sys.ReadFile("/proc/pressure/memory")
	if errors.Is(err, fs.ErrNotExist) {
		return pressure(helperv1.ErrorCode_ERROR_CODE_PRECONDITION, "this kernel has pressure stall information off, and fixes need it to run safely; boot with psi=1, which snugbox doctor explains")
	}
	if err != nil {
		return pressure(helperv1.ErrorCode_ERROR_CODE_PRECONDITION, "can't read memory pressure, which fixes need to run safely: %v", err)
	}
	p, err := kernel.ParsePressure(b)
	if err != nil {
		return pressure(helperv1.ErrorCode_ERROR_CODE_PRECONDITION, "can't read memory pressure: %v", err)
	}
	switch {
	case p.Full.Avg10 > PressureFullAvg10:
		return pressure(helperv1.ErrorCode_ERROR_CODE_PRESSURE,
			"the box is under memory pressure (every task stalled %.2f%% of the last 10 seconds); try again when it is quieter", p.Full.Avg10)
	case p.Some.Avg60 > PressureSomeAvg60:
		return pressure(helperv1.ErrorCode_ERROR_CODE_PRESSURE,
			"the box is under memory pressure (tasks stalled %.1f%% of the last minute); try again when it is quieter", p.Some.Avg60)
	}
	kills, err := e.sys.Kills()
	if err != nil {
		return pressure(helperv1.ErrorCode_ERROR_CODE_PRECONDITION, "can't read the kernel log for recent out-of-memory kills: %v", err)
	}
	now := e.sys.Now()
	for _, at := range kills {
		if ago := now.Sub(at); ago >= 0 && ago < KillQuiet {
			return pressure(helperv1.ErrorCode_ERROR_CODE_PRESSURE,
				"the kernel killed a process for lack of memory %s ago; fixes wait until 10 minutes have passed", ago.Round(time.Second))
		}
	}
	return nil
}

func (e *Engine) paused() bool {
	_, err := e.sys.ReadFile(e.pause)
	return !errors.Is(err, fs.ErrNotExist)
}

// swapoffSafe checks that turning a swap area off can't set off the
// out-of-memory killer: the box is calm and has free memory for everything
// in swap with 20% to spare.
func (e *Engine) swapoffSafe(path string) error {
	if err := e.calm(false); err != nil {
		var he *helperapi.Error
		if errors.As(err, &he) && he.Code == helperv1.ErrorCode_ERROR_CODE_PRESSURE {
			return fmt.Errorf("turning swap off now could set off the out-of-memory killer: %s", he.Message)
		}
		return err
	}
	raw, err := e.sys.ReadFile("/proc/swaps")
	if err != nil {
		return err
	}
	swaps, err := kernel.ParseSwaps(raw)
	if err != nil {
		return err
	}
	var used uint64
	for _, s := range swaps {
		if s.Filename == path {
			used = s.Used
		}
	}
	if used == 0 {
		return nil
	}
	mi, err := e.sys.ReadFile("/proc/meminfo")
	if err != nil {
		return err
	}
	stat, _ := kernel.ParseMeminfo(mi)
	if free := stat.Get("MemAvailable"); float64(free) < SwapoffMargin*float64(used) {
		return fmt.Errorf("swap holds %s and only %s is free, so turning it off now could set off the out-of-memory killer; try again when more memory is free",
			sizeText(used), sizeText(free))
	}
	return nil
}

func sizeText(b uint64) string {
	if b >= helperapi.GiB {
		return fmt.Sprintf("%.1f GiB", float64(b)/float64(helperapi.GiB))
	}
	return fmt.Sprintf("%d MiB", b/helperapi.MiB)
}
