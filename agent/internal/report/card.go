package report

import (
	"fmt"
	"html"
	"io"
	"slices"
	"strings"

	"snugbox.dev/snugbox/agent/internal/audit"
	"snugbox.dev/snugbox/agent/internal/model"
)

// The share card of blueprint section 7.2: the box's numbers with every name
// removed, so it can be posted without showing what the box runs.

// roles maps program names to the generic role a redacted card shows.
var roles = map[string]string{
	"nginx": "web", "caddy": "web", "apache2": "web", "httpd": "web", "haproxy": "web", "traefik": "web", "envoy": "web",
	"memcached": "cache", "redis-server": "cache", "valkey-server": "cache",
	"node": "app", "java": "app", "python": "app", "python3": "app", "ruby": "app", "php-fpm": "app", "gunicorn": "app", "uwsgi": "app",
	"chromium": "browser", "chrome": "browser", "headless_shell": "browser",
	"restic": "backup", "borg": "backup", "rclone": "backup", "rsync": "backup", "pg_dump": "backup", "mysqldump": "backup",
	"sshd": "ssh", "cron": "scheduler", "crond": "scheduler",
}

func roleOf(w model.Workload) string {
	if w.Database != "" {
		if w.Database == "Redis" || w.Database == "Valkey" {
			return "cache"
		}
		return "database"
	}
	for _, p := range w.Programs {
		if r, ok := roles[p]; ok {
			return r
		}
		if strings.HasPrefix(p, "php-fpm") {
			return "app"
		}
	}
	return "other"
}

// RedactedNames gives each workload a name such as "web container 1" that
// says what it does without saying what it is. Numbering follows ids.
func RedactedNames(ws map[string]model.Workload, ids []string) map[string]string {
	out := make(map[string]string, len(ids))
	counts := map[string]int{}
	for _, id := range ids {
		w := ws[id]
		kind := map[model.Kind]string{model.KindContainer: "container", model.KindService: "service"}[w.Kind]
		if kind == "" {
			kind = "process group"
		}
		key := roleOf(w) + " " + kind
		counts[key]++
		out[id] = fmt.Sprintf("%s %d", key, counts[key])
	}
	return out
}

// bySize returns workload IDs, largest working set first.
func bySize(r *audit.Result) []string {
	ids := sortedIDs(r.Workloads)
	slices.SortStableFunc(ids, func(x, y string) int {
		a, b := r.Summary.Workloads[x].WorkingSetQ99, r.Summary.Workloads[y].WorkingSetQ99
		switch {
		case a > b:
			return -1
		case a < b:
			return 1
		}
		return 0
	})
	return ids
}

func topWorkloads(r *audit.Result, n int) []string {
	ids := bySize(r)
	return ids[:min(n, len(ids))]
}

// CardText writes the redacted card as plain text.
func CardText(w io.Writer, r *audit.Result) error {
	var b strings.Builder
	fmt.Fprintln(&b, "Snugbox audit")
	fmt.Fprintln(&b, Headline(r))
	fmt.Fprintf(&b, "Waste %s. Confidence %s, from %s of data.\n", GiB(r.Advice.Waste), r.Estimate.Confidence, Span(r.Estimate.Window))
	top := topWorkloads(r, 5)
	names := RedactedNames(r.Workloads, bySize(r))
	for _, id := range top {
		fmt.Fprintf(&b, "  %-24s %10s\n", names[id], MiB(r.Summary.Workloads[id].WorkingSetQ99))
	}
	if n := len(r.Risks); n > 0 {
		fmt.Fprintf(&b, "%s found.\n", Count(n, "risk", "risks"))
	}
	fmt.Fprintln(&b, "snugbox.dev")
	_, err := io.WriteString(w, b.String())
	return err
}

