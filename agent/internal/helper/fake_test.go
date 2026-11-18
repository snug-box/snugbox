package helper

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"maps"
	"os"
	"path"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"snugbox.dev/snugbox/agent/internal/docker"
	"snugbox.dev/snugbox/agent/internal/helperapi"
	"snugbox.dev/snugbox/agent/internal/journal"
	helperv1 "snugbox.dev/snugbox/gen/snugbox/helper/v1"
)

// fakeBox is a box kept in memory: files, kernel files, systemd units,
// swap areas and Docker. It answers systemctl the way systemd 255 does for
// the calls the helper makes.
type fakeBox struct {
	t     *testing.T
	now   time.Time
	files map[string]string
	// dirs holds the folders made by writing a file into them, or by a
	// test; a folder with a file or folder in it exists too.
	dirs    map[string]bool
	owners  map[string][2]int
	units   map[string]map[string]string
	swaps   []string
	swapKiB uint64
	// swapUnits are swap unit files systemctl list-unit-files shows, such
	// as the dev-zram0.swap Fedora's zram-generator makes.
	swapUnits []string
	calls     []string
	free      uint64
	fstype    string
	virt      string
	// mkswapfile says btrfs-progs is new enough to have filesystem
	// mkswapfile.
	mkswapfile bool
	docker     *fakeDocker
	kills      []time.Time
	snaps      map[string][]byte
	onCall     func(call string)
	// onRemove runs before a file is removed, so a test can cut the power
	// between two steps of one call.
	onRemove func(path string)
	noApply  bool
	// reloadKeepsCgroups makes daemon-reload load memory settings without
	// putting them on a running unit's cgroup, as systemd 257 does; only
	// set-property and a start reach the cgroup then.
	reloadKeepsCgroups bool
	// failOnce makes the next call matching a key fail with its error.
	failOnce map[string]error
	// calendars holds each timer's calendar triggers from its own unit
	// file, which a schedule drop-in replaces at the next reload, and nexts
	// when each timer fires next, as systemd prints it.
	calendars map[string][]string
	nexts     map[string]string
	// paused is snugbox pause's flag, kept apart from files because the
	// engine looks for it from a second goroutine while a step runs.
	paused atomic.Bool
	// block makes the next call that starts with it wait until its context
	// ends, as a restart of a slow service would.
	block string
}

// pauseFlag is where the harness's engine looks for snugbox pause.
const pauseFlag = "/etc/snugbox/paused"

const testMem = 2 * helperapi.GiB

func newBox(t *testing.T) *fakeBox {
	b := &fakeBox{
		t:         t,
		now:       time.Date(2026, 10, 31, 10, 0, 0, 0, time.UTC),
		files:     map[string]string{},
		dirs:      map[string]bool{},
		owners:    map[string][2]int{},
		units:     map[string]map[string]string{},
		free:      20 * helperapi.GiB,
		fstype:    "ext4",
		virt:      "none",
		snaps:     map[string][]byte{},
		calendars: map[string][]string{},
		nexts:     map[string]string{},
	}
	b.files["/proc/pressure/memory"] = "some avg10=0.00 avg60=0.00 avg300=0.00 total=100\nfull avg10=0.00 avg60=0.00 avg300=0.00 total=50\n"
	b.files["/proc/meminfo"] = fmt.Sprintf("MemTotal: %d kB\nMemAvailable: %d kB\n", testMem/1024, testMem/2/1024)
	b.files[CgroupFS+"/cgroup.controllers"] = "cpu memory pids\n"
	for _, n := range []string{"enabled", "compressor", "shrinker_enabled", "max_pool_percent"} {
		b.files[path.Join(ZswapDir, n)] = map[string]string{"enabled": "N", "compressor": "lzo", "shrinker_enabled": "N", "max_pool_percent": "20"}[n] + "\n"
	}
	b.unit("system.slice", "loaded", "active", map[string]string{"ManagedOOMMemoryPressure": "auto"})
	b.unit("user.slice", "loaded", "active", map[string]string{"ManagedOOMMemoryPressure": "auto"})
	b.unit("ssh.service", "loaded", "active", nil)
	b.unit("systemd-oomd.service", "loaded", "inactive", map[string]string{"UnitFileState": "disabled"})
	// The helper's unit makes this folder before the helper starts.
	b.dirs[ControlDir] = true
	return b
}

