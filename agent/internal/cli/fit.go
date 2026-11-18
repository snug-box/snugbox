package cli

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"snugbox.dev/snugbox/agent/internal/audit"
	"snugbox.dev/snugbox/agent/internal/fix"
	"snugbox.dev/snugbox/agent/internal/helperapi"
	"snugbox.dev/snugbox/agent/internal/localapi"
	"snugbox.dev/snugbox/agent/internal/model"
	"snugbox.dev/snugbox/agent/internal/report"
	helperv1 "snugbox.dev/snugbox/gen/snugbox/helper/v1"
)

type fitFlags struct {
	list, dry, status, yes, acceptLow, noPersist bool
	only                                         []string
	keep                                         string
}

var kinds = []fix.Kind{fix.KindSwap, fix.KindOOMProtection, fix.KindContainer, fix.KindService, fix.KindHeap, fix.KindSchedule}

func newFit(e *env) *cobra.Command {
	var o fitFlags
	cmd := &cobra.Command{
		Use:   "fit [<fix>]",
		Short: "List, preview or apply the fixes Snugbox proposes for this box",
		Long: "snugbox fit lists the fixes the watcher's latest analysis proposes. Name one by its number\n" +
			"or its kind to see what it changes, then apply it. Every fix saves the old values first;\n" +
			"snugboxd then watches it for 30 minutes and undoes a limit that hurts, and snugbox undo puts\n" +
			"any fix back.\n\n" +
			"Kinds: swap, oom-protection, container-limits, service-limits, heap-caps, schedule.",
		Example: "  snugbox fit --list\n  snugbox fit --dry-run 2\n  sudo snugbox fit service-limits --only app.service\n" +
			"  snugbox fit --status\n  sudo snugbox fit --keep 20261031-103015-service-limits",
		Args: cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			modes := 0
			for _, on := range []bool{o.list, o.status, o.keep != ""} {
				if on {
					modes++
				}
			}
			switch {
			case modes > 1 || modes == 1 && len(args) > 0:
				return usageError(cmd, errors.New("--list, --status and --keep each stand alone"))
			case o.status:
				return fitStatus(cmd.Context(), e)
			case o.keep != "":
				return fitKeep(cmd.Context(), e, o.keep)
			case len(args) == 0:
				return fitList(cmd.Context(), e)
			}
			return fitApply(cmd, e, args[0], o)
		},
	}
	f := cmd.Flags()
	f.BoolVar(&o.list, "list", false, "list the fixes proposed for this box; the same as no arguments")
	f.BoolVar(&o.dry, "dry-run", false, "show what the fix changes and change nothing")
	f.BoolVar(&o.status, "status", false, "show the fixes applied, being verified or watched")
	f.BoolVar(&o.yes, "yes", false, "apply without asking first, for scripts")
	f.BoolVar(&o.acceptLow, "accept-low-confidence", false, "use limits worked out from less than 7 days of history")
	f.BoolVar(&o.noPersist, "no-persist", false, "don't write compose.override.yaml; container limits then last until the container is made again")
	f.StringSliceVar(&o.only, "only", nil, "apply the fix to these workloads only, by name, comma-separated")
	f.StringVar(&o.keep, "keep", "", "keep a fix that was reopened for review, so it counts as done")
	return cmd
}

// needWatcher returns the watcher's latest analysis. Fixes need the watcher:
// it is what checks a fix for 30 minutes and undoes a limit that hurts.
func needWatcher(ctx context.Context, e *env) (*audit.Result, error) {
	r, err := e.watcher.Result(ctx)
	if errors.Is(err, localapi.ErrNotRunning) {
		return nil, errors.New("snugbox fit needs the watcher, which checks a fix for 30 minutes and undoes a limit that hurts; start it with sudo snugbox watch install")
	}
	return r, err
}

