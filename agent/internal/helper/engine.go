package helper

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"slices"
	"strings"
	"time"

	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/types/known/timestamppb"

	"snugbox.dev/snugbox/agent/internal/docker"
	"snugbox.dev/snugbox/agent/internal/helperapi"
	"snugbox.dev/snugbox/agent/internal/journal"
	helperv1 "snugbox.dev/snugbox/gen/snugbox/helper/v1"
)

// Windows of blueprint section 5.6.
const (
	VerifyWindow = 30 * time.Minute
	WatchWindow  = 7 * 24 * time.Hour
	maxListed    = 100
	maxListedCts = 500
)

// Caller is who sent a request: root, through snugbox, or snugboxd as the
// snugbox user.
type Caller int

// The callers the helper accepts.
const (
	Root Caller = iota + 1
	Watcher
)

func (c Caller) String() string {
	if c == Root {
		return "root"
	}
	return "snugboxd"
}

// stepName is the journal's word for a state.
func stepName(s helperv1.State) string { return helperapi.StateName(s) }

func stateOf(name string) helperv1.State { return helperapi.StateFromName(name) }

// inFlight states are the ones the helper itself is in the middle of; a
// transaction found in one at start was cut short.
func inFlight(s helperv1.State) bool {
	switch s {
	case helperv1.State_STATE_APPROVED, helperv1.State_STATE_PREFLIGHT, helperv1.State_STATE_SNAPSHOT,
		helperv1.State_STATE_APPLYING, helperv1.State_STATE_UNDOING:
		return true
	}
	return false
}

func applied(s helperv1.State) bool {
	switch s {
	case helperv1.State_STATE_VERIFYING, helperv1.State_STATE_WATCHING, helperv1.State_STATE_DONE, helperv1.State_STATE_REOPENED:
		return true
	}
	return false
}

// Config sets up an Engine.
type Config struct {
	// Exe is the helper's own binary, which systemd-run starts as a Compose
	// folder's owner to write an override file.
	Exe      string
	Pause    string
	MemTotal uint64
	Log      *slog.Logger
}

// Engine answers requests one at a time and keeps the journal.
type Engine struct {
	sys   System
	jr    *journal.Journal
	exe   string
	pause string
	box   helperapi.Box
	log   *slog.Logger
}

// New returns an engine that records into jr.
func New(sys System, jr *journal.Journal, cfg Config) *Engine {
	return &Engine{sys: sys, jr: jr, exe: cfg.Exe, pause: cfg.Pause, box: helperapi.Box{MemTotal: cfg.MemTotal}, log: cfg.Log}
}

type approvedData struct {
	Fix           json.RawMessage `json:"fix"`
	LowConfidence bool            `json:"low_confidence_accepted,omitempty"`
}

type snapshotData struct {
	Manifest string   `json:"manifest_sha256"`
	Cgroups  []string `json:"cgroups,omitempty"`
	AutoUndo bool     `json:"auto_undo"`
	Changes  []change `json:"changes"`
}

type change struct {
	Target string `json:"target"`
	Old    string `json:"old,omitempty"`
	New    string `json:"new,omitempty"`
	File   bool   `json:"file,omitempty"`
	Manual bool   `json:"manual,omitempty"`
}

// view is a transaction as the journal tells it.
type view struct {
	id       string
	fix      *helperv1.Fix
	state    helperv1.State
	started  time.Time
	since    time.Time
	reason   string
	manifest string
	cgroups  []string
	autoUndo bool
	changes  []change
}

func (e *Engine) views() []*view {
	byID := map[string]*view{}
	var order []*view
	for _, en := range e.jr.Entries() {
		v := byID[en.Tx]
		if v == nil {
			v = &view{id: en.Tx, started: en.Time}
			byID[en.Tx] = v
			order = append(order, v)
		}
		v.state, v.since = stateOf(en.State), en.Time
		switch v.state {
		case helperv1.State_STATE_UNDONE:
			// The undoing line says why; the undone line adds what was
			// left as it was, if anything.
			if en.Note != "" {
				v.reason = strings.TrimSpace(v.reason + " " + en.Note)
			}
		case helperv1.State_STATE_UNDOING, helperv1.State_STATE_REOPENED, helperv1.State_STATE_FAILED, helperv1.State_STATE_DONE:
			if en.Note != "" {
				v.reason = en.Note
			}
		default:
			v.reason = ""
		}
		switch en.State {
		case "approved":
			var d approvedData
			if json.Unmarshal(en.Data, &d) == nil {
				fix := &helperv1.Fix{}
				if protojson.Unmarshal(d.Fix, fix) == nil {
					v.fix = fix
				}
			}
		case "snapshot":
			var d snapshotData
			if json.Unmarshal(en.Data, &d) == nil {
				v.manifest, v.cgroups, v.autoUndo, v.changes = d.Manifest, d.Cgroups, d.AutoUndo, d.Changes
			}
		}
	}
	slices.Reverse(order)
	return order
}

