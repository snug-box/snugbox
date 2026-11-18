package plan

import (
	"strings"
	"testing"
)

const gib = uint64(1) << 30

func gibs(x float64) uint64 { return uint64(x * float64(gib)) }

func mustEmbedded(t *testing.T) *Catalogue {
	t.Helper()
	c, err := Embedded()
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func TestEmbeddedCatalogue(t *testing.T) {
	c := mustEmbedded(t)
	if c.Checked != "2026-10-31" || len(c.Providers) != 5 {
		t.Fatalf("checked %s, %d providers", c.Checked, len(c.Providers))
	}
	for _, p := range c.Providers {
		if p.Source == "" || p.ResizeDoc == "" || len(p.Plans) == 0 {
			t.Errorf("%s lacks a source, a resize document or plans", p.ID)
		}
		for _, pl := range p.Plans {
			if pl.RAM == 0 || pl.Disk == 0 || pl.Transfer == 0 || pl.Monthly <= 0 || pl.Source == "" {
				t.Errorf("%s/%s is incomplete: %+v", p.ID, pl.ID, pl)
			}
		}
	}
	do, _ := c.Provider("digitalocean")
	if do.Plans[4].ID != "s-2vcpu-4gb" || do.Plans[4].RAM != 4*gib || do.Plans[4].Disk != 80*gib || do.Plans[4].Monthly != 2400 {
		t.Errorf("DigitalOcean 4 GiB plan %+v", do.Plans[4])
	}
	hz, _ := c.Provider("hetzner")
	var cpx12, cpx22 Plan
	for _, pl := range hz.Plans {
		switch pl.ID {
		case "cpx12":
			cpx12 = pl
		case "cpx22":
			cpx22 = pl
		}
	}
	if !cpx12.Converted || !cpx12.IPv4Included || cpx12.Monthly != 1340 || c.Total(cpx12) != 1340 {
		t.Errorf("CPX12 at 11.99 EUR with IPv4: %+v total %v", cpx12, c.Total(cpx12))
	}
	if cpx22.Converted || c.Total(cpx22) != 2299+60 || cpx22.RAM != 4e9 {
		t.Errorf("CPX22: %+v total %v", cpx22, c.Total(cpx22))
	}
}

func TestParseRejectsBadData(t *testing.T) {
	good := `{"schema":1,"checked":"x","currency":"USD","providers":[{"id":"p","name":"P","source":"s","resize":"no_downgrade","resize_doc":"d","plans":[{"id":"a","label":"A","vcpus":1,"ram":"1GiB","disk":"10GB","transfer":"1TB","monthly":"5.00"}]}]}`
	if _, err := Parse([]byte(good)); err != nil {
		t.Fatalf("good catalogue rejected: %v", err)
	}
	for name, bad := range map[string]string{
		"unknown field":  strings.Replace(good, `"vcpus":1`, `"vcpus":1,"cores":1`, 1),
		"unknown unit":   strings.Replace(good, `"1GiB"`, `"1Gb"`, 1),
		"one decimal":    strings.Replace(good, `"5.00"`, `"5.0"`, 1),
		"no price":       strings.Replace(good, `,"monthly":"5.00"`, ``, 1),
		"euro, no rate":  strings.Replace(good, `"monthly":"5.00"`, `"monthly_eur_with_ipv4":"5.00"`, 1),
		"resize rule":    strings.Replace(good, `"no_downgrade"`, `"maybe"`, 1),
		"zero vcpus":     strings.Replace(good, `"vcpus":1`, `"vcpus":0`, 1),
		"schema":         strings.Replace(good, `"schema":1`, `"schema":2`, 1),
		"duplicate plan": strings.Replace(good, `"monthly":"5.00"}]`, `"monthly":"5.00"},{"id":"a","label":"A","vcpus":1,"ram":"1GiB","disk":"10GB","transfer":"1TB","monthly":"5.00"}]`, 1),
	} {
		if _, err := Parse([]byte(bad)); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

func TestParseSize(t *testing.T) {
	for in, want := range map[string]uint64{"4GB": 4e9, "0.5GB": 5e8, "512MiB": 512 << 20, "1GiB": gib, "20TB": 20e12, "81920MiB": 80 * gib} {
		if got, err := ParseSize(in); err != nil || got != want {
			t.Errorf("ParseSize(%q) = %d, %v; want %d", in, got, err, want)
		}
	}
	for _, bad := range []string{"", "GB", "4", "4 GB", "-1GB", "4gb"} {
		if _, err := ParseSize(bad); err == nil {
			t.Errorf("ParseSize(%q) succeeded", bad)
		}
	}
}

func TestMoneyString(t *testing.T) {
	for m, want := range map[Money]string{0: "$0.00", 2400: "$24.00", 1340: "$13.40", 123456789: "$1,234,567.89", -2441: "-$24.41"} {
		if got := m.String(); got != want {
			t.Errorf("%d = %q, want %q", int64(m), got, want)
		}
	}
}

func TestSizeClass(t *testing.T) {
	cases := []struct {
		snug uint64
		want uint64
	}{
		{3380 << 20, 4 * gib},
		{Usable(4*gib) + 1, 8 * gib},
		{100 << 20, gib},
	}
	for _, c := range cases {
		if got, ok := SizeClass(c.snug); !ok || got != c.want {
			t.Errorf("SizeClass(%d MiB) = %d GiB, want %d GiB", c.snug>>20, got/gib, c.want/gib)
		}
	}
	if _, ok := SizeClass(600 * gib); ok {
		t.Error("600 GiB fitted a size class")
	}
}

// The worked example of blueprint section 3.6: a DigitalOcean box on the
// 8 GiB plan needs 3.38 GiB.
func TestAdviseWorkedExample(t *testing.T) {
	c := mustEmbedded(t)
	snug := gibs(3.38)
	box := Box{ProviderID: "digitalocean", MemTotal: 8148000 << 10, CPUs: 4, DiskDevice: 80 * gib, DiskUsed: 31 * gib, CPUQ95: 1.1}
	cur, ok := c.Current(box)
	if !ok || cur.ID != "s-4vcpu-8gb" {
		t.Fatalf("current plan %+v, %v", cur, ok)
	}
	opts, unavailable := c.Advise(snug, box, &cur)
	if len(opts) != 5 {
		t.Fatalf("%d options: %+v", len(opts), opts)
	}
	want := []struct {
		provider, plan string
		total          Money
		move           Move
	}{
		{"vultr", "vc2-2c-4gb", 2000, MoveNewServer},
		{"hetzner", "cpx22", 2359, MoveNewServer},
		{"aws", "ls-4", 2400, MoveNewServer},
		{"digitalocean", "s-2vcpu-4gb", 2400, MoveResize},
		{"linode", "g6-standard-2", 2400, MoveNewServer},
	}
	for i, w := range want {
		o := opts[i]
		if o.Plan.ProviderID != w.provider || o.Plan.ID != w.plan || o.Total != w.total || o.Move != w.move || !o.HasSaving || o.Saving != 4800-w.total {
			t.Errorf("option %d = %s/%s %v %q saving %v; want %s/%s %v %q", i, o.Plan.ProviderID, o.Plan.ID, o.Total, o.Move, o.Saving, w.provider, w.plan, w.total, w.move)
		}
	}
	var names []string
	for _, u := range unavailable {
		names = append(names, u.Plan.ID)
	}
	if strings.Join(names, ",") != "cx33,cax21,cx43" {
		t.Errorf("unavailable cheaper plans %v, want cx33,cax21,cx43 (the 40 GB disks of cx23 and cax11 lack 30%% room over 31 GiB used)", names)
	}

	box.DiskDevice = 160 * gib
	opts, _ = c.Advise(snug, box, &cur)
	for _, o := range opts {
		if o.Plan.ProviderID == "digitalocean" && o.Move != MoveNewServer {
			t.Errorf("a Droplet with a 160 GiB disk can't resize to an 80 GiB plan: %q", o.Move)
		}
	}
}

func TestFitsLimits(t *testing.T) {
	c := mustEmbedded(t)
	do, _ := c.Provider("digitalocean")
	p4 := do.Plans[4]
	if !c.Fits(p4, 3*gib, Box{}) {
		t.Error("3 GiB does not fit a 4 GiB plan")
	}
	if c.Fits(p4, gibs(3.8), Box{}) {
		t.Error("3.8 GiB fits in 93% of 4 GiB")
	}
	if c.Fits(p4, gib, Box{CPUQ95: 2.5}) {
		t.Error("2.5 cores fit 2 vCPUs")
	}
	if c.Fits(p4, gib, Box{DiskUsed: 70 * gib}) {
		t.Error("70 GiB used fits an 80 GiB disk without 30% to spare")
	}
	if c.Fits(p4, gib, Box{EgressMonthly: 3600 * gib}) {
		t.Error("3,600 GiB of traffic fits 4,000 GiB without 20% to spare")
	}
	ls, _ := c.Provider("aws")
	if c.Fits(ls.Plans[3], gib, Box{EgressMonthly: 2e12, IngressMonthly: 2e12}) {
		t.Error("Lightsail counts inbound traffic, so 4 TB in total needs more than its 4 TB allowance")
	}
}

func TestMoves(t *testing.T) {
	c := mustEmbedded(t)
	ln, _ := c.Provider("linode")
	cur := ln.Plans[3]
	if m := c.move(ln.Plans[2], Box{DiskDevice: cur.Disk}, &cur); m != MoveResizeAfterShrink {
		t.Errorf("Linode down a plan: %q", m)
	}
	if m := c.move(ln.Plans[4], Box{DiskDevice: cur.Disk}, &cur); m != MoveResize {
		t.Errorf("Linode up a plan: %q", m)
	}
	vu, _ := c.Provider("vultr")
	vcur := vu.Plans[5]
	if m := c.move(vu.Plans[4], Box{}, &vcur); m != MoveNewServer {
		t.Errorf("Vultr down a plan: %q", m)
	}
	if m := c.move(vcur, Box{}, &vcur); m != MoveSame {
		t.Errorf("same plan: %q", m)
	}
}
