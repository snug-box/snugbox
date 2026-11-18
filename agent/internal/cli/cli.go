// Package cli is the snugbox command line of blueprint section 8.1.
package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/signal"
	"strings"
	"syscall"

	"github.com/spf13/cobra"
	"golang.org/x/term"

	"snugbox.dev/snugbox/agent/internal/audit"
	"snugbox.dev/snugbox/agent/internal/helperapi"
	"snugbox.dev/snugbox/agent/internal/localapi"
	"snugbox.dev/snugbox/agent/internal/version"
)

// Exit codes of blueprint section 8.1.
const (
	ExitOK        = 0
	ExitError     = 1
	ExitUsage     = 2
	ExitRisks     = 3
	ExitNeedsRoot = 4
)

// exitError carries an exit code out of a command, and an optional hint
// printed on the line after the error.
type exitError struct {
	code int
	err  error
	hint string
}

func (e *exitError) Error() string {
	if e.err == nil {
		return ""
	}
	return e.err.Error()
}

func (e *exitError) Unwrap() error { return e.err }

func withCode(code int, err error) error { return &exitError{code: code, err: err} }

// usageError is a mistake in the command line, exit code 2, with a pointer
// to the command's help.
func usageError(c *cobra.Command, err error) error {
	return &exitError{code: ExitUsage, err: err, hint: fmt.Sprintf("See '%s --help'.", c.CommandPath())}
}

// groupRunE is the RunE of a command that only holds others, such as snugbox
// watch. Alone it prints its help; a word after it that names none of its
// commands is a usage error, so a typo exits with 2 rather than 0.
func groupRunE(c *cobra.Command, args []string) error {
	if len(args) == 0 {
		return c.Help()
	}
	msg := fmt.Sprintf("%q is not a command of %s", args[0], c.CommandPath())
	// cobra sets the same distance before its own suggestions.
	if c.SuggestionsMinimumDistance <= 0 {
		c.SuggestionsMinimumDistance = 2
	}
	if s := c.SuggestionsFor(args[0]); len(s) > 0 {
		msg += "; did you mean " + strings.Join(s, " or ") + "?"
	}
	return usageError(c, errors.New(msg))
}

// errRisks ends a successful audit that found risks with exit code 3, which
// monitoring scripts can test for. It prints nothing.
var errRisks = &exitError{code: ExitRisks}

// env is what the commands need from the process. Main fills it from the
// running process; tests point it at fake sockets and folders.
type env struct {
	stdout, stderr io.Writer
	color          bool
	stderrTTY      bool
	// stdin is where confirm reads an answer, and stdinTTY says whether a
	// person is there to give one.
	stdin    io.Reader
	stdinTTY bool
	euid     int
	// watcher reaches snugboxd, and helper reaches snugbox-helper.
	watcher *localapi.Client
	helper  *helperapi.Client
	// pauseFile is the flag snugbox pause creates, and reportDir the folder
	// a root audit saves its Markdown report in.
	pauseFile string
	reportDir string
	// sample runs a one-off audit of this box.
	sample func(context.Context, audit.Options) (*audit.Result, error)
	// update is what snugbox self-update reaches.
	update updateEnv
}

// Main runs the command line and returns the process exit code.
func Main(args []string, stdout, stderr io.Writer) int {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	e := &env{
		stdout:    stdout,
		stderr:    stderr,
		color:     os.Getenv("NO_COLOR") == "" && isTerminal(stdout),
		stderrTTY: isTerminal(stderr),
		stdin:     os.Stdin,
		stdinTTY:  isTerminal(os.Stdin),
		euid:      os.Geteuid(),
		watcher:   localapi.NewClient(localapi.DefaultSocket),
		helper:    &helperapi.Client{},
		pauseFile: localapi.DefaultPause,
		reportDir: ReportDir,
		sample:    liveSample,
		update:    defaultUpdateEnv(),
	}
	return e.run(ctx, args)
}

// run executes one command line and returns its exit code.
func (e *env) run(ctx context.Context, args []string) int {
	root := newRoot(e)
	root.SetArgs(args)
	root.SetOut(e.stdout)
	root.SetErr(e.stderr)
	err := root.ExecuteContext(ctx)
	if err == nil {
		return ExitOK
	}
	var ee *exitError
	if errors.As(err, &ee) {
		if ee.err != nil {
			fmt.Fprintln(e.stderr, "snugbox:", ee.err)
		}
		if ee.hint != "" {
			fmt.Fprintln(e.stderr, ee.hint)
		}
		return ee.code
	}
	fmt.Fprintln(e.stderr, "snugbox:", err)
	return ExitError
}

func isTerminal(f any) bool {
	file, ok := f.(*os.File)
	return ok && term.IsTerminal(int(file.Fd()))
}

var errNeedsRoot = errors.New("this command changes the system; run it with sudo")

func (e *env) requireRoot() error {
	if e.euid != 0 {
		return withCode(ExitNeedsRoot, errNeedsRoot)
	}
	return nil
}

func newRoot(e *env) *cobra.Command {
	root := &cobra.Command{
		Use:   "snugbox",
		Short: "Measure how much memory this server needs, trim the waste, and find the cheapest plan that fits",
		Long: "Snugbox measures how much memory a Linux server really needs, trims the waste with changes\n" +
			"you can undo, and names the cheapest plan that fits.\n\n" +
			"Start with a read-only audit:\n\n  sudo snugbox audit",
		Version:       fmt.Sprintf("%s (commit %s, built %s)", version.Version, version.Commit, version.Date),
		Args:          cobra.ArbitraryArgs,
		RunE:          groupRunE,
		SilenceUsage:  true,
		SilenceErrors: true,
	}
	root.SetVersionTemplate("snugbox {{.Version}}\n")
	root.SetFlagErrorFunc(usageError)
	root.AddCommand(newAudit(e), newTop(e), newFit(e), newUndo(e), newPlan(e), newReport(e), newWatch(e), newPause(e), newResume(e), newDoctor(e),
		newSelfUpdate(e), newVersion(e))
	// cobra adds its help and completion commands when the command line
	// runs; adding them now lets wrapAll reach them too.
	root.InitDefaultHelpCmd()
	root.InitDefaultCompletionCmd()
	wrapAll(root)
	return root
}

// wrapAll applies wrapArgs to every command below c.
func wrapAll(c *cobra.Command) {
	for _, sub := range c.Commands() {
		sub.Args = wrapArgs(sub.Args)
		wrapAll(sub)
	}
}

// wrapArgs turns a wrong number of arguments into a usage error, exit code 2.
func wrapArgs(check cobra.PositionalArgs) cobra.PositionalArgs {
	if check == nil {
		return nil
	}
	return func(c *cobra.Command, args []string) error {
		if err := check(c, args); err != nil {
			return usageError(c, err)
		}
		return nil
	}
}

func newVersion(e *env) *cobra.Command {
	return &cobra.Command{
		Use:   "version",
		Short: "Print the version",
		Args:  cobra.NoArgs,
		RunE: func(*cobra.Command, []string) error {
			_, err := fmt.Fprintf(e.stdout, "snugbox %s (commit %s, built %s)\n", version.Version, version.Commit, version.Date)
			return err
		},
	}
}
