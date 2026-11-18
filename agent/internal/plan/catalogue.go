// Package plan holds the price catalogue that ships inside Snugbox and the
// rules of blueprint section 3.8 for which plans fit a box.
//
// The catalogue covers the five providers whose prices were checked for the
// first release. Each price keeps the page or API it was read from and the
// date, and is refreshed with every release. Prices are integer cents.
package plan

import (
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"strconv"
	"strings"
	"sync"
)

//go:embed catalogue.json
var catalogueJSON []byte

// Money is an amount in cents of the catalogue currency.
type Money int64

// String formats the amount as dollars with two decimals and thousands
// separators, as the reports print it.
func (m Money) String() string {
	neg := m < 0
	if neg {
		m = -m
	}
	whole := strconv.FormatInt(int64(m)/100, 10)
	for i := len(whole) - 3; i > 0; i -= 3 {
		whole = whole[:i] + "," + whole[i:]
	}
	s := fmt.Sprintf("$%s.%02d", whole, int64(m)%100)
	if neg {
		return "-" + s
	}
	return s
}

// ResizeRule says how a provider moves a server to a smaller plan.
type ResizeRule string

// The resize rules the catalogue knows.
const (
	// DiskAtLeastCurrent: resizing in place needs a plan whose disk is at
	// least the server's current disk, because disks never shrink.
	DiskAtLeastCurrent ResizeRule = "disk_at_least_current"
	// NoDowngrade: a smaller plan always means a new server.
	NoDowngrade ResizeRule = "no_downgrade"
	// ShrinkDiskFirst: a smaller plan is possible after shrinking the disks.
	ShrinkDiskFirst ResizeRule = "shrink_disk_first"
)

// Provider is one hosting provider in the catalogue.
type Provider struct {
	ID         string
	Name       string
	Source     string
	RegionNote string
	// IPv4 is the monthly fee for a public IPv4 address, for providers that
	// charge one on top of the plan price.
	IPv4      Money
	Resize    ResizeRule
	ResizeDoc string
	// TransferCountsInbound is true when traffic in both directions counts
	// toward the allowance.
	TransferCountsInbound bool
	Plans                 []Plan
}

// Plan is one server plan. Sizes are in bytes.
type Plan struct {
	ProviderID string
	ID         string
	Label      string
	VCPUs      int
	Arm        bool
	Dedicated  bool
	RAM        uint64
	Disk       uint64
	Transfer   uint64
	// Monthly is the plan's list price before VAT, without the IPv4 fee.
	Monthly Money
	// IPv4Included is true when Monthly already includes the IPv4 fee.
	IPv4Included bool
	// Converted is true when the provider publishes this plan only in euros
	// and Monthly was converted at the catalogue's reference rate.
	Converted bool
	Available bool
	Source    string
}

// FX is a reference rate used to convert a price.
type FX struct {
	Currency string
	USD      string
	Date     string
	Source   string
}

// Catalogue is the price list.
type Catalogue struct {
	Checked   string
	Currency  string
	Note      string
	FX        []FX
	Providers []Provider
}

// Provider returns the provider with the given ID.
func (c *Catalogue) Provider(id string) (*Provider, bool) {
	for i := range c.Providers {
		if c.Providers[i].ID == id {
			return &c.Providers[i], true
		}
	}
	return nil, false
}

// Total is what the plan costs a month, with the IPv4 fee where the provider
// adds one.
func (c *Catalogue) Total(p Plan) Money {
	if p.IPv4Included {
		return p.Monthly
	}
	if prov, ok := c.Provider(p.ProviderID); ok {
		return p.Monthly + prov.IPv4
	}
	return p.Monthly
}

var embedded = sync.OnceValues(func() (*Catalogue, error) { return Parse(catalogueJSON) })

// Embedded returns the catalogue built into this release.
func Embedded() (*Catalogue, error) { return embedded() }

type rawPlan struct {
	ID                string `json:"id"`
	Label             string `json:"label"`
	VCPUs             int    `json:"vcpus"`
	Arm               bool   `json:"arm"`
	Dedicated         bool   `json:"dedicated"`
	RAM               string `json:"ram"`
	Disk              string `json:"disk"`
	Transfer          string `json:"transfer"`
	Monthly           string `json:"monthly"`
	MonthlyEURWithIP4 string `json:"monthly_eur_with_ipv4"`
	Available         *bool  `json:"available"`
	Source            string `json:"source"`
}

type rawProvider struct {
	ID                    string    `json:"id"`
	Name                  string    `json:"name"`
	Source                string    `json:"source"`
	RegionNote            string    `json:"region_note"`
	IPv4Monthly           string    `json:"ipv4_monthly"`
	Resize                string    `json:"resize"`
	ResizeDoc             string    `json:"resize_doc"`
	TransferCountsInbound bool      `json:"transfer_counts_inbound"`
	Plans                 []rawPlan `json:"plans"`
}

type rawCatalogue struct {
	Schema   int    `json:"schema"`
	Checked  string `json:"checked"`
	Currency string `json:"currency"`
	Note     string `json:"note"`
	FX       []struct {
		Currency string `json:"currency"`
		USD      string `json:"usd"`
		Date     string `json:"date"`
		Source   string `json:"source"`
	} `json:"fx"`
	Providers []rawProvider `json:"providers"`
}