func (e *Engine) find(id string) *view {
	for _, v := range e.views() {
		if v.id == id {
			return v
		}
	}
	return nil
}

func (v *view) proto() *helperv1.Transaction {
	t := &helperv1.Transaction{Id: v.id, Fix: v.fix, State: v.state, Started: timestamppb.New(v.started),
		StateSince: timestamppb.New(v.since), Cgroups: v.cgroups, Reason: v.reason, AutoUndo: v.autoUndo}
	for _, c := range v.changes {
		t.Changes = append(t.Changes, &helperv1.Change{Target: c.Target, OldValue: c.Old, NewValue: c.New, File: c.File, Manual: c.Manual})
	}
	return t
}

// step records a state change, logs it and passes it to the client.
func (e *Engine) step(tx string, s helperv1.State, by, note string, data any, progress func(*helperv1.Progress)) error {
	if _, err := e.jr.Append(tx, stepName(s), by, note, data); err != nil {
		e.log.Error("journal write failed", "tx", tx, "state", stepName(s), "err", err)
		return helperapi.Errorf(helperv1.ErrorCode_ERROR_CODE_STUCK, "can't write the journal, so nothing more is changed: %v", err)
	}
	e.log.Info(stepName(s), "tx", tx, "by", by, "note", note)
	if progress != nil {
		progress(&helperv1.Progress{State: s, Detail: note})
	}
	return nil
}

func errorResponse(err error) *helperv1.Response {
	var he *helperapi.Error
	if !errors.As(err, &he) {
		he = &helperapi.Error{Code: helperv1.ErrorCode_ERROR_CODE_INTERNAL, Message: err.Error()}
	}
	return &helperv1.Response{Kind: &helperv1.Response_Error{Error: &helperv1.Error{Code: he.Code, Message: he.Message}}}
}

func denied(msg string) *helperv1.Response {
	return errorResponse(&helperapi.Error{Code: helperv1.ErrorCode_ERROR_CODE_DENIED, Message: msg})
}

// Handle answers one request from by. progress, which may be nil, gets every
// state change while the request runs. Who may ask is checked before the
// request's fields, so a caller without the right learns nothing more.
func (e *Engine) Handle(ctx context.Context, req *helperv1.Request, by Caller, progress func(*helperv1.Progress)) *helperv1.Response {
	if _, ok := req.GetKind().(*helperv1.Request_Apply); ok && by != Root {
		return denied("only root applies fixes; run sudo snugbox fit")
	}
	if err := helperapi.Validate(req, e.box); err != nil {
		return errorResponse(err)
	}
	switch k := req.GetKind().(type) {
	case *helperv1.Request_Apply:
		return e.apply(ctx, k.Apply, progress)
	case *helperv1.Request_Undo:
		return e.undo(ctx, k.Undo, by, progress)
	case *helperv1.Request_Advance:
		return e.advance(k.Advance, by)
	case *helperv1.Request_List:
		vs := e.views()
		out := &helperv1.ListResponse{}
		for _, v := range vs[:min(len(vs), maxListed)] {
			out.Transactions = append(out.Transactions, v.proto())
		}
		return &helperv1.Response{Kind: &helperv1.Response_List{List: out}}
	case *helperv1.Request_Containers:
		return &helperv1.Response{Kind: &helperv1.Response_Containers{Containers: e.containers(ctx)}}
	}
	return errorResponse(fmt.Errorf("unhandled request %T", req.GetKind()))
}

// newID names a transaction after the time and the fix kind, moving on a
// second when the name is taken.
func (e *Engine) newID(kind string) string {
	taken := map[string]bool{}
	for _, v := range e.views() {
		taken[v.id] = true
	}
	t := e.sys.Now().UTC()
	for {
		id := t.Format("20060102-150405") + "-" + kind
		if !taken[id] {
			return id
		}
		t = t.Add(time.Second)
	}
}