// unit adds a unit with a cgroup and the memory files systemd would make.
// Its MemoryHigh and MemoryMax are its own configuration, which a reload
// starts from before drop-ins.
func (b *fakeBox) unit(name, load, active string, props map[string]string) {
	p := map[string]string{"LoadState": load, "ActiveState": active, "MemoryMax": "infinity", "MemoryHigh": "infinity",
		"ControlGroup": "/system.slice/" + name, "UnitFileState": "enabled"}
	maps.Copy(p, props)
	p["own.MemoryHigh"], p["own.MemoryMax"] = p["MemoryHigh"], p["MemoryMax"]
	b.units[name] = p
	cg := path.Join(CgroupFS, "system.slice", name)
	b.files[cg+"/memory.max"], b.files[cg+"/memory.high"] = cgroupValue(p["MemoryMax"])+"\n", cgroupValue(p["MemoryHigh"])+"\n"
	if _, ok := b.files[cg+"/memory.stat"]; !ok {
		b.files[cg+"/memory.stat"] = "anon 104857600\nfile 52428800\nshmem 0\nkernel_stack 1048576\n"
	}
}

func (b *fakeBox) Now() time.Time { return b.now }

// defaultNext is when a test timer fires next unless the test says
// otherwise, well after the fake's clock, which reads 10:00 on 31 October.
const defaultNext = "Sun 2026-11-01 02:00:00 UTC"

// timer adds a running timer that starts service on the calendars given, in
// systemd's normalised form such as "*-*-* 02:00:00". It fires next at
// defaultNext, and hasn't fired since it started.
func (b *fakeBox) timer(name, service string, calendars ...string) {
	b.units[name] = map[string]string{"LoadState": "loaded", "ActiveState": "active", "Unit": service, "UnitFileState": "enabled", "LastTriggerUSec": ""}
	b.calendars[name] = calendars
	b.nexts[name] = defaultNext
	b.units[name]["TimersCalendar"] = calendarProp(calendars, defaultNext)
}

// calendarProp is TimersCalendar as systemctl show prints it, one trigger
// to a line.
func calendarProp(calendars []string, next string) string {
	var lines []string
	for _, c := range calendars {
		lines = append(lines, "{ OnCalendar="+c+" ; next_elapse="+next+" }")
	}
	return strings.Join(lines, "\n")
}

func (b *fakeBox) ReadFile(p string) ([]byte, error) {
	if p == pauseFlag {
		if b.paused.Load() {
			return []byte("paused\n"), nil
		}
		return nil, fmt.Errorf("open %s: %w", p, fs.ErrNotExist)
	}
	if p == "/proc/swaps" {
		out := "Filename\t\t\t\tType\t\tSize\t\tUsed\t\tPriority\n"
		for _, s := range b.swaps {
			out += fmt.Sprintf("%s\tfile\t\t1048576\t\t%d\t\t-2\n", s, b.swapKiB)
		}
		return []byte(out), nil
	}
	s, ok := b.files[p]
	if !ok {
		return nil, fmt.Errorf("open %s: %w", p, fs.ErrNotExist)
	}
	return []byte(s), nil
}

func (b *fakeBox) WriteFile(p string, data []byte, _ fs.FileMode) error {
	for d := path.Dir(p); d != "/"; d = path.Dir(d) {
		b.dirs[d] = true
	}
	b.files[p] = string(data)
	return nil
}

func (b *fakeBox) RemoveFile(p string) error {
	if b.onRemove != nil {
		b.onRemove(p)
	}
	delete(b.files, p)
	return nil
}

// within lists the names directly inside folder d.
func (b *fakeBox) within(d string) []string {
	var names []string
	add := func(p string) {
		if rest, ok := strings.CutPrefix(p, d+"/"); ok {
			name, _, _ := strings.Cut(rest, "/")
			if !slices.Contains(names, name) {
				names = append(names, name)
			}
		}
	}
	for p := range b.files {
		add(p)
	}
	for p := range b.dirs {
		add(p)
	}
	slices.Sort(names)
	return names
}