func fitList(ctx context.Context, e *env) error {
	r, err := needWatcher(ctx, e)
	if err != nil {
		return err
	}
	p := report.Painter{Color: e.color}
	if len(r.Fixes) == 0 {
		fmt.Fprintln(e.stdout, report.NoFixes(r))
		for _, n := range report.FixAdvice(r) {
			fmt.Fprintln(e.stdout, p.Dim(n))
		}
		return nil
	}
	fmt.Fprintf(e.stdout, "Fixes for %s, from %s of watching:\n", r.Machine.Hostname, report.Span(r.Estimate.Window))
	width := 0
	for _, f := range r.Fixes {
		width = max(width, len([]rune(report.FixTitle(f))))
	}
	for i, f := range r.Fixes {
		title := report.FixTitle(f)
		pad := strings.Repeat(" ", width-len([]rune(title)))
		fmt.Fprintf(e.stdout, "  %d  %s%s  %s  %s\n", i+1, title, pad, riskWord(p, f.Risk), p.Dim("snugbox fit "+string(f.Kind)))
	}
	fmt.Fprintln(e.stdout, p.Dim("Preview one with snugbox fit --dry-run <number>. Apply it with sudo snugbox fit <number>."))
	for _, n := range report.FixAdvice(r) {
		fmt.Fprintln(e.stdout, p.Dim(n))
	}
	return nil
}

func riskWord(p report.Painter, r fix.Risk) string {
	switch r {
	case fix.RiskLow:
		return p.Good("low risk")
	case fix.RiskMedium:
		return p.Warn("medium risk")
	}
	return p.Bad(string(r) + " risk")
}

// pick finds a proposal by its number in the list or by its kind.
func pick(r *audit.Result, arg string) (fix.Proposal, error) {
	if n, err := strconv.Atoi(arg); err == nil {
		if n < 1 || n > len(r.Fixes) {
			return fix.Proposal{}, fmt.Errorf("there is no fix %d; snugbox fit --list shows %d", n, len(r.Fixes))
		}
		return r.Fixes[n-1], nil
	}
	if !slices.Contains(kinds, fix.Kind(arg)) {
		return fix.Proposal{}, fmt.Errorf("%q is not a fix; use a number from snugbox fit --list or one of %s", arg, kindList())
	}
	for _, p := range r.Fixes {
		if p.Kind == fix.Kind(arg) {
			return p, nil
		}
	}
	return fix.Proposal{}, fmt.Errorf("the watcher proposes no %s fix for this box right now", arg)
}

func kindList() string {
	names := make([]string, len(kinds))
	for i, k := range kinds {
		names[i] = string(k)
	}
	return strings.Join(names, ", ")
}

// lowConfidence reports whether any limit the fix would set comes from less
// than seven days of history.
func lowConfidence(p fix.Proposal, only []string) bool {
	for _, l := range p.Limits {
		if l.LowConfidence && (len(only) == 0 || slices.Contains(only, l.Name)) {
			return true
		}
	}
	return false
}

func fitApply(cmd *cobra.Command, e *env, arg string, o fitFlags) error {
	ctx := cmd.Context()
	r, err := needWatcher(ctx, e)
	if err != nil {
		return err
	}
	p, err := pick(r, arg)
	if err != nil {
		return usageError(cmd, err)
	}
	req, advice, err := fix.Request(p, r.Workloads, fix.RequestOptions{Only: o.only, Persist: !o.noPersist})
	if err != nil {
		return usageError(cmd, err)
	}
	paint := report.Painter{Color: e.color}
	fmt.Fprintf(e.stdout, "%s (%s)\n", paint.Strong(report.FixTitle(p)), riskWord(paint, p.Risk))
	for _, a := range advice {
		fmt.Fprintln(e.stdout, "  "+a)
	}
	if req == nil {
		fmt.Fprintln(e.stdout, "Nothing here can be applied by Snugbox; the lines above are for you.")
		return nil
	}
	if lowConfidence(p, o.only) && !o.acceptLow && !o.dry {
		return usageError(cmd, fmt.Errorf("these limits come from %s of history, and Snugbox wants 7 days before it trusts a limit; "+
			"wait, or run again with --accept-low-confidence to use them now", report.Span(r.Estimate.Window)))
	}
	if e.euid != 0 {
		if !o.dry {
			return e.requireRoot()
		}
		for _, line := range describe(req, r.Workloads) {
			fmt.Fprintln(e.stdout, "  "+line)
		}
		fmt.Fprintln(e.stdout, paint.Dim("Run it with sudo to see every file and old value the helper would change. Nothing was changed."))
		return nil
	}
	st, err := e.watcher.Status(ctx)
	if err != nil {
		return err
	}
	if st.Paused {
		return errors.New("snugbox pause is in force, so no fix runs; run sudo snugbox resume first")
	}
	helper := e.helper
	preview, err := helper.Apply(ctx, &helperv1.ApplyRequest{Fix: req, DryRun: true}, nil)
	if err != nil {
		return helperError(err)
	}
	printChanges(e, paint, preview.GetChanges(), preview.GetNotes())
	if o.dry {
		fmt.Fprintln(e.stdout, paint.Dim("Nothing was changed."))
		return nil
	}
	if !o.yes {
		ok, err := confirm(e, "Apply this fix? Type yes to go ahead: ")
		if err != nil {
			return err
		}
		if !ok {
			fmt.Fprintln(e.stdout, "Nothing was changed.")
			return nil
		}
	}
	resp, err := helper.Apply(ctx, &helperv1.ApplyRequest{Fix: req, AcceptLowConfidence: o.acceptLow}, progressPrinter(e, paint))
	if err != nil {
		return helperError(err)
	}
	if err := e.watcher.Nudge(ctx); err != nil {
		fmt.Fprintf(e.stderr, "snugbox: the watcher didn't answer (%v); it will find the fix within 5 minutes.\n", err)
	}
	tx := resp.GetTransaction()
	fmt.Fprintf(e.stdout, "Applied as %s. snugboxd watches it for 30 minutes and undoes a limit that hurts.\n", paint.Strong(tx))
	fmt.Fprintf(e.stdout, "See how it goes with snugbox fit --status. Put it back with sudo snugbox undo %s.\n", tx)
	return nil
}