// busy names a transaction that blocks a new fix: one the helper is in the
// middle of, or one in its 30 minutes of verification, so two changes are
// never judged by the same window.
func (e *Engine) busy() error {
	for _, v := range e.views() {
		switch {
		case inFlight(v.state):
			return helperapi.Errorf(helperv1.ErrorCode_ERROR_CODE_BUSY, "transaction %s is still %s", v.id, stepName(v.state))
		case v.state == helperv1.State_STATE_VERIFYING:
			left := max(VerifyWindow-e.sys.Now().Sub(v.since), time.Minute)
			return helperapi.Errorf(helperv1.ErrorCode_ERROR_CODE_BUSY,
				"%s is being verified for about %d more minutes; one fix is verified at a time", v.id, int(left.Minutes()+0.5))
		}
	}
	return nil
}

func (e *Engine) apply(ctx context.Context, a *helperv1.ApplyRequest, progress func(*helperv1.Progress)) *helperv1.Response {
	if err := e.calm(true); err != nil {
		return errorResponse(err)
	}
	if err := e.busy(); err != nil {
		return errorResponse(err)
	}
	kind := helperapi.KindOf(a.GetFix())
	tx := e.newID(kind)
	if a.GetDryRun() {
		plan, err := e.preflight(ctx, tx, a.GetFix())
		if err != nil {
			return errorResponse(err)
		}
		return &helperv1.Response{Kind: &helperv1.Response_Apply{Apply: &helperv1.ApplyResponse{Changes: plan.Changes(), Notes: planNotes(plan)}}}
	}
	fixJSON, err := protojson.Marshal(a.GetFix())
	if err != nil {
		return errorResponse(err)
	}
	if err := e.step(tx, helperv1.State_STATE_APPROVED, Root.String(), "", approvedData{Fix: fixJSON, LowConfidence: a.GetAcceptLowConfidence()}, progress); err != nil {
		return errorResponse(err)
	}
	if err := e.step(tx, helperv1.State_STATE_PREFLIGHT, "helper", "checking every precondition again", nil, progress); err != nil {
		return errorResponse(err)
	}
	plan, err := e.preflight(ctx, tx, a.GetFix())
	if err != nil {
		if jerr := e.step(tx, helperv1.State_STATE_FAILED, "helper", "nothing was changed: "+err.Error(), nil, progress); jerr != nil {
			return errorResponse(jerr)
		}
		return errorResponse(err)
	}
	manifest, err := json.Marshal(plan)
	if err != nil {
		return errorResponse(err)
	}
	if err := e.sys.SaveSnapshot(tx, manifest); err != nil {
		if jerr := e.step(tx, helperv1.State_STATE_FAILED, "helper", "nothing was changed: the snapshot could not be saved: "+err.Error(), nil, progress); jerr != nil {
			return errorResponse(jerr)
		}
		return errorResponse(err)
	}
	sum := sha256.Sum256(manifest)
	snap := snapshotData{Manifest: hex.EncodeToString(sum[:]), Cgroups: plan.Cgroups, AutoUndo: plan.AutoUndo}
	for _, c := range plan.Changes() {
		snap.Changes = append(snap.Changes, change{Target: c.GetTarget(), Old: c.GetOldValue(), New: c.GetNewValue(), File: c.GetFile(), Manual: c.GetManual()})
	}
	if err := e.step(tx, helperv1.State_STATE_SNAPSHOT, "helper", "old values saved in "+UndoDir+"/"+tx, snap, progress); err != nil {
		return errorResponse(err)
	}
	if err := e.step(tx, helperv1.State_STATE_APPLYING, "helper", "", nil, progress); err != nil {
		return errorResponse(err)
	}
	if err := e.run(ctx, plan, progress); err != nil {
		return e.rollBack(ctx, tx, plan, err, progress)
	}
	if err := e.confirm(ctx, plan); err != nil {
		return e.rollBack(ctx, tx, plan, err, progress)
	}
	note := "applied; snugboxd watches it for 30 minutes"
	if err := e.step(tx, helperv1.State_STATE_VERIFYING, "helper", note, nil, progress); err != nil {
		return errorResponse(err)
	}
	return &helperv1.Response{Kind: &helperv1.Response_Apply{Apply: &helperv1.ApplyResponse{Transaction: tx, Changes: plan.Changes(), Notes: planNotes(plan)}}}
}