func (b *fakeBox) ListDir(d string) ([]string, error) {
	names := b.within(d)
	if !b.dirs[d] && len(names) == 0 {
		return nil, fmt.Errorf("open %s: %w", d, fs.ErrNotExist)
	}
	return names, nil
}

func (b *fakeBox) RemoveDir(d string) (bool, error) {
	if !b.dirs[d] || len(b.within(d)) > 0 {
		return false, nil
	}
	delete(b.dirs, d)
	b.record("rmdir " + d)
	return true, nil
}

func (b *fakeBox) WriteKernel(p, v string) error {
	if _, ok := b.files[p]; !ok {
		return fmt.Errorf("%s: %w", p, fs.ErrNotExist)
	}
	b.files[p] = v + "\n"
	return nil
}

func (b *fakeBox) record(call string) {
	b.calls = append(b.calls, call)
	if b.onCall != nil {
		b.onCall(call)
	}
}

// wait holds a call that matches block until its context ends, and returns
// what ended it.
func (b *fakeBox) wait(ctx context.Context, call string) error {
	if b.block == "" || !strings.HasPrefix(call, b.block) {
		return nil
	}
	b.block = ""
	select {
	case <-ctx.Done():
		return context.Cause(ctx)
	case <-time.After(20 * time.Second):
		b.t.Errorf("nothing stopped %q", call)
		return nil
	}
}

func (b *fakeBox) prop(unit, key string) string {
	if p, ok := b.units[unit]; ok {
		return p[key]
	}
	return map[string]string{"LoadState": "not-found", "ActiveState": "inactive"}[key]
}

func (b *fakeBox) Systemctl(ctx context.Context, args ...string) (string, error) {
	call := "systemctl " + strings.Join(args, " ")
	b.record(call)
	if err := b.wait(ctx, call); err != nil {
		return "", err
	}
	if err, ok := b.failOnce[call]; ok {
		delete(b.failOnce, call)
		return "", err
	}
	switch args[0] {
	case "show":
		var props []string
		for _, a := range args {
			if p, ok := strings.CutPrefix(a, "--property="); ok {
				props = strings.Split(p, ",")
			}
		}
		unit := args[len(args)-1]
		var out strings.Builder
		for _, p := range props {
			v := b.prop(unit, p)
			if v == "" && strings.HasPrefix(p, "Timers") {
				// systemd 255 leaves out a timer's empty list of triggers.
				continue
			}
			for line := range strings.SplitSeq(v, "\n") {
				fmt.Fprintf(&out, "%s=%s\n", p, line)
			}
		}
		return out.String(), nil
	case "list-unit-files":
		if len(b.swapUnits) == 0 {
			return "", &ToolError{Name: "systemctl", Code: 1}
		}
		var out strings.Builder
		for _, u := range b.swapUnits {
			fmt.Fprintf(&out, "%s generated -\n", u)
		}
		return out.String(), nil
	case "daemon-reload":
		b.reload()
		return "", nil
	case "set-property":
		b.setProperty(args[1:])
		return "", nil
	case "enable", "disable", "stop", "restart":
		unit := args[len(args)-1]
		b.change(args[0], unit)
		return "", nil
	}
	return "", fmt.Errorf("fake systemctl: no %q", args[0])
}

