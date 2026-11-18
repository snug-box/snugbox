// Package localapi is the contract between snugboxd and the snugbox command:
// the JSON it serves on its Unix socket, and a client for it. It is separate
// from the watcher so the command line doesn't carry the watcher's database
// code.
package localapi

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"time"

	"snugbox.dev/snugbox/agent/internal/audit"
	"snugbox.dev/snugbox/agent/internal/model"
)

// Paths the watcher and the command line share.
const (
	// DefaultSocket is where snugboxd listens.
	DefaultSocket = "/run/snugbox/snugboxd.sock"
	// DefaultDB is the watcher's history.
	DefaultDB = "/var/lib/snugbox/snugbox.db"
	// DefaultPause is the file whose presence means snugbox pause is in
	// force. It sits in a folder only root can write, so a compromised
	// watcher can't lift a pause.
	DefaultPause = "/etc/snugbox/paused"
)

// Status is GET /v1/status.
type Status struct {
	Version    string        `json:"version"`
	Started    time.Time     `json:"started"`
	Paused     bool          `json:"paused"`
	Interval   time.Duration `json:"interval"`
	LastSample time.Time     `json:"last_sample"`
	Workloads  int           `json:"workloads"`
	StoreBytes int64         `json:"store_bytes"`
	WatchStart time.Time     `json:"watch_start"`
	Analysed   bool          `json:"analysed"`
}

// LivePoint is one 10-second box point.
type LivePoint struct {
	Time      time.Time `json:"time"`
	Hard      uint64    `json:"hard"`
	Total     uint64    `json:"total"`
	Hot       uint64    `json:"hot"`
	MemTotal  uint64    `json:"mem_total"`
	MemAvail  uint64    `json:"mem_available"`
	SwapUsed  uint64    `json:"swap_used"`
	PSISomeUS int64     `json:"psi_some_us"`
	PSIFullUS int64     `json:"psi_full_us"`
}

// LiveWorkload is one workload's latest point.
type LiveWorkload struct {
	ID         string     `json:"id"`
	Name       string     `json:"name"`
	Kind       model.Kind `json:"kind"`
	Programs   []string   `json:"programs"`
	WorkingSet uint64     `json:"working_set"`
	Hard       uint64     `json:"hard"`
	Hot        uint64     `json:"hot"`
	Current    uint64     `json:"current"`
	Limit      uint64     `json:"limit"`
	// PressureSome is the workload's cumulative stall time; two readings make
	// a rate.
	PressureSome time.Duration `json:"pressure_some"`
}

// Live is GET /v1/live: the last 15 minutes of the box and every workload's
// latest point.
type Live struct {
	Time      time.Time      `json:"time"`
	Box       []LivePoint    `json:"box"`
	Workloads []LiveWorkload `json:"workloads"`
}

// Event is one entry of GET /v1/events.
type Event struct {
	Time     time.Time `json:"time"`
	Kind     string    `json:"kind"`
	Workload string    `json:"workload"`
	Detail   string    `json:"detail"`
}

// Change is one fix transaction as the watcher sees it.
type Change struct {
	ID string `json:"id"`
	// Kind is the fix kind, such as service-limits, and State the journal's
	// word for where it is, such as verifying.
	Kind    string    `json:"kind"`
	State   string    `json:"state"`
	Started time.Time `json:"started"`
	Since   time.Time `json:"since"`
	// Reason says why a fix was undone, reopened or failed.
	Reason  string   `json:"reason,omitempty"`
	Cgroups []string `json:"cgroups,omitempty"`
	// VerifyEnds is when the 30 minutes of verification end.
	VerifyEnds time.Time `json:"verify_ends,omitzero"`
	// Counted is true once a full day, nightly jobs included, has passed
	// under the fix without trouble; only then does it count as done.
	Counted bool `json:"counted"`
	// Trouble is a failure the watcher saw but could not act on yet, such as
	// while snugbox pause is in force.
	Trouble string `json:"trouble,omitempty"`
	// Changes lists what the fix changed.
	Changes []ChangeItem `json:"changes,omitempty"`
}

// ChangeItem is one thing a fix changed.
type ChangeItem struct {
	Target string `json:"target"`
	Old    string `json:"old,omitempty"`
	New    string `json:"new,omitempty"`
	File   bool   `json:"file,omitempty"`
	Manual bool   `json:"manual,omitempty"`
}

// Changes is GET /v1/changes: every fix the helper's journal holds, newest
// first.
type Changes struct {
	// Helper is false when the watcher could not reach snugbox-helper, and
	// Error says why.
	Helper bool      `json:"helper"`
	Error  string    `json:"error,omitempty"`
	Read   time.Time `json:"read"`
	// Pending is true when the watcher was nudged after it last read the
	// helper, so a fix applied a moment ago may be missing; it reads the
	// helper again at its next sample.
	Pending bool     `json:"pending,omitempty"`
	Changes []Change `json:"changes"`
}

// ErrNotRunning means no watcher answers on the socket.
var ErrNotRunning = errors.New("snugboxd is not running; start it with sudo snugbox watch install")