func planNotes(p *Plan) []string {
	notes := slices.Clone(p.Notes)
	for _, m := range p.Manual {
		notes = append(notes, fmt.Sprintf("%s: Snugbox didn't write it because %s.", m.Target, m.Why))
	}
	return notes
}

// pauseCheck is how often a running step looks for snugbox pause, so a pause
// stops a fix within seconds, inside the 5 that blueprint section 16.2
// allows.
const pauseCheck = time.Second

var errPaused = errors.New("snugbox pause came into force in the middle of the change")

// run does the plan's steps in order. It stops at the first failure, and as
// soon as snugbox pause is in force: before each step, after the last one,
// and while a step runs, whose tool is stopped.
func (e *Engine) run(ctx context.Context, p *Plan, progress func(*helperv1.Progress)) error {
	ctx, cancel := context.WithCancelCause(ctx)
	defer cancel(nil)
	go e.watchPause(ctx, cancel)
	for i := range p.Steps {
		if e.paused() {
			return errPaused
		}
		if ctx.Err() != nil {
			return context.Cause(ctx)
		}
		s := &p.Steps[i]
		if progress != nil && s.Op != OpReload {
			progress(&helperv1.Progress{State: helperv1.State_STATE_APPLYING, Detail: describe(s)})
		}
		if err := s.do(ctx, e.sys, e.exe); err != nil {
			if errors.Is(context.Cause(ctx), errPaused) {
				return fmt.Errorf("%w, while %s", errPaused, describe(s))
			}
			return fmt.Errorf("%s: %w", describe(s), err)
		}
	}
	if e.paused() {
		return errPaused
	}
	return nil
}

// watchPause ends ctx with errPaused once snugbox pause is in force.
func (e *Engine) watchPause(ctx context.Context, cancel context.CancelCauseFunc) {
	tick := time.NewTicker(pauseCheck)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
			if e.paused() {
				cancel(errPaused)
				return
			}
		}
	}
}

func describe(s *Step) string {
	name := s.Target
	if s.Label != "" {
		name = s.Label
	}
	switch s.Op {
	case OpFile, OpOverride:
		return "writing " + s.Target
	case OpKernel:
		return fmt.Sprintf("setting %s to %s", s.Target, s.New)
	case OpEnable:
		return "starting " + s.Target
	case OpRestart:
		return "restarting " + s.Target
	case OpDocker:
		return fmt.Sprintf("limiting container %s to %s", name, sizeText(bytesOf(s.NewMemory.Memory)))
	case OpSwapFile:
		return fmt.Sprintf("making a %s swap file at %s", sizeText(s.Size), s.Target)
	case OpLimit:
		return fmt.Sprintf("limiting the running %s to %s", s.Target, sizeText(s.Max))
	}
	return string(s.Op)
}

// rollBack puts back a change that failed while applying.
func (e *Engine) rollBack(ctx context.Context, tx string, p *Plan, cause error, progress func(*helperv1.Progress)) *helperv1.Response {
	// The rollback must finish even when the client has gone or asked to
	// stop.
	ctx = context.WithoutCancel(ctx)
	if err := e.step(tx, helperv1.State_STATE_UNDOING, "helper", "the change failed: "+cause.Error(), nil, progress); err != nil {
		return errorResponse(err)
	}
	notes, err := e.revert(ctx, p, true)
	if err != nil {
		msg := fmt.Sprintf("the change failed (%v) and putting it back failed too (%v); see %s/%s", cause, err, UndoDir, tx)
		if jerr := e.step(tx, helperv1.State_STATE_FAILED, "helper", msg, nil, progress); jerr != nil {
			return errorResponse(jerr)
		}
		return errorResponse(helperapi.Errorf(helperv1.ErrorCode_ERROR_CODE_STUCK, "%s", msg))
	}
	msg := "the change failed and was put back: " + cause.Error()
	if len(notes) > 0 {
		msg += " " + strings.Join(notes, " ")
	}
	if err := e.step(tx, helperv1.State_STATE_UNDONE, "helper", msg, nil, progress); err != nil {
		return errorResponse(err)
	}
	return errorResponse(helperapi.Errorf(helperv1.ErrorCode_ERROR_CODE_FAILED, "%s", msg))
}

