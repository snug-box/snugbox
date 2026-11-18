package cli

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"snugbox.dev/snugbox/agent/internal/helperapi"
	"snugbox.dev/snugbox/agent/internal/localapi"
	"snugbox.dev/snugbox/agent/internal/report"
	"snugbox.dev/snugbox/agent/internal/units"
	helperv1 "snugbox.dev/snugbox/gen/snugbox/helper/v1"
)

// systemBinary finds a system tool at its usual places, so Snugbox never
// runs something a user put earlier in PATH.
func systemBinary(name string) (string, error) {
	for _, dir := range []string{"/usr/bin", "/bin", "/usr/sbin", "/sbin"} {
		p := filepath.Join(dir, name)
		if fi, err := os.Stat(p); err == nil && fi.Mode().IsRegular() {
			return p, nil
		}
	}
	return "", fmt.Errorf("%s not found; Snugbox needs systemd", name)
}

func runSystem(ctx context.Context, name string, args ...string) error {
	bin, err := systemBinary(name)
	if err != nil {
		return err
	}
	cmd := exec.CommandContext(ctx, bin, args...) //nolint:gosec // bin is a fixed system tool and args are fixed by the caller
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("%s %v: %w: %s", name, args, err, out)
	}
	return nil
}

func newWatch(e *env) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "watch",
		Short: "Run the watcher as a service, see how it is, or remove it",
		Long: "snugboxd samples the box every 10 seconds, keeps 30 days of history and works out the snug\n" +
			"size every 5 minutes. Confidence reaches medium after 7 days and high after 14.",
		Args: cobra.ArbitraryArgs,
		RunE: groupRunE,
	}
	cmd.AddCommand(newWatchInstall(e), newWatchStatus(e), newWatchUninstall(e))
	return cmd
}

func newWatchInstall(e *env) *cobra.Command {
	return &cobra.Command{
		Use:     "install",
		Short:   "Install and start snugboxd and the helper that applies fixes",
		Example: "  sudo snugbox watch install",
		Args:    cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if err := e.requireRoot(); err != nil {
				return err
			}
			ctx := cmd.Context()
			bindir, err := binDir()
			if err != nil {
				return err
			}
			for _, bin := range []string{"snugboxd", "snugbox-helper"} {
				if _, err := os.Stat(filepath.Join(bindir, bin)); err != nil {
					return fmt.Errorf("%s is not next to snugbox in %s; reinstall Snugbox", bin, bindir)
				}
			}
			watcher, err := units.Watcher(bindir)
			if err != nil {
				return err
			}
			helper, err := units.Helper(filepath.Join(bindir, "snugbox-helper"))
			if err != nil {
				return err
			}
			for _, u := range []struct {
				name string
				body []byte
			}{{units.WatcherName, watcher}, {units.HelperName, helper}, {units.HelperSocket, units.Socket()}} {
				if _, err := os.Stat(filepath.Join(units.PackageUnitDir, u.name)); err == nil {
					continue
				}
				if err := writeSystemFile(filepath.Join(units.LocalUnitDir, u.name), u.body, 0o644); err != nil {
					return err
				}
			}
			sysusers := units.SysusersFile
			if _, err := os.Stat(sysusers); err != nil {
				sysusers = units.LocalSysusers
				if err := writeSystemFile(sysusers, units.Sysusers(), 0o644); err != nil {
					return err
				}
			}
			// A package installs the settings file itself; one already
			// there, edited or not, is kept.
			if _, err := createFile(units.SettingsFile, units.Settings(), 0o600); err != nil {
				return err
			}
			for _, step := range [][]string{
				{"systemd-sysusers", sysusers},
				{"systemctl", "daemon-reload"},
				{"systemctl", "enable", "--now", units.HelperSocket},
				{"systemctl", "enable", units.WatcherName},
				{"systemctl", "restart", units.WatcherName},
			} {
				if err := runSystem(ctx, step[0], step[1:]...); err != nil {
					return err
				}
			}
			if err := waitForWatcher(ctx, e.watcher, 15*time.Second); err != nil {
				return err
			}
			fmt.Fprintln(e.stdout, "snugboxd is running. It samples every 10 seconds and keeps 30 days in /var/lib/snugbox.")
			fmt.Fprintln(e.stdout, "The first estimate is ready in about 5 minutes; confidence reaches medium after 7 days.")
			fmt.Fprintln(e.stdout, "See it any time with snugbox audit, or live with snugbox top. Fixes are in snugbox fit.")
			fmt.Fprintln(e.stdout, "Its settings, such as a webhook for early warnings, are in "+units.SettingsFile+".")
			return nil
		},
	}
}

