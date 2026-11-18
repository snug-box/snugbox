package helper

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"path"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	"snugbox.dev/snugbox/agent/internal/docker"
	"snugbox.dev/snugbox/agent/internal/helperapi"
	"snugbox.dev/snugbox/agent/internal/kernel"
	helperv1 "snugbox.dev/snugbox/gen/snugbox/helper/v1"
)

// Plan is what one fix does, worked out by its preflight from the box as it
// is at that moment. It is the snapshot: saved before any step runs, it
// holds every old value undo needs.
type Plan struct {
	Kind     string   `json:"kind"`
	Steps    []Step   `json:"steps"`
	Cgroups  []string `json:"cgroups,omitempty"`
	AutoUndo bool     `json:"auto_undo"`
	// Restart lists the services undo restarts once their files are back.
	Restart []string `json:"restart,omitempty"`
	Notes   []string `json:"notes,omitempty"`
	// Manual holds changes Snugbox only shows, for the person to make.
	Manual []Manual `json:"manual,omitempty"`
}

// Manual is a change a person has to make, and why Snugbox didn't.
type Manual struct {
	Target  string `json:"target"`
	Content string `json:"content"`
	Why     string `json:"why"`
}

// HardFloor is how far above the memory a workload can't give back right now
// a hard limit must be, so applying it can't kill the workload on the spot.
const HardFloor = 1.1

func refuse(format string, args ...any) error {
	return helperapi.Errorf(helperv1.ErrorCode_ERROR_CODE_PRECONDITION, format, args...)
}

func (e *Engine) preflight(ctx context.Context, tx string, fix *helperv1.Fix) (*Plan, error) {
	var p *Plan
	var err error
	switch k := fix.GetKind().(type) {
	case *helperv1.Fix_Swap:
		p, err = e.planSwap(ctx, tx, k.Swap)
	case *helperv1.Fix_OomProtection:
		if k.OomProtection.GetDaemon() == helperv1.OOMDaemon_OOM_DAEMON_EARLYOOM {
			p, err = e.planEarlyoom(ctx, tx)
		} else {
			p, err = e.planOOM(ctx, tx)
		}
	case *helperv1.Fix_ContainerLimits:
		p, err = e.planContainers(ctx, tx, k.ContainerLimits)
	case *helperv1.Fix_ServiceLimits:
		p, err = e.planServices(ctx, tx, k.ServiceLimits)
	case *helperv1.Fix_HeapCaps:
		p, err = e.planHeaps(ctx, tx, k.HeapCaps)
	case *helperv1.Fix_Schedule:
		p, err = e.planSchedule(ctx, tx, k.Schedule)
	default:
		return nil, refuse("unknown fix")
	}
	if err != nil {
		return nil, err
	}
	p.Kind = helperapi.KindOf(fix)
	return p, nil
}

// header starts every file Snugbox writes, so a person who finds it knows
// where it came from and how to take it out.
func header(tx, kind string) string {
	return "# Made by Snugbox for transaction " + tx + ". snugbox undo " + tx + " takes it out.\n" +
		"# What it does and why: https://snugbox.dev/docs/fixes/" + kind + "/\n"
}

// mustBeAbsent refuses when path exists or can't be read, which would mean
// an earlier change, a person's own file or a planted link is in the way.
func (e *Engine) mustBeAbsent(p string) error {
	_, exists, err := readOptional(e.sys, p)
	switch {
	case err != nil:
		return refuse("%s is in the way: %v", p, err)
	case exists:
		return refuse("%s already exists; Snugbox doesn't change a file it didn't make", p)
	}
	return nil
}

// dropInPrefix starts the name of every drop-in Snugbox writes.
const dropInPrefix = "50-snugbox-"

// dropIn checks that a new drop-in has its place to itself, as mustBeAbsent
// does, and returns its folder when undo may take the folder away once it
// is empty: the folder is missing, so the fix makes it, or Snugbox's own
// drop-ins are all it holds. A folder a person made, even an empty one,
// gets "" and stays.
func (e *Engine) dropIn(drop string) (string, error) {
	if err := e.mustBeAbsent(drop); err != nil {
		return "", err
	}
	dir := path.Dir(drop)
	names, err := e.sys.ListDir(dir)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return dir, nil
	case err != nil:
		return "", refuse("%s is in the way: %v", dir, err)
	case len(names) == 0:
		return "", nil
	}
	for _, n := range names {
		if !strings.HasPrefix(n, dropInPrefix) {
			return "", nil
		}
	}
	return dir, nil
}

// container reports what kind of container the box is, or "" for a full
// virtual machine or bare metal. systemd-detect-virt --container prints
// "none" and exits with status 1 outside a container.
func (e *Engine) container(ctx context.Context) string {
	out, _ := e.sys.Tool(ctx, "systemd-detect-virt", nil, "--container")
	if v := strings.TrimSpace(out); v != "none" {
		if v == "" {
			return "unknown"
		}
		return v
	}
	return ""
}

