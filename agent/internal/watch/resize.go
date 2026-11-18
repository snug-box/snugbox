package watch

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"snugbox.dev/snugbox/agent/internal/model"
	"snugbox.dev/snugbox/agent/internal/plan"
	"snugbox.dev/snugbox/agent/internal/store"
)

// boxFacts is what the watcher remembers of the box between runs, so it
// sees a resize that happened while it was stopped, as most do: a provider
// resizes a server while it is off.
type boxFacts struct {
	MemTotal uint64 `json:"mem_total"`
	CPUs     int    `json:"cpus"`
	Provider string `json:"provider,omitempty"`
}

const factsKey = "box"

func (d *Daemon) facts() boxFacts {
	d.mu.RLock()
	defer d.mu.RUnlock()
	return boxFacts{MemTotal: d.machine.MemTotal, CPUs: d.machine.CPUs, Provider: d.prov.Provider}
}

// checkResize compares the box with what the watcher saw of it before it
// last stopped, records a resize when its RAM or CPU count changed in
// between, and remembers the box as it is now.
func (d *Daemon) checkResize(ctx context.Context, now time.Time) {
	cur := d.facts()
	if cur.MemTotal == 0 {
		return
	}
	raw, err := d.st.Meta(ctx, factsKey)
	if err != nil {
		d.deps.Log.Error("could not read the box's size from before", "err", err)
		return
	}
	var old boxFacts
	if raw != "" && json.Unmarshal([]byte(raw), &old) != nil {
		old = boxFacts{}
	}
	if old.MemTotal > 0 && (old.MemTotal != cur.MemTotal || old.CPUs != cur.CPUs) {
		d.recordResize(ctx, now, old, cur)
	}
	if old != cur {
		d.remember(ctx, cur)
	}
}

func (d *Daemon) remember(ctx context.Context, f boxFacts) {
	b, err := json.Marshal(f)
	if err == nil {
		err = d.st.SetMeta(ctx, factsKey, string(b))
	}
	if err != nil {
		d.deps.Log.Error("could not remember the box's size", "err", err)
	}
}

// recordResize stores a resize, with the plans that match the box before and
// after it when the provider is known and stayed the same, and records an
// event.
func (d *Daemon) recordResize(ctx context.Context, now time.Time, old, cur boxFacts) {
	r := store.Resize{Time: now, OldRAM: old.MemTotal, NewRAM: cur.MemTotal, OldCPUs: old.CPUs, NewCPUs: cur.CPUs, Provider: cur.Provider}
	if cur.Provider != "" && old.Provider == cur.Provider {
		if c, err := plan.Embedded(); err == nil {
			if p, ok := c.Current(plan.Box{ProviderID: cur.Provider, MemTotal: old.MemTotal, CPUs: old.CPUs}); ok {
				r.OldPlan, r.OldCents = p.ID, int64(c.Total(p))
			}
			if p, ok := c.Current(plan.Box{ProviderID: cur.Provider, MemTotal: cur.MemTotal, CPUs: cur.CPUs}); ok {
				r.NewPlan, r.NewCents = p.ID, int64(c.Total(p))
			}
		}
	}
	if err := d.st.AddResize(ctx, r); err != nil {
		d.deps.Log.Error("could not record a resize", "err", err)
	}
	var what []string
	if old.MemTotal != cur.MemTotal {
		what = append(what, fmt.Sprintf("MemTotal went from %d MiB to %d MiB", old.MemTotal/model.MiB, cur.MemTotal/model.MiB))
	}
	if old.CPUs != cur.CPUs {
		what = append(what, fmt.Sprintf("the CPUs from %d to %d", old.CPUs, cur.CPUs))
	}
	d.event(ctx, now, "ram_changed", "", strings.Join(what, ", and ")+"; the advice is worked out again")
}