func binDir() (string, error) {
	exe, err := os.Executable()
	if err != nil {
		return "", err
	}
	exe, err = filepath.EvalSymlinks(exe)
	if err != nil {
		return "", err
	}
	return filepath.Dir(exe), nil
}

func waitForWatcher(ctx context.Context, c *localapi.Client, limit time.Duration) error {
	deadline := time.Now().Add(limit)
	for {
		_, err := c.Status(ctx)
		if err == nil {
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("snugboxd started but its socket did not answer within %s: %w; see journalctl -u snugboxd", report.Span(limit), err)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(500 * time.Millisecond):
		}
	}
}

func newWatchStatus(e *env) *cobra.Command {
	return &cobra.Command{
		Use:   "status",
		Short: "Show whether snugboxd runs and what it has seen",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			c := e.watcher
			st, err := c.Status(cmd.Context())
			if err != nil {
				return err
			}
			fmt.Fprintf(e.stdout, "snugboxd %s, running since %s.\n", st.Version, report.Stamp(st.Started.Local()))
			if !st.WatchStart.IsZero() {
				fmt.Fprintf(e.stdout, "Watching since %s, %s of history in %s.\n", report.Stamp(st.WatchStart.Local()), report.Span(time.Since(st.WatchStart)), report.Size(uint64(max(st.StoreBytes, 0))))
			}
			fmt.Fprintf(e.stdout, "Last sample at %s, every %s, %d workloads.\n", report.Stamp(st.LastSample.Local()), report.Span(st.Interval), st.Workloads)
			if st.Paused {
				fmt.Fprintln(e.stdout, "Paused: no fixes run and nothing goes to a hub until sudo snugbox resume.")
			}
			if !st.Analysed {
				fmt.Fprintln(e.stdout, "The first estimate is not ready yet.")
				return nil
			}
			r, err := c.Result(cmd.Context())
			if err != nil {
				return err
			}
			fmt.Fprintln(e.stdout, report.Headline(r))
			fmt.Fprintf(e.stdout, "Confidence %s: %s\n", r.Estimate.Confidence, report.ConfidenceDetail(r.Estimate))
			return nil
		},
	}
}

func newWatchUninstall(e *env) *cobra.Command {
	var purge, keepFixes bool
	cmd := &cobra.Command{
		Use:   "uninstall",
		Short: "Stop snugboxd and the helper and remove their services",
		Long: "Removes the watcher and the helper. A fix still in place needs the helper to be undone, so\n" +
			"uninstall stops if there is one, unless --keep-fixes says to leave the fixes as they are.\n" +
			"The helper's journal and snapshots in /var/lib/snugbox-helper are always kept.",
		Example: "  sudo snugbox watch uninstall\n  sudo snugbox watch uninstall --purge",
		Args:    cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if err := e.requireRoot(); err != nil {
				return err
			}
			ctx := cmd.Context()
			if !keepFixes {
				if err := noFixesInPlace(ctx, e.helper); err != nil {
					return err
				}
			}
			for _, u := range []struct{ name, verb string }{
				{units.WatcherName, "disable"}, {units.HelperSocket, "disable"}, {units.HelperName, "stop"},
			} {
				if !unitInstalled(u.name) {
					continue
				}
				args := []string{u.verb, u.name}
				if u.verb == "disable" {
					args = []string{"disable", "--now", u.name}
				}
				if err := runSystem(ctx, "systemctl", args...); err != nil {
					return err
				}
			}
			for _, name := range []string{units.WatcherName, units.HelperName, units.HelperSocket} {
				if err := os.Remove(filepath.Join(units.LocalUnitDir, name)); err != nil && !errors.Is(err, os.ErrNotExist) {
					return err
				}
			}
			if err := runSystem(ctx, "systemctl", "daemon-reload"); err != nil {
				return err
			}
			if purge {
				for _, f := range []string{localapi.DefaultDB, localapi.DefaultDB + "-wal", localapi.DefaultDB + "-shm"} {
					if err := os.Remove(f); err != nil && !errors.Is(err, os.ErrNotExist) {
						return err
					}
				}
				fmt.Fprintln(e.stdout, "snugboxd and the helper are removed and the watcher's history is deleted.")
				fmt.Fprintln(e.stdout, "Kept: the settings in /etc/snugbox, reports in /var/lib/snugbox/reports, and the helper's journal in /var/lib/snugbox-helper.")
				return nil
			}
			fmt.Fprintln(e.stdout, "snugboxd and the helper are removed. The history stays in /var/lib/snugbox; --purge deletes it.")
			return nil
		},
	}
	cmd.Flags().BoolVar(&purge, "purge", false, "also delete the watcher's history")
	cmd.Flags().BoolVar(&keepFixes, "keep-fixes", false, "remove Snugbox even though fixes are in place, leaving them as they are")
	return cmd
}

