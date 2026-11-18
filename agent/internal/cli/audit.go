package cli

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"snugbox.dev/snugbox/agent/internal/audit"
	"snugbox.dev/snugbox/agent/internal/collect"
	"snugbox.dev/snugbox/agent/internal/docker"
	"snugbox.dev/snugbox/agent/internal/localapi"
	"snugbox.dev/snugbox/agent/internal/report"
	"snugbox.dev/snugbox/agent/internal/version"
)

// ReportDir is where a root audit saves its Markdown report.
const ReportDir = "/var/lib/snugbox/reports"

type auditFlags struct {
	duration time.Duration
	json     bool
	markdown bool
	share    bool
	offline  bool
}

func newAudit(e *env) *cobra.Command {
	var f auditFlags
	cmd := &cobra.Command{
		Use:   "audit",
		Short: "Report what this box needs, read-only",
		Long: "Sample this box's memory for a minute and report what it pays for, what it needs, the waste,\n" +
			"the risks, the fixes available and the plans that fit. It changes nothing and sends no data\n" +
			"anywhere; unless --offline, it asks the cloud metadata service which provider this is.\n\n" +
			"Run it with sudo for the full report: process details, Docker names and the kernel log need root.\n" +
			"Exit code 3 means risks were found.",
		Example: "  sudo snugbox audit\n  sudo snugbox audit --for 10m\n  sudo snugbox audit --json > audit.json\n  sudo snugbox audit --share",
		Args:    cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if f.json && f.markdown {
				return usageError(cmd, errors.New("choose one of --json and --markdown"))
			}
			var r *audit.Result
			if !cmd.Flags().Changed("for") {
				r = watcherResult(cmd.Context(), e)
			}
			if r == nil {
				var err error
				if r, err = runAudit(cmd.Context(), e, f.duration, f.offline, !f.json && !f.markdown); err != nil {
					return err
				}
			}
			return printAudit(e, r, f)
		},
	}
	cmd.Flags().DurationVar(&f.duration, "for", audit.DefaultDuration, "how long to sample, from 10s to 1h")
	cmd.Flags().BoolVar(&f.json, "json", false, "write the versioned JSON report to standard output")
	cmd.Flags().BoolVar(&f.markdown, "markdown", false, "write the full Markdown report to standard output")
	cmd.Flags().BoolVar(&f.share, "share", false, "also write a share card with every name removed, as snugbox-card.svg and snugbox-card.txt")
	cmd.Flags().BoolVar(&f.offline, "offline", false, "don't ask the cloud metadata service which provider this is")
	return cmd
}

// watcherResult returns the running watcher's latest analysis, or nil when
// there is no watcher or no analysis yet; the audit then samples instead and
// says why on standard error when the watcher answered with a problem.
func watcherResult(ctx context.Context, e *env) *audit.Result {
	r, err := e.watcher.Result(ctx)
	if err != nil {
		if !errors.Is(err, localapi.ErrNotRunning) {
			fmt.Fprintf(e.stderr, "snugbox: the watcher's history couldn't be used (%v); sampling instead.\n", err)
		}
		return nil
	}
	return r
}

// runAudit samples the box. showProgress prints a countdown on standard
// error when it is a terminal.
func runAudit(ctx context.Context, e *env, d time.Duration, offline, showProgress bool) (*audit.Result, error) {
	opts := audit.Options{Duration: d, Offline: offline}
	if showProgress && e.stderrTTY {
		opts.Progress = func(left time.Duration) {
			fmt.Fprintf(e.stderr, "\rSampling for %s: %s left. Press Ctrl+C to stop.  ", report.Span(d), report.Span(left.Round(time.Second)))
		}
		defer fmt.Fprint(e.stderr, "\r\x1b[2K")
	}
	r, err := e.sample(ctx, opts)
	if errors.Is(err, context.Canceled) {
		return nil, withCode(ExitError, errors.New("stopped before the sample finished; nothing was changed"))
	}
	return r, err
}