// reload applies drop-ins the way daemon-reload does. Memory settings come
// from the unit's own configuration, then the runtime files set-property
// writes, then Snugbox's drop-in, which sorts after them; they reach the
// cgroup at once unless reloadKeepsCgroups, and a removed drop-in leaves
// the unit's own value.
func (b *fakeBox) reload() {
	if _, ok := b.files[path.Join(UnitDir, SwapUnit)]; ok {
		if b.units[SwapUnit] == nil {
			b.units[SwapUnit] = map[string]string{"LoadState": "loaded", "ActiveState": "inactive", "UnitFileState": "disabled"}
		}
	} else {
		delete(b.units, SwapUnit)
	}
	if b.noApply {
		return
	}
	for name, cals := range b.calendars {
		cur := slices.Clone(cals)
		if d, ok := b.files[path.Join(UnitDir, name+".d", helperapi.ScheduleDropIn)]; ok {
			for line := range strings.Lines(d) {
				// An empty OnCalendar= drops the triggers before it; any
				// other adds one.
				if v, ok := strings.CutPrefix(strings.TrimSpace(line), "OnCalendar="); ok && v == "" {
					cur = nil
				} else if ok {
					cur = append(cur, v)
				}
			}
		}
		b.units[name]["TimersCalendar"] = calendarProp(cur, b.nexts[name])
	}
	for name, p := range b.units {
		if strings.HasSuffix(name, ".timer") || name == SwapUnit {
			continue
		}
		vals := map[string]string{"MemoryHigh": "infinity", "MemoryMax": "infinity"}
		for _, m := range memorySettings {
			if v := p["own."+m.name]; v != "" {
				vals[m.name] = v
			}
			if v := runtimeValue(b.files[runtimeFile(name, m.name)], m.name); v != "" {
				vals[m.name] = v
			}
		}
		if d, ok := b.files[path.Join(UnitDir, name+".d", "50-snugbox-memory.conf")]; ok {
			for _, m := range memorySettings {
				if v := runtimeValue(d, m.name); v != "" {
					vals[m.name] = v
				}
			}
		}
		p["MemoryHigh"], p["MemoryMax"] = vals["MemoryHigh"], vals["MemoryMax"]
		if !b.reloadKeepsCgroups {
			b.toCgroup(p, vals)
		}
	}
}

// toCgroup writes memory settings to a unit's cgroup files, as systemd
// does when it applies them.
func (b *fakeBox) toCgroup(p map[string]string, vals map[string]string) {
	cg := path.Join(CgroupFS, strings.TrimPrefix(p["ControlGroup"], "/"))
	for _, m := range memorySettings {
		if v, ok := vals[m.name]; ok {
			if _, exists := b.files[path.Join(cg, m.file)]; exists {
				b.files[path.Join(cg, m.file)] = cgroupValue(v) + "\n"
			}
		}
	}
}

// setProperty answers systemctl set-property --runtime -- unit Name=value...:
// systemd writes one runtime file per setting, changes the loaded setting,
// and puts it on the running cgroup.
func (b *fakeBox) setProperty(args []string) {
	i := slices.Index(args, "--")
	if i < 0 || !slices.Contains(args[:i], "--runtime") || len(args) < i+3 {
		b.t.Errorf("set-property %q: the helper must set only --runtime properties of one unit", args)
		return
	}
	unit := args[i+1]
	p := b.units[unit]
	if p == nil {
		b.t.Errorf("set-property on unknown unit %s", unit)
		return
	}
	vals := map[string]string{}
	for _, a := range args[i+2:] {
		k, v, _ := strings.Cut(a, "=")
		f := runtimeFile(unit, k)
		for d := path.Dir(f); d != "/"; d = path.Dir(d) {
			b.dirs[d] = true
		}
		b.files[f] = "# This is a drop-in unit file extension, created via \"systemctl set-property\"\n" +
			"# or an equivalent operation. Do not edit.\n[Service]\n" + k + "=" + v + "\n"
		p[k] = v
		vals[k] = v
	}
	if !b.noApply {
		b.toCgroup(p, vals)
	}
}

func (b *fakeBox) change(verb, unit string) {
	p := b.units[unit]
	if p == nil {
		b.t.Errorf("systemctl %s on unknown unit %s", verb, unit)
		return
	}
	switch verb {
	case "enable":
		p["UnitFileState"], p["ActiveState"] = "enabled", "active"
		if unit == SwapUnit && !slices.Contains(b.swaps, SwapFile) {
			b.swaps = append(b.swaps, SwapFile)
		}
	case "disable":
		p["UnitFileState"] = "disabled"
	case "stop":
		p["ActiveState"] = "inactive"
		if unit == SwapUnit {
			b.swaps = slices.DeleteFunc(b.swaps, func(s string) bool { return s == SwapFile })
		}
	case "restart":
		p["ActiveState"] = "active"
		// A start puts the loaded settings on the new cgroup.
		b.toCgroup(p, map[string]string{"MemoryHigh": p["MemoryHigh"], "MemoryMax": p["MemoryMax"]})
	}
}