// CardSVG writes the redacted card as a 1200 by 630 image, the size link
// previews use, in the brand colours of blueprint section 4.4.
func CardSVG(w io.Writer, r *audit.Result) error {
	const (
		paper, ink, muted, marigold, line = "#FBF8F1", "#14181F", "#555D69", "#F2B33D", "#8C939E"
		width, barX, barW, barY, barH     = 1200, 80, 1040, 330, 44
	)
	a, e := r.Advice, r.Estimate
	esc := html.EscapeString
	var b strings.Builder
	fmt.Fprintf(&b, `<svg xmlns="http://www.w3.org/2000/svg" width="%d" height="630" viewBox="0 0 %d 630" role="img" aria-labelledby="t d">`, width, width)
	fmt.Fprintf(&b, `<title id="t">Snugbox audit</title><desc id="d">%s</desc>`, esc(Headline(r)))
	fmt.Fprintf(&b, `<rect width="%d" height="630" fill="%s"/>`, width, paper)
	fmt.Fprintf(&b, `<g transform="translate(80 56) scale(1.25)"><path fill="%s" d="M8 25L13.3 9.6C13.9 7.9 16.1 7.6 17.2 9L25.2 19.6C25.8 20.4 26.7 20.8 27.7 20.8H36.3C37.3 20.8 38.2 20.4 38.8 19.6L46.8 9C47.9 7.6 50.1 7.9 50.7 9.6L56 25V48C56 53.5 51.5 58 46 58H18C12.5 58 8 53.5 8 48Z"/><path fill="%s" d="M18.5 33.5H45.5C46.6 33.5 47.5 34.4 47.5 35.5V47.5C47.5 48.6 46.6 49.5 45.5 49.5H39.5V46.5H36.5V49.5H18.5C17.4 49.5 16.5 48.6 16.5 47.5V35.5C16.5 34.4 17.4 33.5 18.5 33.5Z"/></g>`, ink, marigold)
	font := `font-family="Recursive, ui-sans-serif, system-ui, sans-serif"`
	fmt.Fprintf(&b, `<text x="172" y="112" %s font-size="34" font-weight="700" fill="%s">Snugbox audit</text>`, font, ink)
	fmt.Fprintf(&b, `<text x="80" y="230" %s font-size="64" font-weight="700" fill="%s">Pays for %s. Needs %s.</text>`, font, ink, esc(GiB(a.PaysFor)), esc(GiB(e.Snug)))
	sub := "Snug already."
	switch {
	case a.Withheld:
		sub = "Needs a fix before a smaller plan."
	case a.HasSizeClass && !r.Fits():
		sub = fmt.Sprintf("Needs a %s plan or a fix.", GiB(a.SizeClass))
	case a.HasSizeClass && a.SizeClass < a.OwnClass(r.Machine.MemTotal):
		sub = fmt.Sprintf("A %s plan fits. Waste: %s.", GiB(a.SizeClass), GiB(a.Waste))
	}
	fmt.Fprintf(&b, `<text x="80" y="292" %s font-size="34" fill="%s">%s</text>`, font, muted, esc(sub))

	fmt.Fprintf(&b, `<rect x="%d" y="%d" width="%d" height="%d" rx="10" fill="#FFFFFF" stroke="%s" stroke-width="2"/>`, barX, barY, barW, barH, line)
	if a.PaysFor > 0 {
		need := min(float64(e.Snug)/float64(a.PaysFor), 1)
		fmt.Fprintf(&b, `<rect x="%d" y="%d" width="%.0f" height="%d" rx="10" fill="%s"/>`, barX, barY, need*barW, barH, marigold)
	}
	y := 430
	names := RedactedNames(r.Workloads, bySize(r))
	for _, id := range topWorkloads(r, 4) {
		fmt.Fprintf(&b, `<text x="80" y="%d" %s font-size="28" fill="%s">%s</text>`, y, font, ink, esc(names[id]))
		fmt.Fprintf(&b, `<text x="620" y="%d" text-anchor="end" font-family="Recursive, ui-monospace, monospace" font-size="28" fill="%s">%s</text>`, y, ink, esc(MiB(r.Summary.Workloads[id].WorkingSetQ99)))
		y += 40
	}
	fmt.Fprintf(&b, `<text x="1120" y="590" text-anchor="end" %s font-size="26" fill="%s">Confidence %s, %s of data. snugbox.dev</text>`, font, muted, esc(e.Confidence.String()), esc(Span(e.Window)))
	b.WriteString(`</svg>` + "\n")
	_, err := io.WriteString(w, b.String())
	return err
}