// revert undoes every step in reverse order, carrying on past a failure so
// as much as possible is put back, then reloads systemd, puts each limited
// service's own limits back on its running cgroup, and restarts what the
// fix restarted, unless restart is false.
func (e *Engine) revert(ctx context.Context, p *Plan, restart bool) ([]string, error) {
	since := e.sys.Now()
	var notes []string
	var errs []error
	reload := false
	var moved []*Step
	var limited []string
	for i := len(p.Steps) - 1; i >= 0; i-- {
		s := &p.Steps[i]
		if s.Op == OpReload || (s.Op == OpFile && strings.HasPrefix(s.Target, UnitDir+"/")) {
			reload = true
		}
		if s.Calendar != "" {
			moved = append(moved, s)
		}
		// Every service limit drop-in, also in a snapshot saved before
		// OpLimit existed, whose undo must reach the running service.
		if s.Op == OpFile && s.Max > 0 && s.Label != "" {
			limited = append(limited, s.Label)
		}
		note, err := s.undo(ctx, e.sys, e.exe)
		if err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", describe(s), err))
		}
		if note != "" {
			notes = append(notes, note)
		}
	}
	if reload {
		if _, err := e.sys.Systemctl(ctx, "daemon-reload"); err != nil {
			errs = append(errs, err)
		}
	}
	for _, unit := range limited {
		own, err := ownLimits(ctx, e.sys, unit)
		if err == nil && own != nil {
			var note string
			note, err = setLive(ctx, e.sys, unit, own)
			if note != "" {
				notes = append(notes, note)
			}
		}
		if err != nil {
			errs = append(errs, fmt.Errorf("putting the limits of %s back on it while it runs: %w", unit, err))
		}
	}
	for _, unit := range p.Restart {
		if !restart {
			notes = append(notes, unit+" was not restarted, so it keeps the heap cap until its next restart.")
			continue
		}
		if _, err := e.sys.Systemctl(ctx, "restart", "--", unit); err != nil {
			errs = append(errs, err)
		}
	}
	for _, s := range moved {
		if note := e.rerunNote(ctx, s, since); note != "" {
			notes = append(notes, note)
		}
	}
	return notes, errors.Join(errs...)
}

// rerunNote says when systemd starts a job again because a revert put its
// timer back on its later, old calendar. systemd counts from the timer's
// last run, so a job that has run at its earlier time since the move runs
// once more at the old time that day, and at once when that has passed.
func (e *Engine) rerunNote(ctx context.Context, s *Step, since time.Time) string {
	props, err := e.unitProps(ctx, s.Label, "Unit", "LastTriggerUSec", "TimersCalendar")
	if err != nil {
		return ""
	}
	service := first(props, "Unit")
	_, next, _ := calendarTrigger(first(props, "TimersCalendar"))
	at, nerr := time.ParseInLocation(systemdUTC, next, time.UTC)
	last, lerr := time.ParseInLocation(systemdUTC, first(props, "LastTriggerUSec"), time.UTC)
	// systemctl prints whole seconds, so a run in the same second the
	// revert began would read as one before it.
	switch {
	case lerr == nil && !last.Before(since.Truncate(time.Second)), nerr == nil && !at.After(e.sys.Now()):
		return fmt.Sprintf("systemd started %s at once when the move was put back, because the old time of %s had passed since its last run.", service, s.Label)
	case lerr == nil && nerr == nil && at.Sub(last) <= time.Duration(s.Minutes)*time.Minute+dueSoonSlack:
		return fmt.Sprintf("%s starts %s again at %s, its old time, though it ran at %s: systemd counts from that run.", s.Label, service, clock(at), clock(last))
	}
	return ""
}

func (e *Engine) loadPlan(v *view) (*Plan, error) {
	raw, err := e.sys.LoadSnapshot(v.id)
	if err != nil {
		return nil, helperapi.Errorf(helperv1.ErrorCode_ERROR_CODE_STUCK, "the snapshot of %s can't be read: %v", v.id, err)
	}
	sum := sha256.Sum256(raw)
	if hex.EncodeToString(sum[:]) != v.manifest {
		return nil, helperapi.Errorf(helperv1.ErrorCode_ERROR_CODE_STUCK, "the snapshot of %s was changed after it was saved, so Snugbox won't restore from it", v.id)
	}
	var p Plan
	if err := json.Unmarshal(raw, &p); err != nil {
		return nil, helperapi.Errorf(helperv1.ErrorCode_ERROR_CODE_STUCK, "the snapshot of %s can't be read: %v", v.id, err)
	}
	return &p, nil
}