func (e *Engine) planSwap(ctx context.Context, tx string, f *helperv1.SwapFix) (*Plan, error) {
	if v := e.container(ctx); v != "" {
		return nil, refuse("this box is a %s container, which shares its kernel with the host, so its swap can't be changed here", v)
	}
	raw, err := e.sys.ReadFile("/proc/swaps")
	if err != nil {
		return nil, err
	}
	swaps, err := kernel.ParseSwaps(raw)
	if err != nil {
		return nil, err
	}
	if len(swaps) > 0 {
		return nil, refuse("the box has swap already (%s); Snugbox leaves a swap setup it didn't make alone", swaps[0].Filename)
	}
	if fstab, _, err := readOptional(e.sys, "/etc/fstab"); err == nil && fstabHasSwap(fstab) {
		return nil, refuse("/etc/fstab lists a swap area that is off; that is a choice Snugbox leaves alone")
	}
	units, err := e.sys.Systemctl(ctx, "list-unit-files", "--type=swap", "--no-legend")
	if strings.TrimSpace(units) != "" {
		return nil, refuse("systemd has swap units of its own (%s); Snugbox leaves them alone", strings.Fields(units)[0])
	}
	if err != nil && !quietFailure(err) {
		return nil, err
	}
	free, fstype, err := e.sys.Disk(StateDir)
	if err != nil {
		return nil, err
	}
	size := f.GetSizeBytes()
	switch fstype {
	case "ext4", "xfs":
	case "btrfs":
		if _, err := e.sys.Tool(ctx, "btrfs", nil, "filesystem", "mkswapfile", "--help"); err != nil {
			return nil, refuse("%s is on Btrfs, and a swap file there needs btrfs filesystem mkswapfile from btrfs-progs 6.1 or newer, which this box doesn't have", StateDir)
		}
	default:
		// ZFS is left out on purpose: a swap file on it can deadlock.
		return nil, refuse("%s is on %s; Snugbox makes swap files only on ext4, XFS and Btrfs", StateDir, fstype)
	}
	if need := size + size/10 + 2*helperapi.GiB; free < need {
		return nil, refuse("a %s swap file needs %s free on %s, and %s is free", sizeText(size), sizeText(need), StateDir, sizeText(free))
	}
	for _, p := range []string{SwapFile, path.Join(UnitDir, SwapUnit)} {
		if err := e.mustBeAbsent(p); err != nil {
			return nil, err
		}
	}
	unit := header(tx, helperapi.KindSwap) + "[Unit]\nDescription=Swap file made by Snugbox\nDocumentation=https://snugbox.dev/docs/fixes/swap/\n\n" +
		"[Swap]\nWhat=" + SwapFile + "\n\n[Install]\nWantedBy=swap.target\n"
	p := &Plan{Steps: []Step{
		{Op: OpSwapFile, Target: SwapFile, Size: size, FS: fstype},
		{Op: OpFile, Target: path.Join(UnitDir, SwapUnit), New: unit, Mode: 0o644},
		{Op: OpReload},
		{Op: OpEnable, Target: SwapUnit},
	}}
	if f.GetCompressor() == helperv1.Compressor_COMPRESSOR_UNSPECIFIED {
		p.Notes = append(p.Notes, "The kernel has no zswap, so only the swap file is made and nothing is compressed.")
		return p, nil
	}
	zs, err := e.zswapSteps(tx, f)
	if err != nil {
		return nil, err
	}
	if len(zs) == 0 {
		p.Notes = append(p.Notes, "zswap is on already, so Snugbox keeps its settings.")
		return p, nil
	}
	p.Steps = append(p.Steps, zs...)
	return p, nil
}

func fstabHasSwap(fstab string) bool {
	for line := range strings.Lines(fstab) {
		f := strings.Fields(line)
		if len(f) >= 3 && !strings.HasPrefix(f[0], "#") && f[2] == "swap" {
			return true
		}
	}
	return false
}

