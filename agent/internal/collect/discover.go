package collect

import (
	"context"
	"io/fs"
	"path"
	"regexp"
	"slices"
	"strconv"
	"strings"

	"snugbox.dev/snugbox/agent/internal/kernel"
	"snugbox.dev/snugbox/agent/internal/model"
)

const (
	maxPIDsPerWorkload = 256
	maxPrograms        = 3
)

var (
	containerScope = regexp.MustCompile(`^(?:docker|libpod|cri-containerd|crio)-([0-9a-f]{64})\.scope$`)
	bareContainer  = regexp.MustCompile(`^[0-9a-f]{64}$`)
)

// Discover walks the cgroup tree and the root cgroup's processes, and
// replaces the collector's workload list. Containers are named through the
// ContainerSource when there is one.
func (c *Collector) Discover(ctx context.Context) error {
	found := make(map[string]model.Workload)
	dirs := make(map[string]string)
	groups := make(map[string][]int)
	c.goSeen = make(map[int]goExe)
	c.fpmSeen = make(map[string]uint64)

	if c.cgroupRoot == "" {
		c.gap("cgroup v2", fs.ErrNotExist)
	} else {
		c.walk(c.cgroupRoot, func(dir string) {
			w, ok := c.inspectLeaf(dir)
			if ok {
				found[w.ID], dirs[w.ID] = w, dir
			}
		})
		for id, pids := range c.rootGroups() {
			found[id] = c.programWorkload(id, pids)
			groups[id] = pids
		}
	}
	c.nameContainers(ctx, found)
	c.workloads, c.cgroupDir, c.groupPIDs = found, dirs, groups
	c.goExes, c.goSeen = c.goSeen, nil
	c.fpmLargest, c.fpmSeen = c.fpmSeen, nil
	return ctx.Err()
}

// skippedUnits are systemd unit types whose cgroups hold only a helper for a
// moment, such as the mount command of a .mount unit. They are not
// workloads.
var skippedUnits = []string{".mount", ".swap", ".socket", ".device", ".automount"}

// walk calls leaf for every cgroup that is one workload. Slices are always
// entered. A systemd unit, a container or a cgroup that holds processes is a
// workload and is not entered, since its files already cover everything
// below it. Other cgroups, such as the /docker parent the cgroupfs driver
// makes, are entered.
func (c *Collector) walk(dir string, leaf func(string)) {
	entries, err := fs.ReadDir(c.fsys, dir)
	if err != nil {
		c.gap("cgroup tree under /"+dir, err)
		return
	}
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		name, child := e.Name(), path.Join(dir, e.Name())
		switch {
		case strings.HasSuffix(name, ".slice"):
			c.walk(child, leaf)
		case slices.ContainsFunc(skippedUnits, func(s string) bool { return strings.HasSuffix(name, s) }):
		case strings.HasSuffix(name, ".service"), strings.HasSuffix(name, ".scope"),
			bareContainer.MatchString(name), strings.HasPrefix(name, "lxc.payload."):
			leaf(child)
		case len(c.readPIDs(path.Join(child, "cgroup.procs"))) > 0 || !hasSubdirs(c.fsys, child):
			leaf(child)
		default:
			c.walk(child, leaf)
		}
	}
}

func hasSubdirs(fsys fs.FS, dir string) bool {
	entries, err := fs.ReadDir(fsys, dir)
	if err != nil {
		return false
	}
	return slices.ContainsFunc(entries, fs.DirEntry.IsDir)
}

// inspectLeaf builds the workload for one cgroup. It skips cgroups with no
// processes and no memory charged, which systemd leaves behind briefly.
func (c *Collector) inspectLeaf(dir string) (model.Workload, bool) {
	id := "/" + strings.TrimPrefix(strings.TrimPrefix(dir, c.cgroupRoot), "/")
	pids := c.treePIDs(dir, maxPIDsPerWorkload)
	if len(pids) == 0 {
		cur, err := c.read(path.Join(dir, "memory.current"))
		if err != nil {
			return model.Workload{}, false
		}
		if n, err := kernel.ParseUint(cur); err != nil || n == 0 {
			return model.Workload{}, false
		}
	}
	name := path.Base(dir)
	w := model.Workload{ID: id, Kind: model.KindGroup, Name: name}
	switch {
	case containerScope.MatchString(name):
		w.Kind, w.ContainerID = model.KindContainer, containerScope.FindStringSubmatch(name)[1]
		w.Name = w.ContainerID[:12]
	case bareContainer.MatchString(name):
		w.Kind, w.ContainerID, w.Name = model.KindContainer, name, name[:12]
	case strings.HasPrefix(name, "lxc.payload."):
		w.Kind, w.Name = model.KindContainer, strings.TrimPrefix(name, "lxc.payload.")
	case strings.HasSuffix(name, ".service"):
		w.Kind = model.KindService
	}
	w.Max = c.readLimit(path.Join(dir, "memory.max"))
	w.High = c.readLimit(path.Join(dir, "memory.high"))
	c.describePrograms(&w, pids)
	return w, true
}