// describe says what a fix asks for, for a person without root, who can't
// ask the helper for the exact files.
func describe(f *helperv1.Fix, ws map[string]model.Workload) []string {
	names := map[string]string{}
	for _, w := range ws {
		if w.ContainerID != "" {
			names[w.ContainerID] = w.Label()
		}
	}
	var out []string
	switch k := f.GetKind().(type) {
	case *helperv1.Fix_Swap:
		s := k.Swap
		out = append(out, fmt.Sprintf("Makes a %s swap file, and a systemd unit that turns it on at every boot.", report.Size(s.GetSizeBytes())))
		switch {
		case s.GetCompressor() == helperv1.Compressor_COMPRESSOR_UNSPECIFIED:
			out = append(out, "This kernel has no zswap, so nothing is compressed.")
		case s.GetShrinker():
			out = append(out, fmt.Sprintf("Turns zswap on with %s and its shrinker, which moves cold pages on to the swap file.", compressorName(s.GetCompressor())))
		default:
			out = append(out, fmt.Sprintf("Turns zswap on with %s.", compressorName(s.GetCompressor())))
		}
	case *helperv1.Fix_OomProtection:
		if k.OomProtection.GetDaemon() == helperv1.OOMDaemon_OOM_DAEMON_EARLYOOM {
			out = append(out, "Turns on earlyoom, which stops the largest process when free memory and free swap both fall under 10%, before the kernel has to.",
				"Keeps sshd, systemd and Snugbox out of its reach, so you can always log in.")
			break
		}
		out = append(out, "Turns on systemd-oomd for the system and user slices, which stops the workload under the most memory pressure before the kernel has to.",
			"Keeps sshd out of its reach, so you can always log in.")
	case *helperv1.Fix_ContainerLimits:
		for _, l := range k.ContainerLimits.GetLimits() {
			line := fmt.Sprintf("Container %s: limit %s", names[l.GetContainerId()], report.MiB(l.GetMaxBytes()))
			if r := l.GetReservationBytes(); r > 0 {
				line += ", soft limit " + report.MiB(r)
			}
			out = append(out, line+".")
		}
	case *helperv1.Fix_ServiceLimits:
		for _, l := range k.ServiceLimits.GetLimits() {
			out = append(out, fmt.Sprintf("%s: MemoryHigh %s, MemoryMax %s.", l.GetUnit(), report.MiB(l.GetHighBytes()), report.MiB(l.GetMaxBytes())))
		}
	case *helperv1.Fix_HeapCaps:
		for _, c := range k.HeapCaps.GetCaps() {
			out = append(out, fmt.Sprintf("%s: %s, then a restart.", c.GetUnit(), heapText(c)))
		}
	case *helperv1.Fix_Schedule:
		for _, s := range k.Schedule.GetShifts() {
			out = append(out, fmt.Sprintf("%s: starts %s %s earlier, with a drop-in that moves its OnCalendar.",
				s.GetTimer(), s.GetService(), report.Count(int(s.GetMinutes()), "minute", "minutes")))
		}
	}
	return out
}