func (e *Engine) zswapSteps(tx string, f *helperv1.SwapFix) ([]Step, error) {
	read := func(name string) (string, error) {
		b, err := e.sys.ReadFile(path.Join(ZswapDir, name))
		if errors.Is(err, fs.ErrNotExist) {
			return "", refuse("this kernel has no zswap %s setting", name)
		}
		return strings.TrimSpace(string(b)), err
	}
	enabled, err := read("enabled")
	if err != nil {
		return nil, err
	}
	if enabled == "Y" {
		return nil, nil
	}
	if err := e.mustBeAbsent(TmpFiles); err != nil {
		return nil, err
	}
	comp, err := read("compressor")
	if err != nil {
		return nil, err
	}
	want := "zstd"
	if f.GetCompressor() == helperv1.Compressor_COMPRESSOR_LZ4 {
		want = "lz4"
	}
	steps := []Step{{Op: OpKernel, Target: path.Join(ZswapDir, "compressor"), Old: comp, New: want}}
	boot := header(tx, helperapi.KindSwap) + "w " + path.Join(ZswapDir, "compressor") + " - - - - " + want + "\n"
	if f.GetShrinker() {
		shrinker, err := read("shrinker_enabled")
		if err != nil {
			return nil, err
		}
		steps = append(steps, Step{Op: OpKernel, Target: path.Join(ZswapDir, "shrinker_enabled"), Old: shrinker, New: "Y"})
		boot += "w " + path.Join(ZswapDir, "shrinker_enabled") + " - - - - Y\n"
	}
	steps = append(steps,
		Step{Op: OpKernel, Target: path.Join(ZswapDir, "enabled"), Old: enabled, New: "Y"},
		Step{Op: OpFile, Target: TmpFiles, New: boot + "w " + path.Join(ZswapDir, "enabled") + " - - - - Y\n", Mode: 0o644})
	return steps, nil
}

// unitProps reads unit properties with systemctl show.
func (e *Engine) unitProps(ctx context.Context, unit string, props ...string) (map[string][]string, error) {
	return showUnit(ctx, e.sys, unit, props...)
}

// showUnit reads unit properties with systemctl show. A property printed
// more than once, such as EnvironmentFiles, keeps every value.
func showUnit(ctx context.Context, sys System, unit string, props ...string) (map[string][]string, error) {
	out, err := sys.Systemctl(ctx, "show", "--property="+strings.Join(props, ","), "--", unit)
	if err != nil {
		return nil, err
	}
	m := make(map[string][]string, len(props))
	for line := range strings.Lines(out) {
		if k, v, ok := strings.Cut(strings.TrimRight(line, "\n"), "="); ok {
			m[k] = append(m[k], v)
		}
	}
	return m, nil
}

func first(m map[string][]string, k string) string {
	if v := m[k]; len(v) > 0 {
		return v[0]
	}
	return ""
}

func (e *Engine) planOOM(ctx context.Context, tx string) (*Plan, error) {
	if _, err := e.sys.ReadFile(CgroupFS + "/cgroup.controllers"); err != nil {
		return nil, refuse("systemd-oomd needs cgroup v2, which this box doesn't use")
	}
	oomd, err := e.unitProps(ctx, "systemd-oomd.service", "LoadState", "ActiveState", "UnitFileState")
	if err != nil {
		return nil, err
	}
	if first(oomd, "LoadState") != "loaded" {
		return nil, refuse("systemd-oomd is not installed; on Debian and Ubuntu it comes in the systemd-oomd package")
	}
	if err := e.noOOMDaemonRuns(ctx); err != nil {
		return nil, err
	}
	p := &Plan{}
	for _, slice := range []string{"system.slice", "user.slice"} {
		props, err := e.unitProps(ctx, slice, "ManagedOOMMemoryPressure")
		if err != nil {
			return nil, err
		}
		if v := first(props, "ManagedOOMMemoryPressure"); v != "auto" && v != "" {
			return nil, refuse("%s has ManagedOOMMemoryPressure=%s set already; Snugbox leaves that policy alone", slice, v)
		}
		drop := path.Join(UnitDir, slice+".d", "50-snugbox-oomd.conf")
		dir, err := e.dropIn(drop)
		if err != nil {
			return nil, err
		}
		p.Steps = append(p.Steps, Step{Op: OpFile, Target: drop, Dir: dir, Mode: 0o644,
			New: header(tx, helperapi.KindOOM) + "[Slice]\nManagedOOMMemoryPressure=kill\n"})
	}
	for _, ssh := range []string{"ssh.service", "sshd.service"} {
		props, err := e.unitProps(ctx, ssh, "LoadState")
		if err != nil {
			return nil, err
		}
		if first(props, "LoadState") != "loaded" {
			continue
		}
		drop := path.Join(UnitDir, ssh+".d", "50-snugbox-oomd.conf")
		dir, err := e.dropIn(drop)
		if err != nil {
			return nil, err
		}
		p.Steps = append(p.Steps, Step{Op: OpFile, Target: drop, Dir: dir, Mode: 0o644,
			New: header(tx, helperapi.KindOOM) + "[Service]\nManagedOOMPreference=omit\n"})
	}
	p.Steps = append(p.Steps, Step{Op: OpReload},
		Step{Op: OpEnable, Target: "systemd-oomd.service", Existed: first(oomd, "UnitFileState") == "enabled"})
	return p, nil
}

// noOOMDaemonRuns refuses when an early out-of-memory killer runs already,
// since two would fight over the same processes.
func (e *Engine) noOOMDaemonRuns(ctx context.Context) error {
	for _, u := range []string{"systemd-oomd.service", "earlyoom.service", "nohang.service"} {
		p, err := e.unitProps(ctx, u, "ActiveState")
		if err != nil {
			return err
		}
		if first(p, "ActiveState") == "active" {
			return refuse("%s runs already, and two out-of-memory daemons would fight", strings.TrimSuffix(u, ".service"))
		}
	}
	return nil
}