func (e *Engine) undo(ctx context.Context, u *helperv1.UndoRequest, by Caller, progress func(*helperv1.Progress)) *helperv1.Response {
	var v *view
	if u.GetTransaction() == "" {
		if by != Root {
			return denied("snugboxd must name the transaction it undoes")
		}
		for _, c := range e.views() {
			if applied(c.state) {
				v = c
				break
			}
		}
		if v == nil {
			return errorResponse(helperapi.Errorf(helperv1.ErrorCode_ERROR_CODE_NOT_FOUND, "no applied fix to undo"))
		}
	} else if v = e.find(u.GetTransaction()); v == nil {
		return errorResponse(helperapi.Errorf(helperv1.ErrorCode_ERROR_CODE_NOT_FOUND, "no transaction %s", u.GetTransaction()))
	}
	if !applied(v.state) {
		return errorResponse(helperapi.Errorf(helperv1.ErrorCode_ERROR_CODE_PRECONDITION, "%s is %s, so there is nothing to undo", v.id, stepName(v.state)))
	}
	if by == Watcher {
		switch {
		case v.state != helperv1.State_STATE_VERIFYING || !v.autoUndo:
			return denied("snugboxd undoes only a limit fix that fails its 30 minutes of verification")
		case u.GetReason() == "":
			return denied("an automatic undo needs a reason")
		case e.paused():
			return errorResponse(helperapi.Errorf(helperv1.ErrorCode_ERROR_CODE_PAUSED, "snugbox pause is in force"))
		}
	}
	plan, err := e.loadPlan(v)
	if err != nil {
		return errorResponse(err)
	}
	if plan.Kind == helperapi.KindSwap {
		if on, err := swapActive(e.sys, SwapFile); err != nil || on {
			if err == nil {
				err = e.swapoffSafe(SwapFile)
			}
			if err != nil {
				return errorResponse(helperapi.Errorf(helperv1.ErrorCode_ERROR_CODE_PRESSURE, "%v", err))
			}
		}
	}
	reason := u.GetReason()
	if reason == "" {
		reason = "asked for with snugbox undo"
	}
	if err := e.step(v.id, helperv1.State_STATE_UNDOING, by.String(), reason, nil, progress); err != nil {
		return errorResponse(err)
	}
	notes, err := e.revert(context.WithoutCancel(ctx), plan, true)
	if err != nil {
		msg := fmt.Sprintf("putting it back failed: %v; see %s/%s", err, UndoDir, v.id)
		if jerr := e.step(v.id, helperv1.State_STATE_FAILED, "helper", msg, nil, progress); jerr != nil {
			return errorResponse(jerr)
		}
		return errorResponse(helperapi.Errorf(helperv1.ErrorCode_ERROR_CODE_STUCK, "%s", msg))
	}
	if err := e.step(v.id, helperv1.State_STATE_UNDONE, "helper", strings.Join(notes, " "), nil, progress); err != nil {
		return errorResponse(err)
	}
	return &helperv1.Response{Kind: &helperv1.Response_Undo{Undo: &helperv1.UndoResponse{Transaction: v.id, Changes: plan.Changes(), Notes: notes}}}
}