func compressorName(c helperv1.Compressor) string {
	if c == helperv1.Compressor_COMPRESSOR_LZ4 {
		return "lz4"
	}
	return "zstd"
}

func heapText(c *helperv1.HeapCap) string {
	m := c.GetHeapBytes() / model.MiB
	switch c.GetRuntime() {
	case helperv1.Runtime_RUNTIME_JAVA:
		return fmt.Sprintf("JAVA_TOOL_OPTIONS=-Xmx%dm", m)
	case helperv1.Runtime_RUNTIME_NODE:
		return fmt.Sprintf("NODE_OPTIONS=--max-old-space-size=%d", m)
	case helperv1.Runtime_RUNTIME_GO:
		return fmt.Sprintf("GOMEMLIMIT=%dMiB", m)
	}
	return "MALLOC_ARENA_MAX=2"
}

// printChanges shows each change: a new file line by line, a setting as its
// old and new value, and what the person must change by hand.
func printChanges(e *env, p report.Painter, cs []*helperv1.Change, notes []string) {
	var manual []*helperv1.Change
	fmt.Fprintln(e.stdout, "Changes:")
	for _, c := range cs {
		if c.GetManual() {
			manual = append(manual, c)
			continue
		}
		if !c.GetFile() {
			fmt.Fprintf(e.stdout, "  %s: %s -> %s\n", c.GetTarget(), orNone(c.GetOldValue()), c.GetNewValue())
			continue
		}
		state := "new file"
		if c.GetOldValue() != "" {
			state = "replaced"
		}
		fmt.Fprintf(e.stdout, "  %s (%s)\n", c.GetTarget(), state)
		for line := range strings.Lines(c.GetOldValue()) {
			fmt.Fprintln(e.stdout, "    "+p.Bad("- "+strings.TrimRight(line, "\n")))
		}
		for line := range strings.Lines(c.GetNewValue()) {
			fmt.Fprintln(e.stdout, "    "+p.Good("+ "+strings.TrimRight(line, "\n")))
		}
	}
	if len(manual) > 0 {
		fmt.Fprintln(e.stdout, "For you to add, since Snugbox won't write these:")
		for _, c := range manual {
			fmt.Fprintf(e.stdout, "  %s\n", c.GetTarget())
			for line := range strings.Lines(c.GetNewValue()) {
				fmt.Fprintln(e.stdout, "    "+strings.TrimRight(line, "\n"))
			}
		}
	}
	for _, n := range notes {
		fmt.Fprintln(e.stdout, p.Dim("Note: "+n))
	}
}

func orNone(s string) string {
	if s == "" {
		return "none"
	}
	return s
}

func progressPrinter(e *env, p report.Painter) func(*helperv1.Progress) {
	return func(pr *helperv1.Progress) {
		if pr.GetDetail() == "" {
			return
		}
		fmt.Fprintf(e.stdout, "  %-10s %s\n", p.Dim(helperapi.StateName(pr.GetState())), pr.GetDetail())
	}
}

// confirm asks a yes-or-no question on the terminal. Without a terminal it
// refuses, so a script never applies a fix by accident.
func confirm(e *env, question string) (bool, error) {
	if !e.stdinTTY {
		return false, withCode(ExitUsage, errors.New("standard input is not a terminal, so Snugbox can't ask; add --yes to apply without asking"))
	}
	fmt.Fprint(e.stdout, question)
	line, err := bufio.NewReader(e.stdin).ReadString('\n')
	if errors.Is(err, io.EOF) {
		fmt.Fprintln(e.stdout)
		return false, nil
	}
	if err != nil {
		return false, err
	}
	answer := strings.ToLower(strings.TrimSpace(line))
	return answer == "yes" || answer == "y", nil
}

