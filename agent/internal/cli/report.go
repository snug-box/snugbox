package cli

import (
	"errors"
	"time"

	"github.com/spf13/cobra"

	"snugbox.dev/snugbox/agent/internal/localapi"
	"snugbox.dev/snugbox/agent/internal/report"
)

func newReport(e *env) *cobra.Command {
	var week bool
	cmd := &cobra.Command{
		Use:   "report",
		Short: "Say what changed on this box and what it saved",
		Long: "snugbox report reads the watcher's history for the last 24 hours, or the last 7 days with\n" +
			"--week: what the box needs now and at the start, the saving from a move to a cheaper plan,\n" +
			"the fixes applied, undone or kept, and the out-of-memory kills, early warnings and\n" +
			"memory.high hits. A saving counts only once the watcher has seen the box on its new plan.",
		Example: "  snugbox report\n  sudo snugbox report --week",
		Args:    cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			ctx := cmd.Context()
			since := report.PeriodSince(time.Now(), week)
			rep, err := e.watcher.Report(ctx, since)
			if errors.Is(err, localapi.ErrNotRunning) {
				return errors.New("snugbox report reads the watcher's history; start the watcher with sudo snugbox watch install")
			}
			if err != nil {
				return err
			}
			// The latest analysis says what the box pays for; before the
			// first one, the report goes without it.
			r, _ := e.watcher.Result(ctx)
			return report.Period(e.stdout, rep, r, report.Painter{Color: e.color})
		},
	}
	cmd.Flags().BoolVar(&week, "week", false, "cover the last 7 days instead of the last 24 hours")
	return cmd
}