func (b *fakeBox) Tool(ctx context.Context, name string, stdin []byte, args ...string) (string, error) {
	call := name + " " + strings.Join(args, " ")
	b.record(call)
	if err := b.wait(ctx, call); err != nil {
		return "", err
	}
	switch name {
	case "systemd-detect-virt":
		if b.virt == "none" {
			return "none\n", errors.New("exit status 1")
		}
		return b.virt + "\n", nil
	case "mkswap":
		return "", nil
	case "btrfs":
		if len(args) < 2 || args[0] != "filesystem" || args[1] != "mkswapfile" {
			return "", fmt.Errorf("the fake knows only btrfs filesystem mkswapfile, not %q", args)
		}
		if !b.mkswapfile {
			return "", &ToolError{Name: "btrfs", Code: 1, Stderr: "btrfs filesystem: unknown token 'mkswapfile'"}
		}
		if slices.Contains(args, "--help") {
			return "usage: btrfs filesystem mkswapfile [options] <file>\n", nil
		}
		target := args[len(args)-1]
		if _, ok := b.files[target]; ok {
			return "", errors.New("file exists")
		}
		b.files[target] = ""
		return "", nil
	case "systemd-run":
		i := slices.Index(args, "--")
		verb, dir, file := args[i+2], args[i+3], args[i+4]
		target := path.Join(dir, file)
		switch verb {
		case "write-override":
			if _, ok := b.files[target]; ok {
				return "", errors.New("file exists")
			}
			b.files[target] = string(stdin)
			return "", nil
		case "remove-override":
			cur, ok := b.files[target]
			if !ok {
				return "", nil
			}
			if contentHash(cur) != args[i+5] {
				return target + " was changed after Snugbox wrote it, so it was left as it is.\n", nil
			}
			delete(b.files, target)
			return "", nil
		}
	}
	return "", fmt.Errorf("fake tool %s", name)
}

func (b *fakeBox) CreateSwapFile(p string, size uint64) error {
	if _, ok := b.files[p]; ok {
		return fmt.Errorf("%s: file exists", p)
	}
	b.files[p] = "swap of " + strconv.FormatUint(size, 10)
	return nil
}

func (b *fakeBox) Disk(string) (uint64, string, error) { return b.free, b.fstype, nil }

func (b *fakeBox) Owner(dir string) (int, int, error) {
	o, ok := b.owners[dir]
	if !ok {
		return 0, 0, fmt.Errorf("%s: %w", dir, fs.ErrNotExist)
	}
	return o[0], o[1], nil
}

func (b *fakeBox) SaveSnapshot(tx string, data []byte) error {
	if _, ok := b.snaps[tx]; ok {
		return errors.New("snapshot exists")
	}
	b.snaps[tx] = slices.Clone(data)
	return nil
}

func (b *fakeBox) LoadSnapshot(tx string) ([]byte, error) {
	s, ok := b.snaps[tx]
	if !ok {
		return nil, fs.ErrNotExist
	}
	return s, nil
}

func (b *fakeBox) Docker() Docker {
	if b.docker == nil {
		return nil
	}
	return b.docker
}

func (b *fakeBox) Kills() ([]time.Time, error) { return b.kills, nil }

type fakeDocker struct {
	cs      map[string]*docker.Container
	updates []docker.MemoryUpdate
}

func (d *fakeDocker) List(context.Context) ([]docker.Summary, error) {
	var out []docker.Summary
	for _, id := range slices.Sorted(maps.Keys(d.cs)) {
		out = append(out, docker.Summary{ID: id, Names: []string{d.cs[id].Name}})
	}
	return out, nil
}

func (d *fakeDocker) Inspect(_ context.Context, id string) (docker.Container, error) {
	c, ok := d.cs[id]
	if !ok {
		return docker.Container{}, fmt.Errorf("inspect: %w", docker.ErrNotFound)
	}
	return *c, nil
}