// unitInstalled reports whether systemd has a unit file by that name in
// either place Snugbox installs one.
func unitInstalled(name string) bool {
	for _, dir := range []string{units.LocalUnitDir, units.PackageUnitDir} {
		if _, err := os.Stat(filepath.Join(dir, name)); err == nil {
			return true
		}
	}
	return false
}

// noFixesInPlace refuses to remove the helper while a fix it applied is
// still in place, since only the helper can put it back.
func noFixesInPlace(ctx context.Context, helper *helperapi.Client) error {
	txs, err := helper.List(ctx)
	if errors.Is(err, helperapi.ErrNoHelper) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("can't ask the helper which fixes are in place (%w); --keep-fixes removes Snugbox anyway", err)
	}
	var in []string
	for _, tx := range txs {
		switch tx.GetState() {
		case helperv1.State_STATE_VERIFYING, helperv1.State_STATE_WATCHING, helperv1.State_STATE_DONE, helperv1.State_STATE_REOPENED:
			in = append(in, tx.GetId())
		}
	}
	if len(in) == 0 {
		return nil
	}
	msg := fmt.Sprintf("%d fixes are in place (%s), and only the helper can put them back", len(in), strings.Join(in, ", "))
	if len(in) == 1 {
		msg = fmt.Sprintf("a fix is in place (%s), and only the helper can put it back", in[0])
	}
	return &exitError{code: ExitError, err: errors.New(msg),
		hint: "Put fixes back first with sudo snugbox undo <transaction>, or add --keep-fixes to leave them in place; every file a fix wrote says how to remove it."}
}

func newPause(e *env) *cobra.Command {
	return &cobra.Command{
		Use:   "pause",
		Short: "Stop all fixes and all hub traffic until snugbox resume",
		Long: "Pause is the kill switch of blueprint section 6.2. While it is in force no fix runs, no\n" +
			"automatic undo runs, and nothing is sent to a hub. A fix being applied stops within a few\n" +
			"seconds, and what it had changed is put back. Sampling goes on, so history has no gap.",
		Example: "  sudo snugbox pause",
		Args:    cobra.NoArgs,
		RunE: func(*cobra.Command, []string) error {
			if err := e.requireRoot(); err != nil {
				return err
			}
			existed, err := createFile(e.pauseFile, []byte("paused at "+time.Now().UTC().Format(time.RFC3339)+"\n"), 0o644)
			if err != nil {
				return err
			}
			if existed {
				fmt.Fprintln(e.stdout, "Already paused. Run sudo snugbox resume to start again.")
				return nil
			}
			fmt.Fprintln(e.stdout, "Paused. No fix runs and nothing goes to a hub until sudo snugbox resume. Sampling goes on.")
			return nil
		},
	}
}

func newResume(e *env) *cobra.Command {
	return &cobra.Command{
		Use:     "resume",
		Short:   "Let fixes and hub traffic run again after snugbox pause",
		Example: "  sudo snugbox resume",
		Args:    cobra.NoArgs,
		RunE: func(*cobra.Command, []string) error {
			if err := e.requireRoot(); err != nil {
				return err
			}
			err := os.Remove(e.pauseFile)
			switch {
			case errors.Is(err, os.ErrNotExist):
				fmt.Fprintln(e.stdout, "Not paused.")
			case err != nil:
				return err
			default:
				fmt.Fprintln(e.stdout, "Resumed. Fixes and hub traffic can run again.")
			}
			return nil
		},
	}
}