// Snapshot is one answer of the memory model the watcher stored: what the
// box needed from a time on.
type Snapshot struct {
	Time       time.Time     `json:"time"`
	Need       uint64        `json:"need_bytes"`
	Headroom   uint64        `json:"headroom_bytes"`
	Cache      uint64        `json:"cache_bytes"`
	Snug       uint64        `json:"snug_bytes"`
	Confidence string        `json:"confidence"`
	Window     time.Duration `json:"window"`
}

// Resize is a change of the box's RAM or CPU count the watcher saw, with
// the catalogue plans that match the box before and after it and their
// prices a month in cents; a plan is "" when it is unknown.
type Resize struct {
	Time     time.Time `json:"time"`
	OldRAM   uint64    `json:"old_ram_bytes"`
	NewRAM   uint64    `json:"new_ram_bytes"`
	OldCPUs  int       `json:"old_cpus"`
	NewCPUs  int       `json:"new_cpus"`
	Provider string    `json:"provider,omitempty"`
	OldPlan  string    `json:"old_plan,omitempty"`
	NewPlan  string    `json:"new_plan,omitempty"`
	OldCents int64     `json:"old_cents,omitempty"`
	NewCents int64     `json:"new_cents,omitempty"`
}

// Report is GET /v1/report: what changed on the box from a time until now,
// for snugbox report.
type Report struct {
	From time.Time `json:"from"`
	To   time.Time `json:"to"`
	// WatchStart is when the watcher's history begins, which may be after
	// From.
	WatchStart time.Time `json:"watch_start"`
	// Then is the estimate that stood at From, and Now the latest; nil
	// before the first analysis.
	Then *Snapshot `json:"then,omitempty"`
	Now  *Snapshot `json:"now,omitempty"`
	// Resizes are the period's changes of RAM or CPUs, oldest first.
	Resizes []Resize `json:"resizes"`
	// Events are the period's events, newest first, except memory.high
	// hits, which Throttled counts by workload.
	Events    []Event        `json:"events"`
	Throttled map[string]int `json:"throttled,omitempty"`
	// Changes are the fixes in the helper's journal, as GET /v1/changes
	// gives them.
	Changes Changes `json:"changes"`
}

// Client talks to a running watcher.
type Client struct {
	hc *http.Client
}

// NewClient returns a client for the watcher on socket.
func NewClient(socket string) *Client {
	tr := &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
		var dl net.Dialer
		return dl.DialContext(ctx, "unix", socket)
	}}
	return &Client{hc: &http.Client{Transport: tr, Timeout: 10 * time.Second}}
}

func (c *Client) get(ctx context.Context, path string, out any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://snugboxd"+path, nil)
	if err != nil {
		return err
	}
	resp, err := c.hc.Do(req)
	if err != nil {
		if notListening(err) {
			return ErrNotRunning
		}
		if errors.Is(err, os.ErrPermission) {
			return errors.New("can't open the watcher's socket, which needs root or the snugbox group; run with sudo or join the group")
		}
		return fmt.Errorf("snugboxd: %w", err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 64<<20))
	if err != nil {
		return fmt.Errorf("snugboxd: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		var e struct{ Error string }
		if json.Unmarshal(body, &e) == nil && e.Error != "" {
			return errors.New(e.Error)
		}
		return fmt.Errorf("snugboxd answered %d", resp.StatusCode)
	}
	return json.Unmarshal(body, out)
}

// notListening reports whether a dial failed because nothing listens at the
// socket's path.
func notListening(err error) bool {
	if errors.Is(err, os.ErrNotExist) {
		return true
	}
	for _, code := range refused {
		if errors.Is(err, code) {
			return true
		}
	}
	return false
}

// Status asks the watcher how it is.
func (c *Client) Status(ctx context.Context) (Status, error) {
	var s Status
	err := c.get(ctx, "/v1/status", &s)
	return s, err
}

// Result fetches the watcher's latest analysis.
func (c *Client) Result(ctx context.Context) (*audit.Result, error) {
	var r audit.Result
	if err := c.get(ctx, "/v1/result", &r); err != nil {
		return nil, err
	}
	if err := r.Reattach(); err != nil {
		return nil, err
	}
	return &r, nil
}

// Live fetches the last 15 minutes.
func (c *Client) Live(ctx context.Context) (Live, error) {
	var l Live
	err := c.get(ctx, "/v1/live", &l)
	return l, err
}

// Events fetches events since a time.
func (c *Client) Events(ctx context.Context, since time.Time) ([]Event, error) {
	var evs []Event
	err := c.get(ctx, "/v1/events?since="+since.UTC().Format(time.RFC3339), &evs)
	return evs, err
}

// Changes fetches the fixes the watcher is verifying and watching.
func (c *Client) Changes(ctx context.Context) (Changes, error) {
	var ch Changes
	err := c.get(ctx, "/v1/changes", &ch)
	return ch, err
}

// Report fetches what changed on the box since a time.
func (c *Client) Report(ctx context.Context, since time.Time) (Report, error) {
	var r Report
	err := c.get(ctx, "/v1/report?since="+since.UTC().Format(time.RFC3339), &r)
	return r, err
}

// Nudge asks the watcher to read the helper's transactions at its next
// sample, so a fix applied a moment ago is judged from its first seconds.
func (c *Client) Nudge(ctx context.Context) error {
	var ch Changes
	return c.get(ctx, "/v1/changes?refresh=1", &ch)
}