func (c *Collector) readLimit(name string) model.Limit {
	b, err := c.read(name)
	if err != nil {
		return model.Limit{}
	}
	l, err := kernel.ParseLimit(b)
	if err != nil || l.Unlimited {
		return model.Limit{}
	}
	return model.Limit{Bytes: l.Bytes, Set: true}
}

type procInfo struct {
	pid     int
	ppid    int
	name    string
	rss     uint64
	threads int
}

// describePrograms fills in the workload's program names, largest first, and
// the database and runtime they show.
func (c *Collector) describePrograms(w *model.Workload, pids []int) {
	rss := make(map[string]uint64)
	var procs []procInfo
	for _, pid := range pids {
		p, ok := c.procStatus(pid)
		if !ok {
			continue
		}
		rss[p.name] += p.rss
		procs = append(procs, p)
	}
	names := make([]string, 0, len(rss))
	for n := range rss {
		names = append(names, n)
	}
	slices.SortFunc(names, func(a, b string) int {
		if rss[a] != rss[b] {
			if rss[a] > rss[b] {
				return -1
			}
			return 1
		}
		return strings.Compare(a, b)
	})
	w.Programs = names[:min(len(names), maxPrograms)]
	for _, n := range names {
		if db := databaseOf(n); db != "" && w.Database == "" {
			w.Database = db
		}
	}
	c.describeDatabase(w, procs)
	c.describeRuntime(w, procs)
	c.describeOrphans(w, procs)
}

// databaseFlags are the command-line settings kept for each database: the
// memory ones the report reasons about. A flag ending in "=" carries its
// value; one that doesn't takes the next argument as its value.
var databaseFlags = map[string][]string{
	"PostgreSQL": {"shared_buffers=", "work_mem=", "max_connections=", "--shared_buffers=", "--work_mem=", "--max_connections="},
	"MySQL":      {"--innodb-buffer-pool-size=", "--innodb_buffer_pool_size=", "--innodb-buffer-pool-size", "--innodb_buffer_pool_size"},
	"MariaDB":    {"--innodb-buffer-pool-size=", "--innodb_buffer_pool_size=", "--innodb-buffer-pool-size", "--innodb_buffer_pool_size"},
	"Redis":      {"--maxmemory"},
	"Valkey":     {"--maxmemory"},
	"MongoDB":    {"--wiredTigerCacheSizeGB", "--wiredTigerCacheSizeGB="},
}

// describeDatabase keeps the memory settings on the command line of the
// database's main process, the one whose parent is not the database
// itself. Settings in configuration files stay unread.
func (c *Collector) describeDatabase(w *model.Workload, procs []procInfo) {
	flags := databaseFlags[w.Database]
	if len(flags) == 0 {
		return
	}
	byPID := make(map[int]procInfo, len(procs))
	for _, p := range procs {
		byPID[p.pid] = p
	}
	for _, p := range procs {
		if databaseOf(p.name) != w.Database || databaseOf(byPID[p.ppid].name) == w.Database {
			continue
		}
		b, err := c.read(path.Join("proc", strconv.Itoa(p.pid), "cmdline"))
		if err != nil {
			return
		}
		w.DBSettings = settingFlags(b, flags)
		return
	}
}

// settingFlags returns the arguments of a NUL-separated command line that
// match flags, joined with their value when it is the next argument.
func settingFlags(cmdline []byte, flags []string) []string {
	args := strings.Split(strings.TrimRight(string(cmdline), "\x00"), "\x00")
	var out []string
	for i, arg := range args {
		for _, f := range flags {
			switch {
			case strings.HasSuffix(f, "=") && strings.HasPrefix(arg, f) && len(arg) <= 64:
				out = append(out, arg)
			case arg == f && i+1 < len(args) && len(args[i+1]) <= 32:
				out = append(out, arg+" "+args[i+1])
			}
		}
	}
	return out
}

