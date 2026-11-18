package report

import (
	"fmt"
	"io"
	"strconv"
	"strings"

	"snugbox.dev/snugbox/agent/internal/audit"
	"snugbox.dev/snugbox/agent/internal/plan"
)

// Plans writes every plan that fits, for snugbox plan, cheapest first. Plans
// a provider no longer sells follow in a table of their own, so they are
// never ranked with plans that can be bought. provider, when not empty,
// keeps one provider's plans only.
func Plans(w io.Writer, r *audit.Result, provider string, p Painter) error {
	var b strings.Builder
	line := func(s string) { b.WriteString(strings.TrimRight(s, " ") + "\n") }
	a := r.Advice
	line(Headline(r))
	line(p.paint(dim, SnugParts(r.Estimate)+". Confidence "+r.Estimate.Confidence.String()+": "+ConfidenceDetail(r.Estimate)))
	line("")
	switch {
	case a.Catalogue == nil:
		line("The price catalogue could not be read.")
	case a.Withheld:
		line("No smaller plan is advised: this box shows signs it needs more memory or a fix.")
		for _, s := range RiskLines(r) {
			line(p.paint(warn, "  !  ") + s)
		}
		line(p.paint(dim, "Run sudo snugbox audit for the fixes."))
	default:
		if provider != "" {
			if _, ok := a.Catalogue.Provider(provider); !ok {
				return fmt.Errorf("unknown provider %q; the catalogue has %s", provider, providerIDs(a.Catalogue))
			}
		}
		var buyable, gone [][]cell
		for _, o := range a.Catalogue.All(r.Estimate.Snug, a.Box, a.Current) {
			if provider != "" && o.Plan.ProviderID != provider {
				continue
			}
			if o.Plan.Available {
				buyable = append(buyable, append(planCells(a.Catalogue, o), cell{text: MoveText(o)}))
			} else {
				// A saving no one can buy is no saving, so this table has
				// no saving column.
				gone = append(gone, planCells(a.Catalogue, o)[:7])
			}
		}
		if len(buyable)+len(gone) == 0 {
			line("No plan in the catalogue fits this box.")
			break
		}
		heads := []cell{{}, {text: "plan"}, {text: "vCPU", right: true}, {text: "memory", right: true}, {text: "usable", right: true}, {text: "disk", right: true}, {text: "a month", right: true}}
		if len(buyable) > 0 {
			for _, l := range tableLines(p, "PLANS THAT FIT", append(heads, cell{text: "saving", right: true}, cell{text: "how"}), buyable) {
				line(l)
			}
		} else {
			line("No plan that can still be ordered fits this box.")
		}
		if len(gone) > 0 {
			line("")
			for _, l := range tableLines(p, "NOT AVAILABLE TO ORDER", heads, gone) {
				line(l)
			}
		}
		converted := hasConverted(buyable) || hasConverted(gone)
		line("")
		line(p.paint(dim, fmt.Sprintf("Usable memory is %.0f%% of the advertised size until Snugbox has measured the plan.", plan.UsableShare*100)))
		for _, n := range PriceNote(a.Catalogue, converted) {
			line(p.paint(dim, n))
		}
		if converted {
			line(p.paint(dim, "* converted from euros."))
		}
		if a.Current != nil && (provider == "" || provider == a.Current.ProviderID) {
			if prov, ok := a.Catalogue.Provider(a.Current.ProviderID); ok {
				line(p.paint(dim, fmt.Sprintf("How %s moves a server to a smaller plan: %s", prov.Name, prov.ResizeDoc)))
			}
		}
	}
	_, err := io.WriteString(w, b.String())
	return err
}

// planCells are the columns both plan tables share.
func planCells(c *plan.Catalogue, o plan.Option) []cell {
	prov, _ := c.Provider(o.Plan.ProviderID)
	price := o.Total.String()
	if o.Plan.Converted {
		price += "*"
	}
	saving := ""
	if o.HasSaving {
		saving = o.Saving.String()
	}
	return []cell{
		{text: prov.Name}, {text: o.Plan.Label},
		{text: strconv.Itoa(o.Plan.VCPUs), right: true},
		{text: GiB(o.Plan.RAM), right: true}, {text: GiB(o.Usable), right: true},
		{text: GiB(o.Plan.Disk), right: true},
		{text: price, right: true}, {text: saving, right: true, style: good},
	}
}

func hasConverted(rows [][]cell) bool {
	for _, r := range rows {
		if strings.HasSuffix(r[6].text, "*") {
			return true
		}
	}
	return false
}

func providerIDs(c *plan.Catalogue) string {
	var ids []string
	for _, p := range c.Providers {
		ids = append(ids, p.ID)
	}
	return List(ids)
}