// liveSample audits the box this runs on, naming containers through Docker
// when its socket is there.
func liveSample(ctx context.Context, opts audit.Options) (*audit.Result, error) {
	if runtime.GOOS != "linux" {
		return nil, fmt.Errorf("snugbox measures Linux servers; this is %s", runtime.GOOS)
	}
	var containers collect.ContainerSource
	if _, err := os.Stat(docker.DefaultSocket); err == nil {
		containers = collect.DockerSource{Client: docker.New(docker.DefaultSocket)}
	}
	deps, err := audit.Live(containers)
	if err != nil {
		return nil, err
	}
	return audit.Run(ctx, deps, opts)
}

func printAudit(e *env, r *audit.Result, f auditFlags) error {
	var err error
	switch {
	case f.json:
		err = report.JSON(e.stdout, r, version.Version)
	case f.markdown:
		err = report.Markdown(e.stdout, r, version.Version)
	default:
		saved := ""
		if r.Root {
			path, serr := saveMarkdown(r, e.reportDir)
			if serr != nil {
				fmt.Fprintf(e.stderr, "snugbox: the Markdown report was not saved: %v\n", serr)
			}
			saved = path
		}
		err = report.Terminal(e.stdout, r, version.Version, report.Painter{Color: e.color}, saved)
	}
	if err != nil {
		return err
	}
	if f.share {
		if err := writeCard(e, r); err != nil {
			return err
		}
	}
	if len(r.Risks) > 0 {
		return errRisks
	}
	return nil
}

// saveMarkdown writes the report into dir as a new file, never through a
// link.
func saveMarkdown(r *audit.Result, dir string) (string, error) {
	var buf bytes.Buffer
	if err := report.Markdown(&buf, r, version.Version); err != nil {
		return "", err
	}
	name := "audit-" + r.Ended.UTC().Format("20060102-150405") + ".md"
	if err := saveFile(dir, name, buf.Bytes()); err != nil {
		return "", err
	}
	return filepath.Join(dir, name), nil
}

func writeCard(e *env, r *audit.Result) error {
	var svg, txt bytes.Buffer
	if err := report.CardSVG(&svg, r); err != nil {
		return err
	}
	if err := report.CardText(&txt, r); err != nil {
		return err
	}
	for _, card := range []struct {
		name string
		body []byte
	}{{"snugbox-card.svg", svg.Bytes()}, {"snugbox-card.txt", txt.Bytes()}} {
		if err := writeNew(card.name, card.body); err != nil {
			return fmt.Errorf("share card: %w", err)
		}
	}
	_, err := fmt.Fprintln(e.stdout, "Wrote snugbox-card.svg and snugbox-card.txt with every workload name removed. Nothing was uploaded.")
	return err
}

// writeNew replaces a file in the current folder without following a link
// planted at its name.
func writeNew(name string, body []byte) error {
	fh, err := os.OpenFile(name, os.O_WRONLY|os.O_CREATE|os.O_TRUNC|noFollow, 0o644) //nolint:gosec // name is one of the two fixed card names
	if err != nil {
		return err
	}
	if _, err := fh.Write(body); err != nil {
		return errors.Join(err, fh.Close())
	}
	return fh.Close()
}

func newPlan(e *env) *cobra.Command {
	var (
		d        time.Duration
		offline  bool
		provider string
	)
	cmd := &cobra.Command{
		Use:   "plan",
		Short: "Show the plans that fit, from the price list built into this release",
		Long: "Sample this box, then list the plans that fit across providers, cheapest first, with the\n" +
			"move each one takes. Prices come from the catalogue inside this release; run\n" +
			"snugbox version to see which one.",
		Example: "  sudo snugbox plan\n  sudo snugbox plan --provider hetzner",
		Args:    cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			r, err := runAudit(cmd.Context(), e, d, offline, true)
			if err != nil {
				return err
			}
			return report.Plans(e.stdout, r, strings.ToLower(provider), report.Painter{Color: e.color})
		},
	}
	cmd.Flags().DurationVar(&d, "for", audit.DefaultDuration, "how long to sample first, from 10s to 1h")
	cmd.Flags().BoolVar(&offline, "offline", false, "don't ask the cloud metadata service which provider this is")
	cmd.Flags().StringVar(&provider, "provider", "", "show only this provider: digitalocean, hetzner, linode, vultr or aws")
	return cmd
}