// earlyoomAvoid is the process names earlyoom must never pick, as a regular
// expression in a unit file, where $$ stands for $. sshd keeps the box
// reachable; the rest keep the system and Snugbox itself alive.
const earlyoomAvoid = `'^(sshd|systemd|systemd-.*|init|dbus-daemon|dbus-broker|snugboxd|snugbox-helper)$$'`

var execPath = regexp.MustCompile(`(?:^|[{ ;])path=(/[^ ;]+)`)

// planEarlyoom turns on earlyoom, the fallback for boxes without
// systemd-oomd (blueprint section 3.7). A drop-in sets its arguments, so the
// distribution's own files stay as they are: kill when free memory and free
// swap are both under earlyoom's default of 10%, report hourly instead of
// every second, and never pick sshd, systemd or Snugbox.
func (e *Engine) planEarlyoom(ctx context.Context, tx string) (*Plan, error) {
	unit, err := e.unitProps(ctx, "earlyoom.service", "LoadState", "UnitFileState", "ExecStart")
	if err != nil {
		return nil, err
	}
	if first(unit, "LoadState") != "loaded" {
		return nil, refuse("earlyoom is not installed; it comes in the earlyoom package of most distributions")
	}
	m := execPath.FindStringSubmatch(first(unit, "ExecStart"))
	if m == nil {
		return nil, refuse("can't tell which program earlyoom.service runs from its ExecStart, %q", first(unit, "ExecStart"))
	}
	if err := e.noOOMDaemonRuns(ctx); err != nil {
		return nil, err
	}
	drop := path.Join(UnitDir, "earlyoom.service.d", "50-snugbox-earlyoom.conf")
	dir, err := e.dropIn(drop)
	if err != nil {
		return nil, err
	}
	body := header(tx, helperapi.KindOOM) + "[Service]\nExecStart=\nExecStart=" + m[1] + " -r 3600 --avoid " + earlyoomAvoid + "\n"
	return &Plan{Steps: []Step{
		{Op: OpFile, Target: drop, Dir: dir, Mode: 0o644, New: body},
		{Op: OpReload},
		{Op: OpEnable, Target: "earlyoom.service", Existed: first(unit, "UnitFileState") == "enabled"},
	}}, nil
}

// hardMemory is what a cgroup holds that reclaim can't take back without
// swap, the same sum as model.Usage.Hard.
func (e *Engine) hardMemory(cgroup string) (uint64, error) {
	b, err := e.sys.ReadFile(path.Join(CgroupFS, cgroup, "memory.stat"))
	if err != nil {
		return 0, err
	}
	s, _ := kernel.ParseStat(b)
	var n uint64
	for _, k := range []string{"anon", "shmem", "kernel_stack", "pagetables", "percpu", "slab_unreclaimable", "vmalloc", "sock", "zswap"} {
		n += s.Get(k)
	}
	return n, nil
}

func (e *Engine) floor(what, cgroup string, limit uint64) error {
	hard, err := e.hardMemory(cgroup)
	if err != nil {
		return err
	}
	if float64(limit) < HardFloor*float64(hard) {
		return refuse("%s holds %s it can't give back right now, so a limit of %s would stop it at once", what, sizeText(hard), sizeText(limit))
	}
	return nil
}

// containerCgroup finds a container's cgroup under either of Docker's cgroup
// drivers.
func (e *Engine) containerCgroup(id string) string {
	for _, cg := range []string{"system.slice/docker-" + id + ".scope", "docker/" + id} {
		if _, err := e.sys.ReadFile(path.Join(CgroupFS, cg, "memory.max")); err == nil {
			return cg
		}
	}
	return ""
}