// helperError gives a helper refusal its own line, and the codes a person
// can do something about a hint.
func helperError(err error) error {
	var he *helperapi.Error
	if !errors.As(err, &he) {
		return err
	}
	hint := ""
	switch he.Code {
	case helperv1.ErrorCode_ERROR_CODE_BUSY:
		hint = "See snugbox fit --status."
	case helperv1.ErrorCode_ERROR_CODE_STUCK:
		hint = "Nothing more is changed until a person looks; see https://snugbox.dev/docs/fixes/stuck/"
	}
	return &exitError{code: ExitError, err: errors.New(he.Message), hint: hint}
}

// The watcher reads the helper's journal at its next sample after a nudge,
// so for a few seconds after snugbox fit its answer can miss the new fix.
// fit --status waits up to pendingWait, longer than a slowed-down sample
// interval, for it to catch up.
const (
	pendingWait = 30 * time.Second
	pendingPoll = time.Second
)

func fitStatus(ctx context.Context, e *env) error {
	paint := report.Painter{Color: e.color}
	ch, err := e.watcher.Changes(ctx)
	if err == nil && ch.Pending {
		fmt.Fprintln(e.stderr, paint.Dim("The watcher reads the helper's newest changes at its next sample; waiting for it."))
		for deadline := time.Now().Add(pendingWait); err == nil && ch.Pending && time.Now().Before(deadline); {
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(pendingPoll):
			}
			ch, err = e.watcher.Changes(ctx)
		}
		if err == nil && ch.Pending {
			fmt.Fprintln(e.stderr, paint.Dim("It hasn't yet, so a fix applied a moment ago may be missing below."))
		}
	}
	if err != nil {
		if !errors.Is(err, localapi.ErrNotRunning) || e.euid != 0 {
			return err
		}
		if ch, err = changesFromHelper(ctx, e.helper); err != nil {
			return helperError(err)
		}
		fmt.Fprintln(e.stderr, paint.Dim("snugboxd is not running, so nothing is being verified; this is the helper's journal."))
	}
	if !ch.Helper && ch.Error != "" {
		fmt.Fprintf(e.stderr, "snugbox: the watcher can't reach snugbox-helper: %s\n", ch.Error)
	}
	if len(ch.Changes) == 0 {
		fmt.Fprintln(e.stdout, "No fix has been applied on this box.")
		return nil
	}
	now := time.Now()
	for _, c := range ch.Changes {
		fmt.Fprintf(e.stdout, "%s  %s, %s since %s\n", paint.Strong(c.ID), c.Kind, stateWord(paint, c.State), report.Stamp(c.Since.Local()))
		switch {
		case c.Trouble != "":
			fmt.Fprintf(e.stdout, "  %s %s\n", paint.Warn("Trouble:"), c.Trouble)
		case c.State == "verifying":
			fmt.Fprintf(e.stdout, "  Verified until %s, %s from now.\n", report.Stamp(c.VerifyEnds.Local()), report.Span(max(c.VerifyEnds.Sub(now), time.Minute)))
		case c.State == "watching" && c.Counted:
			fmt.Fprintln(e.stdout, "  A full day has passed without trouble, so it counts as done; watching ends after 7 days.")
		case c.State == "watching":
			fmt.Fprintln(e.stdout, "  Watched for 7 days; it counts as done after a full day without trouble.")
		case c.State == "reopened":
			fmt.Fprintf(e.stdout, "  %s %s\n  Keep it with sudo snugbox fit --keep %s, or put it back with sudo snugbox undo %s.\n", paint.Warn("Reopened:"), c.Reason, c.ID, c.ID)
		case c.Reason != "":
			fmt.Fprintf(e.stdout, "  %s\n", c.Reason)
		}
	}
	return nil
}

func stateWord(p report.Painter, s string) string {
	switch s {
	case "done", "watching":
		return p.Good(s)
	case "reopened", "failed":
		return p.Bad(s)
	case "verifying":
		return p.Warn(s)
	}
	return s
}

