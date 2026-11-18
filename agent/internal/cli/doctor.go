package cli

import (
	"context"
	"errors"
	"fmt"
	"os"
	"runtime"

	"github.com/spf13/cobra"

	"snugbox.dev/snugbox/agent/internal/collect"
	"snugbox.dev/snugbox/agent/internal/docker"
	"snugbox.dev/snugbox/agent/internal/doctor"
	"snugbox.dev/snugbox/agent/internal/report"
)

func newDoctor(e *env) *cobra.Command {
	return &cobra.Command{
		Use:   "doctor",
		Short: "Check the kernel, cgroups, pressure files, zswap, virtualisation and permissions",
		Long: "Check whether this box has everything Snugbox reads, and say what is missing.\n" +
			"Exit code 1 means a check failed and the audit can't run here.",
		Example: "  sudo snugbox doctor",
		Args:    cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if runtime.GOOS != "linux" {
				return fmt.Errorf("snugbox measures Linux servers; this is %s", runtime.GOOS)
			}
			root := os.DirFS("/")
			c, err := collect.New(collect.Options{FS: root})
			if err != nil {
				return err
			}
			if err := c.Discover(cmd.Context()); err != nil {
				return err
			}
			in := doctor.Inputs{
				FS:         root,
				Machine:    c.Machine(cmd.Context(), collect.LiveHost{}),
				Root:       e.euid == 0,
				CgroupRoot: c.CgroupRoot(),
				Readable:   readable,
			}
			if _, err := os.Stat(docker.DefaultSocket); err == nil {
				client := docker.New(docker.DefaultSocket)
				in.Docker = func(ctx context.Context) (string, error) {
					v, err := client.Version(ctx)
					return v.Version, err
				}
			}
			return printDoctor(e, in.Machine.Hostname, doctor.Run(cmd.Context(), in))
		},
	}
}

func readable(path string) bool {
	f, err := os.Open(path) //nolint:gosec // callers pass fixed paths such as /dev/kmsg
	if err != nil {
		return false
	}
	return f.Close() == nil
}

func printDoctor(e *env, host string, checks []doctor.Check) error {
	p := e.color
	paint := func(code, s string) string {
		if !p {
			return s
		}
		return "\x1b[" + code + "m" + s + "\x1b[0m"
	}
	if host == "" {
		host = "this box"
	}
	fmt.Fprintf(e.stdout, "snugbox doctor on %s\n\n", host)
	counts := map[doctor.Status]int{}
	for _, c := range checks {
		counts[c.Status]++
		label := fmt.Sprintf("%-5s", c.Status)
		switch c.Status {
		case doctor.Pass:
			label = paint("32", label)
		case doctor.Warn:
			label = paint("33", label)
		case doctor.Fail:
			label = paint("31", label)
		default:
			label = paint("2", label)
		}
		fmt.Fprintf(e.stdout, "  %s %s\n", label, c.Text)
	}
	fmt.Fprintf(e.stdout, "\n%s: %d ok, %d for information, %s, %d failed.\n",
		report.Count(len(checks), "check", "checks"), counts[doctor.Pass], counts[doctor.Info],
		report.Count(counts[doctor.Warn], "warning", "warnings"), counts[doctor.Fail])
	if counts[doctor.Fail] > 0 {
		return withCode(ExitError, errors.New("a check failed; see above"))
	}
	return nil
}