// UpdateMemory follows moby's Container.UpdateContainer: zero leaves a
// field as it is.
func (d *fakeDocker) UpdateMemory(_ context.Context, id string, u docker.MemoryUpdate) error {
	c, ok := d.cs[id]
	if !ok {
		return fmt.Errorf("update: %w", docker.ErrNotFound)
	}
	if u.Memory != 0 && u.Memory < 6<<20 {
		return errors.New("Minimum memory limit allowed is 6MB")
	}
	d.updates = append(d.updates, u)
	if u.Memory != 0 {
		c.HostConfig.Memory = u.Memory
	}
	if u.MemoryReservation != 0 {
		c.HostConfig.MemoryReservation = u.MemoryReservation
	}
	if u.MemorySwap != 0 {
		c.HostConfig.MemorySwap = u.MemorySwap
	}
	return nil
}

// harness is an engine on a fake box with a journal in a temporary file.
type harness struct {
	t   *testing.T
	box *fakeBox
	e   *Engine
	jp  string
}

func newHarness(t *testing.T) *harness {
	t.Helper()
	box := newBox(t)
	h := &harness{t: t, box: box, jp: filepath.Join(t.TempDir(), "journal")}
	h.reopen()
	return h
}

// reopen starts a new engine on the same box and journal, as a restarted
// helper would.
func (h *harness) reopen() {
	h.t.Helper()
	f, err := os.OpenFile(h.jp, os.O_RDWR|os.O_CREATE, 0o600)
	if err != nil {
		h.t.Fatal(err)
	}
	h.t.Cleanup(func() { f.Close() })
	jr, _, err := journal.Open(f, func() time.Time { return h.box.now })
	if err != nil {
		h.t.Fatal(err)
	}
	h.e = New(h.box, jr, Config{Exe: "/usr/libexec/snugbox/snugbox-helper", Pause: pauseFlag, MemTotal: testMem,
		Log: slog.New(slog.DiscardHandler)})
}

func (h *harness) do(req *helperv1.Request, by Caller) (*helperv1.Response, []*helperv1.Progress) {
	var steps []*helperv1.Progress
	resp := h.e.Handle(context.Background(), req, by, func(p *helperv1.Progress) { steps = append(steps, p) })
	return resp, steps
}

func applyReq(fix *helperv1.Fix, dry bool) *helperv1.Request {
	return &helperv1.Request{Kind: &helperv1.Request_Apply{Apply: &helperv1.ApplyRequest{Fix: fix, DryRun: dry}}}
}

func undoReq(tx, reason string) *helperv1.Request {
	return &helperv1.Request{Kind: &helperv1.Request_Undo{Undo: &helperv1.UndoRequest{Transaction: tx, Reason: reason}}}
}

func advanceReq(tx string, s helperv1.State, reason string) *helperv1.Request {
	return &helperv1.Request{Kind: &helperv1.Request_Advance{Advance: &helperv1.AdvanceRequest{Transaction: tx, State: s, Reason: reason}}}
}

// mustApply applies fix as root and returns the transaction.
func (h *harness) mustApply(fix *helperv1.Fix) *helperv1.ApplyResponse {
	h.t.Helper()
	resp, _ := h.do(applyReq(fix, false), Root)
	if e := resp.GetError(); e != nil {
		h.t.Fatalf("apply refused: %s: %s", e.GetCode(), e.GetMessage())
	}
	return resp.GetApply()
}

func wantError(t *testing.T, resp *helperv1.Response, code helperv1.ErrorCode, contains string) {
	t.Helper()
	e := resp.GetError()
	if e == nil {
		t.Fatalf("got %v, want error %s", resp, code)
	}
	if e.GetCode() != code || !strings.Contains(e.GetMessage(), contains) {
		t.Fatalf("got %s %q, want %s containing %q", e.GetCode(), e.GetMessage(), code, contains)
	}
}

// states lists the journal's states for a transaction.
func (h *harness) states(tx string) []string {
	var out []string
	for _, en := range h.e.jr.Entries() {
		if en.Tx == tx {
			out = append(out, en.State)
		}
	}
	return out
}

func (h *harness) state(tx string) helperv1.State {
	if v := h.e.find(tx); v != nil {
		return v.state
	}
	return helperv1.State_STATE_UNSPECIFIED
}