// changesFromHelper reads the journal through the helper when the watcher
// is down, in the same shape the watcher serves.
func changesFromHelper(ctx context.Context, helper *helperapi.Client) (localapi.Changes, error) {
	txs, err := helper.List(ctx)
	if err != nil {
		return localapi.Changes{}, err
	}
	out := localapi.Changes{Helper: true, Read: time.Now()}
	for _, tx := range txs {
		c := localapi.Change{ID: tx.GetId(), Kind: helperapi.KindOf(tx.GetFix()), State: helperapi.StateName(tx.GetState()),
			Started: tx.GetStarted().AsTime(), Since: tx.GetStateSince().AsTime(), Reason: tx.GetReason()}
		if tx.GetState() == helperv1.State_STATE_VERIFYING {
			c.VerifyEnds = c.Since.Add(30 * time.Minute)
		}
		out.Changes = append(out.Changes, c)
	}
	return out, nil
}

func fitKeep(ctx context.Context, e *env, tx string) error {
	if err := e.requireRoot(); err != nil {
		return err
	}
	if !helperapi.ValidTransaction(tx) {
		return withCode(ExitUsage, fmt.Errorf("%q is not a transaction ID; snugbox fit --status lists them", tx))
	}
	if _, err := e.helper.Advance(ctx, tx, helperv1.State_STATE_DONE, ""); err != nil {
		return helperError(err)
	}
	if err := e.watcher.Nudge(ctx); err != nil && !errors.Is(err, localapi.ErrNotRunning) {
		fmt.Fprintf(e.stderr, "snugbox: the watcher didn't answer: %v\n", err)
	}
	_, err := fmt.Fprintf(e.stdout, "Kept. %s counts as done.\n", tx)
	return err
}

func newUndo(e *env) *cobra.Command {
	var last bool
	cmd := &cobra.Command{
		Use:   "undo [--last] [<transaction>]",
		Short: "Put a fix back",
		Long: "snugbox undo puts back what a fix changed, from the snapshot taken before it ran, and\n" +
			"records that in the journal. A file someone edited since is left as it is, and swap is\n" +
			"turned off only when the box has the free memory to take what is in it.",
		Example: "  sudo snugbox undo --last\n  sudo snugbox undo 20261031-103015-swap",
		Args:    cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if last && len(args) == 1 {
				return usageError(cmd, errors.New("name a transaction or use --last, not both"))
			}
			if err := e.requireRoot(); err != nil {
				return err
			}
			ctx := cmd.Context()
			helper := e.helper
			if !last && len(args) == 0 {
				return listUndoable(ctx, e, cmd, helper)
			}
			tx := ""
			if len(args) == 1 {
				if tx = args[0]; !helperapi.ValidTransaction(tx) {
					return usageError(cmd, fmt.Errorf("%q is not a transaction ID", tx))
				}
			}
			paint := report.Painter{Color: e.color}
			resp, err := helper.Undo(ctx, tx, "", progressPrinter(e, paint))
			if err != nil {
				return helperError(err)
			}
			for _, n := range resp.GetNotes() {
				fmt.Fprintln(e.stdout, paint.Dim("Note: "+n))
			}
			if err := e.watcher.Nudge(ctx); err != nil && !errors.Is(err, localapi.ErrNotRunning) {
				fmt.Fprintf(e.stderr, "snugbox: the watcher didn't answer: %v\n", err)
			}
			_, err = fmt.Fprintf(e.stdout, "Put back. %s is undone.\n", resp.GetTransaction())
			return err
		},
	}
	cmd.Flags().BoolVar(&last, "last", false, "put back the newest fix that is still in place")
	return cmd
}

func listUndoable(ctx context.Context, e *env, cmd *cobra.Command, helper *helperapi.Client) error {
	txs, err := helper.List(ctx)
	if err != nil {
		return helperError(err)
	}
	var in []string
	for _, tx := range txs {
		switch tx.GetState() {
		case helperv1.State_STATE_VERIFYING, helperv1.State_STATE_WATCHING, helperv1.State_STATE_DONE, helperv1.State_STATE_REOPENED:
			in = append(in, fmt.Sprintf("  %s  %s, %s", tx.GetId(), helperapi.KindOf(tx.GetFix()), helperapi.StateName(tx.GetState())))
		}
	}
	if len(in) == 0 {
		fmt.Fprintln(e.stdout, "No fix is in place, so there is nothing to undo.")
		return nil
	}
	fmt.Fprintln(e.stdout, "Fixes in place, newest first:")
	for _, line := range in {
		fmt.Fprintln(e.stdout, line)
	}
	return usageError(cmd, errors.New("name one of these, or use --last"))
}