func (c *Collector) procStatus(pid int) (procInfo, bool) {
	b, err := c.read(path.Join("proc", strconv.Itoa(pid), "status"))
	if err != nil {
		return procInfo{}, false
	}
	st, err := kernel.ParseProcStatus(b)
	if err != nil || st.Kernel {
		return procInfo{}, false
	}
	return procInfo{pid: pid, ppid: st.PPid, name: cleanName(st.Name), rss: st.VmRSS, threads: st.Threads}, true
}

// rootGroups groups the processes in the root cgroup by program name. On a
// systemd box this is usually empty; on a box without systemd it is everything.
func (c *Collector) rootGroups() map[string][]int {
	out := make(map[string][]int)
	for _, pid := range c.readPIDs(path.Join(c.cgroupRoot, "cgroup.procs")) {
		p, ok := c.procStatus(pid)
		if !ok {
			continue
		}
		id := "proc:" + p.name
		out[id] = append(out[id], pid)
	}
	return out
}

func (c *Collector) programWorkload(id string, pids []int) model.Workload {
	w := model.Workload{ID: id, Kind: model.KindGroup, Name: strings.TrimPrefix(id, "proc:"), FromSmaps: true}
	c.describePrograms(&w, pids)
	return w
}

// nameContainers replaces container IDs with the runtime's names and Compose
// labels, and keeps the runtime's whole answer, which also covers containers
// waiting to restart and so missing from the cgroup tree.
func (c *Collector) nameContainers(ctx context.Context, ws map[string]model.Workload) {
	if c.containers == nil {
		return
	}
	var ids []string
	for _, w := range ws {
		if w.ContainerID != "" {
			ids = append(ids, w.ContainerID)
		}
	}
	slices.Sort(ids)
	meta, err := c.containers.Containers(ctx, ids)
	if err != nil {
		if len(ids) > 0 {
			c.gap("container names from Docker", err)
		}
		return
	}
	c.containerMeta = meta
	for id, w := range ws {
		m, ok := meta[w.ContainerID]
		if !ok || w.ContainerID == "" {
			continue
		}
		if m.Name != "" {
			w.Name = m.Name
		}
		w.Compose = m.Compose
		w.Restarts = m.Restarts
		if m.Reservation > 0 {
			w.Reservation = model.Limit{Bytes: m.Reservation, Set: true}
		}
		if w.Database == "" {
			w.Database = databaseOfImage(m.Image)
		}
		ws[id] = w
	}
}

// databaseOf names the database a program name belongs to. The kernel keeps
// 15 bytes of a command name, so long names are matched by prefix.
func databaseOf(program string) string {
	switch {
	case program == "postgres", program == "postmaster":
		return "PostgreSQL"
	case program == "mysqld":
		return "MySQL"
	case program == "mariadbd":
		return "MariaDB"
	case program == "redis-server":
		return "Redis"
	case program == "valkey-server":
		return "Valkey"
	case program == "mongod":
		return "MongoDB"
	case strings.HasPrefix(program, "clickhouse-serv"):
		return "ClickHouse"
	}
	return ""
}

func databaseOfImage(image string) string {
	repo := image
	if i := strings.LastIndex(repo, "/"); i >= 0 {
		repo = repo[i+1:]
	}
	repo, _, _ = strings.Cut(repo, ":")
	repo, _, _ = strings.Cut(repo, "@")
	switch repo {
	case "postgres", "postgis", "timescaledb":
		return "PostgreSQL"
	case "mysql":
		return "MySQL"
	case "mariadb":
		return "MariaDB"
	case "redis", "redis-stack-server":
		return "Redis"
	case "valkey":
		return "Valkey"
	case "mongo":
		return "MongoDB"
	case "clickhouse-server":
		return "ClickHouse"
	}
	return ""
}

// cleanName drops control characters from a command name before it reaches a
// terminal or a report.
func cleanName(s string) string {
	return strings.Map(func(r rune) rune {
		if r < 0x20 || r == 0x7f {
			return -1
		}
		return r
	}, s)
}
