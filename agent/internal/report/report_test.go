package report

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"snugbox.dev/snugbox/agent/internal/audit"
	"snugbox.dev/snugbox/agent/internal/collect"
	"snugbox.dev/snugbox/agent/internal/fix"
	"snugbox.dev/snugbox/agent/internal/model"
	"snugbox.dev/snugbox/agent/internal/provider"
	"snugbox.dev/snugbox/agent/internal/testbox"
)

var update = flag.Bool("update", false, "rewrite the golden files")

const mib = model.MiB

type prober provider.Info

func (p prober) Detect(context.Context) provider.Info { return provider.Info(p) }

type containerNames map[string]collect.ContainerMeta

func (n containerNames) Containers(context.Context, []string) (map[string]collect.ContainerMeta, error) {
	return n, nil
}

func id(c byte) string { return strings.Repeat(string(c), 64) }

// shopResult audits a made-up DigitalOcean box with a Node app, a PostgreSQL
// container and a backup service, for one minute on a fake clock.
func shopResult(t *testing.T) *audit.Result {
	t.Helper()
	b := testbox.New()
	b.OOMD = true
	app := b.Add(&testbox.Workload{Path: "system.slice/docker-" + id('a') + ".scope", Programs: []string{"node"}, Anon: 1100 * mib, ActiveFile: 100 * mib, InactiveFile: 300 * mib, Peak: 1600 * mib})
	b.Add(&testbox.Workload{Path: "system.slice/docker-" + id('b') + ".scope", Programs: []string{"postgres"}, Anon: 300 * mib, Shmem: 128 * mib, ActiveFile: 200 * mib, InactiveFile: 400 * mib})
	b.Add(&testbox.Workload{Path: "system.slice/backup.service", Programs: []string{"restic"}, Anon: 300 * mib})
	b.Add(&testbox.Workload{Path: "system.slice/ssh.service", Programs: []string{"sshd"}, Anon: 6 * mib})
	b.Add(&testbox.Workload{Path: "system.slice/cron.service", Programs: []string{"cron"}, Anon: 300 << 10})

	c, err := collect.New(collect.Options{FS: b, PageSize: 4096, Containers: containerNames{id('a'): {Name: "shop-app-1"}, id('b'): {Name: "shop-db-1", Image: "postgres:18"}}})
	if err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 10, 31, 9, 0, 0, 0, time.UTC)
	step := 0
	d := audit.Deps{
		Collector: c,
		Prober:    prober{Provider: provider.DigitalOcean, Region: "fra1", FromMetadata: true},
		Now:       func() time.Time { return now },
		Sleep: func(_ context.Context, d time.Duration) error {
			now = now.Add(d)
			step++
			b.Update(func(*testbox.Box) {
				app.Anon = 1100*mib + uint64(step%5)*20*mib
				b.CPUBusy += 200
				b.CPUIdle += 600
			})
			return nil
		},
		KernelLog: func(time.Time) (collect.KernelLog, error) { return collect.KernelLog{}, errors.New("no log") },
		FS:        b,
		Root:      true,
	}
	r, err := audit.Run(context.Background(), d, audit.Options{Duration: time.Minute})
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func golden(t *testing.T, name string, got []byte) {
	t.Helper()
	path := filepath.Join("testdata", name)
	if *update {
		if err := os.MkdirAll("testdata", 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, got, 0o644); err != nil {
			t.Fatal(err)
		}
		return
	}
	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("%v (run go test -update to create it)", err)
	}
	if !bytes.Equal(got, want) {
		t.Errorf("%s differs from the golden file; run go test -update and review the diff.\n--- got ---\n%s", name, got)
	}
}

func TestTerminalGolden(t *testing.T) {
	r := shopResult(t)
	var buf bytes.Buffer
	if err := Terminal(&buf, r, "1.0.0", Painter{}, "/var/lib/snugbox/reports/audit-20261031-090100.md"); err != nil {
		t.Fatal(err)
	}
	golden(t, "shop.terminal.golden", buf.Bytes())
	if bytes.Contains(buf.Bytes(), []byte("\x1b[")) {
		t.Error("colour codes written with colour off")
	}
	buf.Reset()
	if err := Terminal(&buf, r, "1.0.0", Painter{Color: true}, ""); err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(buf.Bytes(), []byte("\x1b[33m")) {
		t.Error("no colour with colour on")
	}
}

func TestMarkdownGolden(t *testing.T) {
	var buf bytes.Buffer
	if err := Markdown(&buf, shopResult(t), "1.0.0"); err != nil {
		t.Fatal(err)
	}
	golden(t, "shop.md.golden", buf.Bytes())
}

// On a kernel with pressure stall information off, as RHEL 9 ships, the
// listed fixes can't run, and both reports say why and what to do.
func TestFixesWithoutPressureFiles(t *testing.T) {
	r := shopResult(t)
	if len(r.Fixes) == 0 {
		t.Fatal("the shop box offers no fix to block")
	}
	r.Machine.PSI = false
	var term, md bytes.Buffer
	if err := Terminal(&term, r, "1.0.0", Painter{}, ""); err != nil {
		t.Fatal(err)
	}
	if err := Markdown(&md, r, "1.0.0"); err != nil {
		t.Fatal(err)
	}
	for name, out := range map[string]string{"terminal": term.String(), "markdown": md.String()} {
		if !strings.Contains(out, "None of these can run yet") || !strings.Contains(out, "Boot with psi=1") {
			t.Errorf("the %s report doesn't say the fixes need pressure stall information:\n%s", name, out)
		}
	}
	r.Machine.PSI = true
	if FixesBlocked(r) != "" {
		t.Error("fixes are reported blocked on a box with pressure files")
	}
}