// Compose reads one of these as the project file and one override next to
// it, when it is started without -f (compose-go cli.DefaultFileNames and
// DefaultOverrideFileNames).
var (
	composeFiles   = []string{"compose.yaml", "compose.yml", "docker-compose.yml", "docker-compose.yaml"}
	overrideFiles  = []string{"compose.override.yml", "compose.override.yaml", "docker-compose.override.yml", "docker-compose.override.yaml"}
	serviceName    = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9_.-]*$`)
	overrideTarget = "compose.override.yaml"
)

type composeProject struct {
	dir, files string
	lines      []string
	oddName    bool
}

func (e *Engine) planContainers(ctx context.Context, tx string, f *helperv1.ContainerLimitsFix) (*Plan, error) {
	d := e.sys.Docker()
	if d == nil {
		return nil, refuse("Docker is not running")
	}
	p := &Plan{AutoUndo: true}
	var projects []*composeProject
	for _, l := range f.GetLimits() {
		id := l.GetContainerId()
		c, err := d.Inspect(ctx, id)
		if errors.Is(err, docker.ErrNotFound) {
			return nil, refuse("container %s is gone", id[:12])
		}
		if err != nil {
			return nil, err
		}
		name := strings.TrimPrefix(c.Name, "/")
		switch {
		case !c.State.Running:
			return nil, refuse("container %s is not running", name)
		case docker.Limited(c.HostConfig.Memory):
			return nil, refuse("container %s has a limit already (%s)", name, sizeText(bytesOf(c.HostConfig.Memory)))
		}
		cg := e.containerCgroup(id)
		if cg == "" {
			return nil, refuse("can't find the cgroup of container %s", name)
		}
		if err := e.floor("container "+name, cg, l.GetMaxBytes()); err != nil {
			return nil, err
		}
		p.Cgroups = append(p.Cgroups, cg)
		p.Steps = append(p.Steps, Step{Op: OpDocker, Target: id, Label: name,
			OldMemory: docker.MemoryUpdate{MemoryReservation: c.HostConfig.MemoryReservation, MemorySwap: c.HostConfig.MemorySwap},
			NewMemory: docker.MemoryUpdate{Memory: dockerBytes(l.GetMaxBytes()), MemoryReservation: dockerBytes(l.GetReservationBytes()), MemorySwap: -1}})
		if !f.GetPersist() {
			continue
		}
		labels := c.Config.Labels
		dir, svc := labels["com.docker.compose.project.working_dir"], labels["com.docker.compose.service"]
		if dir == "" || svc == "" {
			p.Notes = append(p.Notes, fmt.Sprintf("Container %s is not from a Compose project, so its limit lasts until the container is made again.", name))
			continue
		}
		i := slices.IndexFunc(projects, func(cp *composeProject) bool { return cp.dir == dir })
		if i < 0 {
			projects = append(projects, &composeProject{dir: dir, files: labels["com.docker.compose.project.config_files"]})
			i = len(projects) - 1
		}
		if !serviceName.MatchString(svc) {
			projects[i].oddName = true
		}
		block := "  " + svc + ":\n    mem_limit: " + strconv.FormatUint(l.GetMaxBytes()/helperapi.MiB, 10) + "m\n"
		if r := l.GetReservationBytes(); r > 0 {
			block += "    mem_reservation: " + strconv.FormatUint(r/helperapi.MiB, 10) + "m\n"
		}
		projects[i].lines = append(projects[i].lines, block)
	}
	for _, cp := range projects {
		step, manual := e.override(tx, cp)
		if step != nil {
			p.Steps = append(p.Steps, *step)
		}
		if manual != nil {
			p.Manual = append(p.Manual, *manual)
		}
	}
	return p, nil
}

// override decides whether Snugbox can write compose.override.yaml for a
// project, or only show it.
func (e *Engine) override(tx string, cp *composeProject) (*Step, *Manual) {
	content := header(tx, helperapi.KindContainer) + "services:\n" + strings.Join(cp.lines, "")
	target := path.Join(cp.dir, overrideTarget)
	manual := func(why string) (*Step, *Manual) {
		return nil, &Manual{Target: target, Content: content, Why: why}
	}
	if cp.oddName {
		return manual("a service name has characters Snugbox won't write into YAML unquoted")
	}
	if !path.IsAbs(cp.dir) || path.Clean(cp.dir) != cp.dir {
		return manual("the project folder in the container's labels is not a clean absolute path")
	}
	files := strings.Split(cp.files, ",")
	if len(files) != 1 || path.Dir(files[0]) != cp.dir || !slices.Contains(composeFiles, path.Base(files[0])) {
		return manual("the project was started with its own list of files, so Compose won't read an override file by itself; add these lines to one of them")
	}
	for _, name := range overrideFiles {
		if _, exists, err := readOptional(e.sys, path.Join(cp.dir, name)); exists || err != nil {
			return manual(name + " exists already; add these lines to it")
		}
	}
	uid, gid, err := e.sys.Owner(cp.dir)
	if err != nil {
		return manual("Snugbox can't read who owns the project folder: " + err.Error())
	}
	if uid == 0 {
		return manual("the project folder belongs to root, and Snugbox writes Compose files only as the folder's owner, never as root")
	}
	return &Step{Op: OpOverride, Target: target, New: content, UID: uid, GID: gid}, nil
}

func (e *Engine) serviceState(ctx context.Context, unit string, extra ...string) (map[string][]string, string, error) {
	props, err := e.unitProps(ctx, unit, append([]string{"LoadState", "ActiveState", "ControlGroup"}, extra...)...)
	if err != nil {
		return nil, "", err
	}
	switch {
	case first(props, "LoadState") != "loaded":
		return nil, "", refuse("%s is not a loaded unit", unit)
	case first(props, "ActiveState") != "active":
		return nil, "", refuse("%s is not running", unit)
	}
	cg := strings.TrimPrefix(first(props, "ControlGroup"), "/")
	if cg == "" {
		return nil, "", refuse("%s has no cgroup", unit)
	}
	return props, cg, nil
}

func (e *Engine) planServices(ctx context.Context, tx string, f *helperv1.ServiceLimitsFix) (*Plan, error) {
	p := &Plan{AutoUndo: true}
	var live []Step
	for _, l := range f.GetLimits() {
		unit := l.GetUnit()
		props, cg, err := e.serviceState(ctx, unit, "MemoryMax", "MemoryHigh")
		if err != nil {
			return nil, err
		}
		if first(props, "MemoryMax") != "infinity" || first(props, "MemoryHigh") != "infinity" {
			return nil, refuse("%s has a memory limit already (MemoryMax=%s, MemoryHigh=%s)", unit, first(props, "MemoryMax"), first(props, "MemoryHigh"))
		}
		for _, m := range memorySettings {
			f := runtimeFile(unit, m.name)
			_, exists, err := readOptional(e.sys, f)
			switch {
			case err != nil:
				return nil, refuse("%s is in the way: %v", f, err)
			case exists:
				return nil, refuse("%s has %s set at runtime with systemctl set-property (%s); Snugbox leaves a setting someone made alone, and the next reboot clears it", unit, m.name, f)
			}
		}
		if err := e.floor(unit, cg, l.GetHighBytes()); err != nil {
			return nil, err
		}
		drop := path.Join(UnitDir, unit+".d", "50-snugbox-memory.conf")
		dir, err := e.dropIn(drop)
		if err != nil {
			return nil, err
		}
		p.Cgroups = append(p.Cgroups, cg)
		p.Steps = append(p.Steps, Step{Op: OpFile, Target: drop, Dir: dir, Label: unit, Mode: 0o644, High: l.GetHighBytes(), Max: l.GetMaxBytes(),
			New: header(tx, helperapi.KindService) + fmt.Sprintf("[Service]\nMemoryHigh=%d\nMemoryMax=%d\n", l.GetHighBytes(), l.GetMaxBytes())})
		live = append(live, Step{Op: OpLimit, Target: unit, High: l.GetHighBytes(), Max: l.GetMaxBytes()})
	}
	// The reload loads the drop-ins; the OpLimit steps put the limits on
	// the running services where the reload didn't (see limits.go).
	p.Steps = append(append(p.Steps, Step{Op: OpReload}), live...)
	return p, nil
}

// heapSetting is the environment variable a heap cap sets.
func heapSetting(c *helperv1.HeapCap) (name, value string) {
	m := c.GetHeapBytes() / helperapi.MiB
	switch c.GetRuntime() {
	case helperv1.Runtime_RUNTIME_JAVA:
		return "JAVA_TOOL_OPTIONS", fmt.Sprintf("-Xmx%dm", m)
	case helperv1.Runtime_RUNTIME_NODE:
		return "NODE_OPTIONS", fmt.Sprintf("--max-old-space-size=%d", m)
	case helperv1.Runtime_RUNTIME_GO:
		return "GOMEMLIMIT", fmt.Sprintf("%dMiB", m)
	}
	return "MALLOC_ARENA_MAX", "2"
}

func (e *Engine) planHeaps(ctx context.Context, tx string, f *helperv1.HeapCapsFix) (*Plan, error) {
	p := &Plan{}
	var restarts []Step
	for _, c := range f.GetCaps() {
		unit := c.GetUnit()
		props, cg, err := e.serviceState(ctx, unit, "MemoryMax", "Environment", "EnvironmentFiles")
		if err != nil {
			return nil, err
		}
		name, value := heapSetting(c)
		if c.GetRuntime() != helperv1.Runtime_RUNTIME_GLIBC {
			limit, err := strconv.ParseUint(first(props, "MemoryMax"), 10, 64)
			if err != nil {
				return nil, refuse("%s has no MemoryMax, and a heap cap is set as a share of it; limit the service first", unit)
			}
			if most := helperapi.MaxHeap(c.GetRuntime(), limit); c.GetHeapBytes() > most {
				return nil, refuse("a heap of %s is more than the %s Snugbox allows under the %s limit of %s", sizeText(c.GetHeapBytes()), sizeText(most), sizeText(limit), unit)
			}
		}
		if err := e.envFree(unit, name, props); err != nil {
			return nil, err
		}
		drop := path.Join(UnitDir, unit+".d", "50-snugbox-heap.conf")
		dir, err := e.dropIn(drop)
		if err != nil {
			return nil, err
		}
		p.Cgroups = append(p.Cgroups, cg)
		p.Restart = append(p.Restart, unit)
		p.Steps = append(p.Steps, Step{Op: OpFile, Target: drop, Dir: dir, Label: unit, Mode: 0o644,
			New: header(tx, helperapi.KindHeap) + "[Service]\nEnvironment=" + name + "=" + value + "\n"})
		restarts = append(restarts, Step{Op: OpRestart, Target: unit})
	}
	p.Steps = append(append(p.Steps, Step{Op: OpReload}), restarts...)
	return p, nil
}

// timerProps are what the schedule fix reads of a timer.
var timerProps = []string{"LoadState", "ActiveState", "Unit", "TimersCalendar", "TimersMonotonic"}

// planSchedule moves each timer earlier with a drop-in that replaces its
// one calendar trigger. Only a timer that systemd runs, that fires on the
// calendar alone, and that starts a unit a fix may touch is moved, and
// only by a shift that keeps it on its days.
//
// systemd works out a timer's next run from its last one, even after a
// restart, and runs the job at once when the calendar has a time between
// the two. Moved earlier, a calendar has none, unless the job is due
// within the move, and that move is refused. Moved later, it would always
// have one, which is why Snugbox never moves a timer later.
func (e *Engine) planSchedule(ctx context.Context, tx string, f *helperv1.ScheduleFix) (*Plan, error) {
	var files []Step
	for _, s := range f.GetShifts() {
		timer := s.GetTimer()
		props, err := e.unitProps(ctx, timer, timerProps...)
		if err != nil {
			return nil, err
		}
		cals := slices.DeleteFunc(slices.Clone(props["TimersCalendar"]), func(v string) bool { return v == "" })
		switch unit := first(props, "Unit"); {
		case first(props, "LoadState") != "loaded":
			return nil, refuse("%s is not a timer on this box", timer)
		case first(props, "ActiveState") != "active":
			return nil, refuse("%s is not running, so it starts nothing to move", timer)
		case helperapi.ProtectedUnit(unit):
			return nil, refuse("%s starts %s, which no fix may touch", timer, unit)
		case unit != s.GetService():
			return nil, refuse("%s starts %s now, not %s; the analysis is out of date", timer, unit, s.GetService())
		case first(props, "TimersMonotonic") != "":
			return nil, refuse("%s also fires a time after boot or after its last run, which a new calendar wouldn't move", timer)
		case len(cals) != 1:
			return nil, refuse("%s has %d calendar triggers; Snugbox moves a timer that has exactly one", timer, len(cals))
		}
		spec, next, ok := calendarTrigger(cals[0])
		if !ok {
			return nil, refuse("can't read the calendar of %s from %q", timer, cals[0])
		}
		shifted, err := shiftCalendar(spec, int(s.GetMinutes()))
		if err != nil {
			return nil, refuse("%s fires at %s, which Snugbox won't move %d minutes earlier: %v. RandomizedDelaySec in a drop-in of your own spreads it instead",
				timer, spec, s.GetMinutes(), err)
		}
		if err := e.notDueSoon(timer, s.GetService(), next, int(s.GetMinutes())); err != nil {
			return nil, err
		}
		drop := path.Join(UnitDir, timer+".d", helperapi.ScheduleDropIn)
		dir, err := e.dropIn(drop)
		if err != nil {
			return nil, err
		}
		files = append(files, Step{Op: OpFile, Target: drop, Dir: dir, Label: timer, Mode: 0o644, Calendar: shifted, Minutes: int(s.GetMinutes()),
			New: header(tx, helperapi.KindSchedule) + "[Timer]\nOnCalendar=\nOnCalendar=" + shifted + "\n"})
	}
	return &Plan{Steps: append(files, Step{Op: OpReload})}, nil
}

// systemdUTC is how systemctl prints a time when TZ is UTC.
const systemdUTC = "Mon 2006-01-02 15:04:05 UTC"

// dueSoonSlack is how far ahead of now the new time must still be, so the
// move is in force before it comes.
const dueSoonSlack = time.Minute

// notDueSoon refuses a move earlier by minutes when the job is due within
// them, or within a minute more: its new time would have passed already,
// counted from its last run, and systemd would start it at once.
func (e *Engine) notDueSoon(timer, service, next string, minutes int) error {
	if next == "" {
		return refuse("%s never fires again, so there is nothing to move", timer)
	}
	at, err := time.ParseInLocation(systemdUTC, next, time.UTC)
	if err != nil {
		return refuse("can't read when %s fires next from %q", timer, next)
	}
	if !at.Add(-time.Duration(minutes) * time.Minute).After(e.sys.Now().Add(dueSoonSlack)) {
		return refuse("%s starts %s next at %s; moved %d minutes earlier now, it would start it at once. Apply this after %s",
			timer, service, clock(at), minutes, clock(at))
	}
	return nil
}

// clock is a time of day in the box's own time zone, such as "02:20 CET".
func clock(t time.Time) string { return t.Local().Format("15:04 MST") }

// envFree refuses a heap cap when the service sets the same variable itself,
// in its unit or in an environment file: theirs would win, or ours would
// throw theirs away.
func (e *Engine) envFree(unit, name string, props map[string][]string) error {
	for _, env := range props["Environment"] {
		if strings.Contains(" "+env, " "+name+"=") || strings.Contains(env, `"`+name+"=") {
			return refuse("%s sets %s already; add the flag there instead", unit, name)
		}
	}
	for _, ef := range props["EnvironmentFiles"] {
		file, _, _ := strings.Cut(ef, " (")
		if file == "" {
			continue
		}
		b, exists, err := readOptional(e.sys, file)
		if err != nil {
			return refuse("can't read %s, an environment file of %s: %v", file, unit, err)
		}
		if !exists {
			continue
		}
		for line := range strings.Lines(b) {
			l := strings.TrimPrefix(strings.TrimSpace(line), "export ")
			if strings.HasPrefix(l, name+"=") {
				return refuse("%s sets %s already in %s; add the flag there instead", unit, name, file)
			}
		}
	}
	return nil
}

