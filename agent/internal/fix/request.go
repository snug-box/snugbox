package fix

import (
	"fmt"
	"slices"

	"snugbox.dev/snugbox/agent/internal/docker"
	"snugbox.dev/snugbox/agent/internal/helperapi"
	"snugbox.dev/snugbox/agent/internal/model"
	helperv1 "snugbox.dev/snugbox/gen/snugbox/helper/v1"
)

// RequestOptions narrow what a proposal asks the helper for.
type RequestOptions struct {
	// Only keeps the limits and heap caps of the workloads with these names.
	// Empty keeps all of them.
	Only []string
	// Persist asks for a Compose override file next to each container's
	// project, so the limits outlive the container.
	Persist bool
}

// Request turns a proposal into the helper's message for it. A heap cap for
// a container can't be applied, since that means making the container again;
// it comes back as advice for the person instead.
func Request(p Proposal, ws map[string]model.Workload, o RequestOptions) (*helperv1.Fix, []string, error) {
	keep := func(name string) bool { return len(o.Only) == 0 || slices.Contains(o.Only, name) }
	var advice []string
	switch p.Kind {
	case KindSwap:
		s := &helperv1.SwapFix{SizeBytes: p.SwapSize}
		if p.Zswap {
			s.Compressor, s.Shrinker = helperv1.Compressor_COMPRESSOR_ZSTD, p.Shrinker
			if p.Compressor == "lz4" {
				s.Compressor = helperv1.Compressor_COMPRESSOR_LZ4
			}
		}
		return &helperv1.Fix{Kind: &helperv1.Fix_Swap{Swap: s}}, nil, nil
	case KindOOMProtection:
		daemon := helperv1.OOMDaemon_OOM_DAEMON_SYSTEMD_OOMD
		if p.OOMDaemon == DaemonEarlyoom {
			daemon = helperv1.OOMDaemon_OOM_DAEMON_EARLYOOM
		}
		return &helperv1.Fix{Kind: &helperv1.Fix_OomProtection{OomProtection: &helperv1.OOMProtectionFix{Daemon: daemon}}}, nil, nil
	case KindContainer:
		c := &helperv1.ContainerLimitsFix{Persist: o.Persist}
		for _, l := range p.Limits {
			if !keep(l.Name) {
				continue
			}
			id := ws[l.WorkloadID].ContainerID
			if !docker.ValidID(id) {
				return nil, nil, fmt.Errorf("container %s has no full ID in the watcher's records", l.Name)
			}
			c.Limits = append(c.Limits, &helperv1.ContainerLimit{ContainerId: id, MaxBytes: l.Max, ReservationBytes: l.Soft / model.MiB * model.MiB})
		}
		if len(c.Limits) == 0 {
			return nil, nil, errNoneLeft(o.Only)
		}
		return &helperv1.Fix{Kind: &helperv1.Fix_ContainerLimits{ContainerLimits: c}}, nil, nil
	case KindService:
		s := &helperv1.ServiceLimitsFix{}
		for _, l := range p.Limits {
			if keep(l.Name) {
				s.Limits = append(s.Limits, &helperv1.ServiceLimit{Unit: l.Name, HighBytes: l.Soft, MaxBytes: l.Max})
			}
		}
		if len(s.Limits) == 0 {
			return nil, nil, errNoneLeft(o.Only)
		}
		return &helperv1.Fix{Kind: &helperv1.Fix_ServiceLimits{ServiceLimits: s}}, nil, nil
	case KindHeap:
		h := &helperv1.HeapCapsFix{}
		for _, c := range p.Heaps {
			if !keep(c.Name) {
				continue
			}
			if c.Unit == "" {
				advice = append(advice, Advice(c))
				continue
			}
			h.Caps = append(h.Caps, &helperv1.HeapCap{Unit: c.Unit, Runtime: runtimeOf(c), HeapBytes: c.Bytes})
		}
		if len(h.Caps) == 0 {
			if len(advice) > 0 {
				return nil, advice, nil
			}
			return nil, nil, errNoneLeft(o.Only)
		}
		return &helperv1.Fix{Kind: &helperv1.Fix_HeapCaps{HeapCaps: h}}, advice, nil
	case KindSchedule:
		s := &helperv1.ScheduleFix{}
		for _, sh := range p.Shifts {
			if keep(sh.Service) || keep(sh.Timer) {
				s.Shifts = append(s.Shifts, &helperv1.TimerShift{Timer: sh.Timer, Service: sh.Service, Minutes: uint32(min(max(sh.Minutes, 1), helperapi.MaxShiftMinutes))})
			}
		}
		for _, c := range p.Cron {
			if keep(c.Program) || keep(c.File) {
				advice = append(advice, CronAdvice(c))
			}
		}
		if len(s.Shifts) == 0 {
			if len(advice) > 0 {
				return nil, advice, nil
			}
			return nil, nil, errNoneLeft(o.Only)
		}
		return &helperv1.Fix{Kind: &helperv1.Fix_Schedule{Schedule: s}}, advice, nil
	}
	return nil, nil, fmt.Errorf("%s is not a fix Snugbox can apply", p.Kind)
}

// Advice is what a person does by hand for a heap cap Snugbox can't apply:
// a container's environment, or PHP-FPM's pool file.
func Advice(h Heap) string {
	if h.Runtime == "php-fpm" {
		return fmt.Sprintf("%s: set %s in the pool's file, such as www.conf, then reload PHP-FPM. "+
			"That many workers of the largest size seen, %d MiB, fit in %d%% of its %d MiB limit; %d run now.",
			h.Name, h.Setting, h.WorkerBytes/model.MiB, int(FPMShare*100), h.Limit/model.MiB, h.Workers)
	}
	return fmt.Sprintf("Container %s: set %s=%s in its environment and make the container again.", h.Name, h.Env, h.Setting)
}

func runtimeOf(h Heap) helperv1.Runtime {
	switch h.Env {
	case "JAVA_TOOL_OPTIONS":
		return helperv1.Runtime_RUNTIME_JAVA
	case "NODE_OPTIONS":
		return helperv1.Runtime_RUNTIME_NODE
	case "GOMEMLIMIT":
		return helperv1.Runtime_RUNTIME_GO
	}
	return helperv1.Runtime_RUNTIME_GLIBC
}

func errNoneLeft(only []string) error {
	if len(only) > 0 {
		return fmt.Errorf("the fix covers none of %v", only)
	}
	return fmt.Errorf("the fix has nothing left to apply")
}