func (e *Engine) advance(a *helperv1.AdvanceRequest, by Caller) *helperv1.Response {
	v := e.find(a.GetTransaction())
	if v == nil {
		return errorResponse(helperapi.Errorf(helperv1.ErrorCode_ERROR_CODE_NOT_FOUND, "no transaction %s", a.GetTransaction()))
	}
	held := e.sys.Now().Sub(v.since)
	from, to := v.state, a.GetState()
	ok := false
	switch {
	case from == helperv1.State_STATE_VERIFYING && to == helperv1.State_STATE_WATCHING:
		if held < VerifyWindow {
			return errorResponse(helperapi.Errorf(helperv1.ErrorCode_ERROR_CODE_PRECONDITION, "%s has been verified for %s of its 30 minutes", v.id, held.Round(time.Second)))
		}
		ok = true
	case from == helperv1.State_STATE_WATCHING && to == helperv1.State_STATE_DONE:
		if held < WatchWindow {
			return errorResponse(helperapi.Errorf(helperv1.ErrorCode_ERROR_CODE_PRECONDITION, "%s has been watched for %s of its 7 days", v.id, held.Round(time.Minute)))
		}
		ok = true
	case (from == helperv1.State_STATE_VERIFYING || from == helperv1.State_STATE_WATCHING) && to == helperv1.State_STATE_REOPENED:
		ok = true
	case from == helperv1.State_STATE_REOPENED && to == helperv1.State_STATE_DONE:
		ok = by == Root
	}
	if !ok {
		return errorResponse(helperapi.Errorf(helperv1.ErrorCode_ERROR_CODE_PRECONDITION, "%s can't go from %s to %s", v.id, stepName(from), stepName(to)))
	}
	note := a.GetReason()
	if to == helperv1.State_STATE_DONE && from == helperv1.State_STATE_REOPENED {
		note = strings.TrimSpace("a person reviewed it and kept it. " + note)
	}
	if err := e.step(v.id, to, by.String(), note, nil, nil); err != nil {
		return errorResponse(err)
	}
	return &helperv1.Response{Kind: &helperv1.Response_Advance{Advance: &helperv1.AdvanceResponse{Transaction: e.find(v.id).proto()}}}
}

// Recover finishes what a stopped helper left: every transaction cut short
// in the middle of a step is put back, as blueprint section 5.6 asks for a
// reboot in the middle of a fix. Services are not restarted while
// recovering; the notes say which ones keep a change until their next
// restart. It stops early, returning the error, if the journal can't be
// written.
func (e *Engine) Recover(ctx context.Context) error {
	for _, v := range e.views() {
		if !inFlight(v.state) {
			continue
		}
		e.log.Warn("finishing a transaction the helper stopped in the middle of", "tx", v.id, "state", stepName(v.state))
		state, note := helperv1.State_STATE_UNDONE, "the helper stopped before changing anything"
		if v.manifest != "" {
			plan, err := e.loadPlan(v)
			var notes []string
			if err == nil {
				notes, err = e.revert(ctx, plan, false)
			}
			if err != nil {
				state, note = helperv1.State_STATE_FAILED, "the helper stopped in the middle of the change and could not put it back: "+err.Error()
			} else {
				note = strings.Join(append([]string{"the helper stopped in the middle of the change; what it had changed was put back."}, notes...), " ")
			}
		}
		if err := e.step(v.id, state, "helper", note, nil, nil); err != nil {
			return err
		}
	}
	return nil
}

func (e *Engine) containers(ctx context.Context) *helperv1.ContainersResponse {
	d := e.sys.Docker()
	if d == nil {
		return &helperv1.ContainersResponse{}
	}
	list, err := d.List(ctx)
	if err != nil {
		e.log.Warn("docker list failed", "err", err)
		return &helperv1.ContainersResponse{}
	}
	out := &helperv1.ContainersResponse{Docker: true}
	for _, s := range list[:min(len(list), maxListedCts)] {
		c, err := d.Inspect(ctx, s.ID)
		if err != nil {
			continue
		}
		l := c.Config.Labels
		restarts := int32(math.MaxInt32)
		if c.RestartCount >= 0 && c.RestartCount < math.MaxInt32 {
			restarts = int32(c.RestartCount)
		}
		memory := c.HostConfig.Memory
		if !docker.Limited(memory) {
			memory = 0
		}
		out.Containers = append(out.Containers, &helperv1.Container{
			Id: c.ID, Name: strings.TrimPrefix(c.Name, "/"), Image: c.Config.Image,
			ComposeProject: l["com.docker.compose.project"], ComposeService: l["com.docker.compose.service"],
			ComposeWorkingDir: l["com.docker.compose.project.working_dir"],
			MemoryBytes:       memory, MemoryReservationBytes: c.HostConfig.MemoryReservation,
			RestartCount: restarts, Running: c.State.Running, OomKilled: c.State.OOMKilled,
			Started: timestamppb.New(c.State.StartedAt), Restarting: c.State.Status == "restarting",
		})
	}
	return out
}
