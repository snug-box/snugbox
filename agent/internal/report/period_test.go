package report

import (
	"bytes"
	"strings"
	"testing"
	"time"

	"snugbox.dev/snugbox/agent/internal/localapi"
	"snugbox.dev/snugbox/agent/internal/plan"
)

func periodReport() localapi.Report {
	from := time.Date(2026, 10, 30, 9, 0, 0, 0, time.UTC)
	to := from.Add(7 * 24 * time.Hour)
	return localapi.Report{
		From: from, To: to, WatchStart: from.Add(-30 * 24 * time.Hour),
		Then: &localapi.Snapshot{Snug: 3 << 30, Confidence: "high"},
		Now:  &localapi.Snapshot{Snug: 2 << 30, Confidence: "high"},
		Events: []localapi.Event{
			{Time: from.Add(time.Hour), Kind: "oom_kill", Workload: "app.service"},
			{Time: from.Add(2 * time.Hour), Kind: "oom_kill", Workload: "app.service"},
			{Time: from.Add(3 * time.Hour), Kind: "oom_kill", Workload: "db.service"},
			{Time: from.Add(4 * time.Hour), Kind: "warning", Workload: "app.service"},
			{Time: from.Add(5 * time.Hour), Kind: "warning_outcome", Workload: "app.service", Detail: "nothing followed"},
			{Time: from.Add(6 * time.Hour), Kind: "fix_undone", Workload: "20261031-103015-service-limits"},
		},
		Throttled: map[string]int{"app.service": 14},
		Changes: localapi.Changes{Helper: true, Changes: []localapi.Change{
			{ID: "20261102-101500-swap", Kind: "swap", State: "verifying", Started: to.Add(-time.Hour), Since: to.Add(-time.Hour), VerifyEnds: to.Add(-30 * time.Minute)},
			{ID: "20261031-103015-service-limits", Kind: "service-limits", State: "undone", Started: from.Add(time.Hour), Since: from.Add(2 * time.Hour),
				Reason: "app.service restarted 2 times in verification; Snugbox put the limits back"},
			{ID: "20261001-090000-oom-protection", Kind: "oom-protection", State: "done", Started: from.Add(-29 * 24 * time.Hour), Since: from.Add(-20 * 24 * time.Hour), Counted: true},
		}},
	}
}

func TestPeriod(t *testing.T) {
	c, err := plan.Embedded()
	if err != nil {
		t.Fatal(err)
	}
	rep := periodReport()
	prov, _ := c.Provider("digitalocean")
	big, small := prov.Plans[len(prov.Plans)-1], prov.Plans[0]
	rep.Resizes = []localapi.Resize{{Time: rep.From.Add(24 * time.Hour), OldRAM: big.RAM, NewRAM: small.RAM, OldCPUs: big.VCPUs, NewCPUs: small.VCPUs,
		Provider: "digitalocean", OldPlan: big.ID, NewPlan: small.ID, OldCents: int64(c.Total(big)), NewCents: int64(c.Total(small))}}
	var b bytes.Buffer
	if err := Period(&b, rep, shopResult(t), Painter{}); err != nil {
		t.Fatal(err)
	}
	out := b.String()
	saving := c.Total(big) - c.Total(small)
	for _, want := range []string{
		"SINCE\n  " + Stamp(rep.From.Local()) + ", the last 7 days.",
		"Needs " + GiB(2<<30) + " now, high confidence. It needed " + GiB(3<<30) + " at the start.",
		"This box pays for",
		"SAVED\n  " + saving.String() + " a month, " + (12 * saving).String() + " a year.",
		"from " + PlanName(c, big) + " (" + c.Total(big).String() + " a month) to " + PlanName(c, small) + " (" + c.Total(small).String() + " a month).",
		"20261102-101500-swap  swap, verifying since",
		"20261031-103015-service-limits  service-limits, undone since " + Stamp(rep.From.Add(2*time.Hour).Local()) + ": app.service restarted 2 times",
		"3 out-of-memory kills: app.service 2 and db.service 1.",
		"Held back at memory.high in 14 samples: app.service 14.",
		"1 early warning; nothing followed 1 of them.",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("the report has no %q:\n%s", want, out)
		}
	}
	// A fix done before the period isn't news, and fix events are told
	// under FIXES only.
	if strings.Contains(out, "oom-protection") || strings.Contains(out, "fix_undone") {
		t.Errorf("the report lists what happened before the period:\n%s", out)
	}
}

func TestPeriodWithNothingToTell(t *testing.T) {
	rep := localapi.Report{From: time.Date(2026, 11, 1, 9, 0, 0, 0, time.UTC), To: time.Date(2026, 11, 2, 9, 0, 0, 0, time.UTC),
		WatchStart: time.Date(2026, 11, 1, 18, 0, 0, 0, time.UTC),
		Resizes:    []localapi.Resize{{Time: time.Date(2026, 11, 1, 19, 0, 0, 0, time.UTC), OldRAM: 2 << 30, NewRAM: 4 << 30, OldCPUs: 1, NewCPUs: 1}},
		Changes:    localapi.Changes{Error: "snugboxd runs without snugbox-helper"}}
	var b bytes.Buffer
	if err := Period(&b, rep, nil, Painter{}); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"the last 24 hours.", "The watcher has history only from " + Stamp(rep.WatchStart.Local()),
		"No analysis yet",
		"Nothing yet. A saving counts once the box moves to a cheaper plan",
		"Its plans aren't known, so no saving is counted.",
		"The fixes can't be listed: snugboxd runs without snugbox-helper.",
		"No out-of-memory kill, early warning or memory.high hit in this time.",
	} {
		if !strings.Contains(b.String(), want) {
			t.Errorf("the report has no %q:\n%s", want, b.String())
		}
	}
}

func TestPeriodOfAnUpgrade(t *testing.T) {
	rep := periodReport()
	rep.Resizes = []localapi.Resize{{Time: rep.From, OldRAM: 2 << 30, NewRAM: 4 << 30, OldCPUs: 1, NewCPUs: 2, Provider: "digitalocean",
		OldPlan: "x", NewPlan: "y", OldCents: 1200, NewCents: 2400}}
	var b bytes.Buffer
	if err := Period(&b, rep, nil, Painter{}); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(b.String(), "Nothing saved: the box moved to a larger plan, which costs $12.00 more a month.") ||
		!strings.Contains(b.String(), "from a plan Snugbox doesn't know to a plan Snugbox doesn't know") {
		t.Errorf("an upgrade:\n%s", b.String())
	}
}

func TestPeriodSince(t *testing.T) {
	now := time.Date(2026, 11, 1, 9, 0, 0, 0, time.UTC)
	if got := PeriodSince(now, false); !got.Equal(now.Add(-24 * time.Hour)) {
		t.Errorf("a day: %v", got)
	}
	if got := PeriodSince(now, true); !got.Equal(now.Add(-7 * 24 * time.Hour)) {
		t.Errorf("a week: %v", got)
	}
}