// confirm checks right after the steps that the box took the change, before
// the transaction counts as applied.
func (e *Engine) confirm(ctx context.Context, p *Plan) error {
	for _, s := range p.Steps {
		switch {
		case s.Op == OpEnable:
			props, err := e.unitProps(ctx, s.Target, "ActiveState")
			if err != nil {
				return err
			}
			if st := first(props, "ActiveState"); st != "active" {
				return fmt.Errorf("%s is %s after starting it", s.Target, st)
			}
		case s.Op == OpRestart:
			props, err := e.unitProps(ctx, s.Target, "ActiveState")
			if err != nil {
				return err
			}
			if st := first(props, "ActiveState"); st != "active" && st != "activating" {
				return fmt.Errorf("%s is %s after the restart", s.Target, st)
			}
		case s.Op == OpFile && s.Max > 0:
			if err := e.confirmLimit(ctx, s); err != nil {
				return err
			}
		case s.Op == OpFile && s.Calendar != "":
			props, err := e.unitProps(ctx, s.Label, "TimersCalendar")
			if err != nil {
				return err
			}
			if got, _, _ := calendarTrigger(first(props, "TimersCalendar")); got != s.Calendar {
				return fmt.Errorf("%s fires at %q after the reload, not %q", s.Label, got, s.Calendar)
			}
		}
	}
	if p.Kind == helperapi.KindSwap {
		if on, err := swapActive(e.sys, SwapFile); err != nil || !on {
			return errors.Join(err, errors.New("the swap file is not in use after starting its unit"))
		}
	}
	return nil
}