// With no early out-of-memory protection running and no fix for it, both
// reports say what a person can do, whether or not other fixes are listed:
// start systemd-oomd where the distribution gave it a policy, which the
// helper leaves alone, or install a daemon where none is installed.
func TestOOMAdvice(t *testing.T) {
	r := shopResult(t)
	var others []fix.Proposal
	for _, f := range r.Fixes {
		if f.Kind != fix.KindOOMProtection {
			others = append(others, f)
		}
	}
	if len(others) == 0 {
		t.Fatal("the shop box offers no other fix to list beside the advice")
	}
	r.Fixes = others
	r.Machine.OOMDaemon = ""
	r.Risks = append(r.Risks, audit.Risk{Kind: audit.RiskNoOOMProtection})
	reports := func() map[string]string {
		t.Helper()
		var term, md bytes.Buffer
		if err := Terminal(&term, r, "1.0.0", Painter{}, ""); err != nil {
			t.Fatal(err)
		}
		if err := Markdown(&md, r, "1.0.0"); err != nil {
			t.Fatal(err)
		}
		return map[string]string{"terminal": term.String(), "markdown": md.String()}
	}
	for name, out := range reports() {
		if !strings.Contains(out, "install systemd-oomd or earlyoom") {
			t.Errorf("the %s report doesn't say to install an out-of-memory daemon:\n%s", name, out)
		}
	}

	r.Machine.OOMPolicy = collect.OOMPolicy{Slice: "system.slice", Value: "kill", File: "/usr/lib/systemd/system/system.slice.d/10-oomd-per-slice-defaults.conf"}
	for _, fixes := range [][]fix.Proposal{others, nil} {
		r.Fixes = fixes
		for name, out := range reports() {
			if !strings.Contains(out, "10-oomd-per-slice-defaults.conf gives system.slice the policy ManagedOOMMemoryPressure=kill") ||
				!strings.Contains(out, "sudo systemctl enable --now systemd-oomd") || strings.Contains(out, "install systemd-oomd or earlyoom") {
				t.Errorf("with %d other fixes, the %s report doesn't say to start the distribution's systemd-oomd:\n%s", len(fixes), name, out)
			}
		}
	}

	r.Fixes = append(others, fix.Proposal{Kind: fix.KindOOMProtection, Risk: fix.RiskMedium, OOMDaemon: fix.DaemonEarlyoom})
	if got := FixAdvice(r); len(got) != 0 {
		t.Errorf("advice beside a fix that covers it: %q", got)
	}
}

func TestJSONGolden(t *testing.T) {
	var buf bytes.Buffer
	if err := JSON(&buf, shopResult(t), "1.0.0"); err != nil {
		t.Fatal(err)
	}
	golden(t, "shop.json.golden", buf.Bytes())
	var doc map[string]any
	if err := json.Unmarshal(buf.Bytes(), &doc); err != nil {
		t.Fatal(err)
	}
	if doc["schema"] != SchemaV1 {
		t.Errorf("schema %v", doc["schema"])
	}
	for _, key := range []string{"estimate", "advice", "risks", "fixes", "workloads", "data_quality"} {
		if _, ok := doc[key]; !ok {
			t.Errorf("no %q in the JSON report", key)
		}
	}
}

func TestCardRedacts(t *testing.T) {
	r := shopResult(t)
	var svg, txt bytes.Buffer
	if err := CardSVG(&svg, r); err != nil {
		t.Fatal(err)
	}
	if err := CardText(&txt, r); err != nil {
		t.Fatal(err)
	}
	golden(t, "shop.card.txt.golden", txt.Bytes())
	for _, secret := range []string{"shop-app-1", "shop-db-1", "backup.service", "restic", "web-1", "postgres", id('a')[:12]} {
		if bytes.Contains(svg.Bytes(), []byte(secret)) || bytes.Contains(txt.Bytes(), []byte(secret)) {
			t.Errorf("the share card names %q", secret)
		}
	}
	if !bytes.Contains(txt.Bytes(), []byte("database container 1")) || !bytes.Contains(txt.Bytes(), []byte("app container 1")) {
		t.Errorf("redacted names missing:\n%s", txt.String())
	}
}

func TestFormat(t *testing.T) {
	for b, want := range map[uint64]string{3629252608: "3.38 GiB", 4 << 30: "4 GiB", 512 << 20: "0.5 GiB"} {
		if got := GiB(b); got != want {
			t.Errorf("GiB(%d) = %q, want %q", b, got, want)
		}
	}
	for b, want := range map[uint64]string{1240 * mib: "1,240 MiB", 300 << 10: "0.3 MiB", 0: "0 MiB", 12 * mib: "12 MiB"} {
		if got := MiB(b); got != want {
			t.Errorf("MiB(%d) = %q, want %q", b, got, want)
		}
	}
	for d, want := range map[time.Duration]string{time.Second: "1 second", time.Minute: "60 seconds", 14 * 24 * time.Hour: "14 days", 3 * time.Hour: "3 hours"} {
		if got := Span(d); got != want {
			t.Errorf("Span(%v) = %q, want %q", d, got, want)
		}
	}
	if got := List([]string{"a", "b", "c"}); got != "a, b and c" {
		t.Errorf("List = %q", got)
	}
	if got := Percent(0.016); got != "1.6%" {
		t.Errorf("Percent = %q", got)
	}
	if got := clip("abcdefghijklmnop", 10); got != "abcdefg..." {
		t.Errorf("clip = %q", got)
	}
	if got := mdEscape("a|b_c*d"); got != `a\|b\_c\*d` {
		t.Errorf("mdEscape = %q", got)
	}
}