// Parse reads a catalogue file. It rejects unknown fields, unknown units and
// prices that aren't exact amounts in cents, so a typo in the data fails the
// build's tests instead of shipping.
func Parse(b []byte) (*Catalogue, error) {
	dec := json.NewDecoder(strings.NewReader(string(b)))
	dec.DisallowUnknownFields()
	var raw rawCatalogue
	if err := dec.Decode(&raw); err != nil {
		return nil, fmt.Errorf("catalogue: %w", err)
	}
	if raw.Schema != 1 || raw.Currency != "USD" {
		return nil, fmt.Errorf("catalogue: schema %d in %s is not supported", raw.Schema, raw.Currency)
	}
	c := &Catalogue{Checked: raw.Checked, Currency: raw.Currency, Note: raw.Note}
	eurRate := 0.0
	for _, fx := range raw.FX {
		c.FX = append(c.FX, FX{Currency: fx.Currency, USD: fx.USD, Date: fx.Date, Source: fx.Source})
		if fx.Currency == "EUR" {
			r, err := strconv.ParseFloat(fx.USD, 64)
			if err != nil || r <= 0 {
				return nil, fmt.Errorf("catalogue: EUR rate %q", fx.USD)
			}
			eurRate = r
		}
	}
	seen := make(map[string]bool)
	for _, rp := range raw.Providers {
		p := Provider{ID: rp.ID, Name: rp.Name, Source: rp.Source, RegionNote: rp.RegionNote, Resize: ResizeRule(rp.Resize), ResizeDoc: rp.ResizeDoc, TransferCountsInbound: rp.TransferCountsInbound}
		switch p.Resize {
		case DiskAtLeastCurrent, NoDowngrade, ShrinkDiskFirst:
		default:
			return nil, fmt.Errorf("catalogue %s: resize rule %q", p.ID, rp.Resize)
		}
		if rp.IPv4Monthly != "" {
			m, err := parseMoney(rp.IPv4Monthly)
			if err != nil {
				return nil, fmt.Errorf("catalogue %s ipv4: %w", p.ID, err)
			}
			p.IPv4 = m
		}
		for _, r := range rp.Plans {
			pl, err := parsePlan(p.ID, r, eurRate)
			if err != nil {
				return nil, fmt.Errorf("catalogue %s/%s: %w", p.ID, r.ID, err)
			}
			if seen[p.ID+"/"+pl.ID] {
				return nil, fmt.Errorf("catalogue: plan %s/%s listed twice", p.ID, pl.ID)
			}
			seen[p.ID+"/"+pl.ID] = true
			if pl.Source == "" {
				pl.Source = p.Source
			}
			p.Plans = append(p.Plans, pl)
		}
		c.Providers = append(c.Providers, p)
	}
	return c, nil
}

func parsePlan(provider string, r rawPlan, eurRate float64) (Plan, error) {
	p := Plan{ProviderID: provider, ID: r.ID, Label: r.Label, VCPUs: r.VCPUs, Arm: r.Arm, Dedicated: r.Dedicated, Available: r.Available == nil || *r.Available, Source: r.Source}
	if p.ID == "" || p.Label == "" || p.VCPUs <= 0 {
		return Plan{}, errors.New("plan needs an id, a label and vCPUs")
	}
	var err error
	if p.RAM, err = ParseSize(r.RAM); err != nil {
		return Plan{}, fmt.Errorf("ram: %w", err)
	}
	if p.Disk, err = ParseSize(r.Disk); err != nil {
		return Plan{}, fmt.Errorf("disk: %w", err)
	}
	if p.Transfer, err = ParseSize(r.Transfer); err != nil {
		return Plan{}, fmt.Errorf("transfer: %w", err)
	}
	switch {
	case r.Monthly != "" && r.MonthlyEURWithIP4 == "":
		p.Monthly, err = parseMoney(r.Monthly)
	case r.MonthlyEURWithIP4 != "" && r.Monthly == "":
		if eurRate == 0 {
			return Plan{}, errors.New("a euro price needs an EUR rate")
		}
		var eur Money
		eur, err = parseMoney(r.MonthlyEURWithIP4)
		p.Monthly = Money(math.Round(float64(eur) * eurRate))
		p.IPv4Included, p.Converted = true, true
	default:
		return Plan{}, errors.New("plan needs exactly one price")
	}
	if err != nil {
		return Plan{}, fmt.Errorf("price: %w", err)
	}
	return p, nil
}

var units = map[string]float64{
	"MB": 1e6, "GB": 1e9, "TB": 1e12,
	"MiB": 1 << 20, "GiB": 1 << 30, "TiB": 1 << 40,
}

// ParseSize reads a size such as "4GB", "0.5GB" or "512MiB" as bytes. GB and
// TB are powers of ten and GiB and MiB powers of two, as each provider states
// them; the catalogue records the unit the provider uses.
func ParseSize(s string) (uint64, error) {
	i := strings.IndexFunc(s, func(r rune) bool { return (r < '0' || r > '9') && r != '.' })
	if i <= 0 {
		return 0, fmt.Errorf("size %q has no number or no unit", s)
	}
	mult, ok := units[s[i:]]
	if !ok {
		return 0, fmt.Errorf("size %q: unknown unit", s)
	}
	n, err := strconv.ParseFloat(s[:i], 64)
	if err != nil || n <= 0 {
		return 0, fmt.Errorf("size %q: bad number", s)
	}
	return uint64(math.Round(n * mult)), nil
}

// parseMoney reads "12.49" as 1249 cents. Anything but two decimals is
// refused, so no price is rounded silently.
func parseMoney(s string) (Money, error) {
	whole, frac, ok := strings.Cut(s, ".")
	if !ok || len(frac) != 2 {
		return 0, fmt.Errorf("amount %q: want two decimals", s)
	}
	w, err1 := strconv.ParseInt(whole, 10, 64)
	f, err2 := strconv.ParseInt(frac, 10, 64)
	if err1 != nil || err2 != nil || w < 0 || f < 0 {
		return 0, fmt.Errorf("amount %q: not a number", s)
	}
	return Money(w*100 + f), nil
}