func (e *Engine) confirmLimit(ctx context.Context, s Step) error {
	props, err := e.unitProps(ctx, s.Label, "ControlGroup")
	if err != nil {
		return err
	}
	cg := path.Join(CgroupFS, strings.TrimPrefix(first(props, "ControlGroup"), "/"))
	for _, c := range []struct {
		file string
		want uint64
	}{{"memory.max", s.Max}, {"memory.high", s.High}} {
		b, err := e.sys.ReadFile(path.Join(cg, c.file))
		if err != nil {
			return err
		}
		if got := strings.TrimSpace(string(b)); got != strconv.FormatUint(c.want, 10) {
			return fmt.Errorf("%s of %s reads %s after the change, not %d", c.file, s.Label, got, c.want)
		}
	}
	return nil
}

// Changes describes a plan for people: one entry per thing that changes,
// with the old and new value, and the changes Snugbox only shows.
func (p *Plan) Changes() []*helperv1.Change {
	var out []*helperv1.Change
	for _, s := range p.Steps {
		switch s.Op {
		case OpFile, OpOverride:
			out = append(out, &helperv1.Change{Target: s.Target, OldValue: s.Old, NewValue: s.New, File: true})
		case OpKernel:
			out = append(out, &helperv1.Change{Target: s.Target, OldValue: s.Old, NewValue: s.New})
		case OpEnable:
			out = append(out, &helperv1.Change{Target: s.Target, OldValue: enabledText(s.Existed), NewValue: "enabled and running"})
		case OpRestart:
			out = append(out, &helperv1.Change{Target: s.Target, OldValue: "running", NewValue: "restarted"})
		case OpDocker:
			out = append(out, &helperv1.Change{Target: s.Label + " memory limit", OldValue: "none", NewValue: sizeText(bytesOf(s.NewMemory.Memory))},
				&helperv1.Change{Target: s.Label + " soft limit", OldValue: softText(s.OldMemory.MemoryReservation), NewValue: softText(s.NewMemory.MemoryReservation)})
		case OpSwapFile:
			out = append(out, &helperv1.Change{Target: s.Target, OldValue: "none", NewValue: sizeText(s.Size) + " swap file"})
		}
	}
	for _, m := range p.Manual {
		out = append(out, &helperv1.Change{Target: m.Target, NewValue: m.Content, File: true, Manual: true})
	}
	return out
}

func enabledText(on bool) string {
	if on {
		return "enabled, not running"
	}
	return "off"
}

func softText(b int64) string {
	if b <= 0 {
		return "none"
	}
	return sizeText(bytesOf(b))
}
