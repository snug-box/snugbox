package collect

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"
	"testing/fstest"
	"time"

	"snugbox.dev/snugbox/agent/internal/model"
)

const (
	cid       = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
	mountinfo = "22 1 0:21 / /sys/fs/cgroup rw,nosuid - cgroup2 cgroup2 rw,nsdelegate\n" +
		"23 1 252:1 / / rw,relatime - ext4 /dev/vda1 rw\n"
)

func file(s string) *fstest.MapFile { return &fstest.MapFile{Data: []byte(s)} }

func cgroupFiles(fsys fstest.MapFS, dir string, procs string, anon, file_, active uint64) {
	fsys[dir+"/cgroup.procs"] = file(procs)
	fsys[dir+"/memory.current"] = file(fmt.Sprint(anon + file_))
	fsys[dir+"/memory.stat"] = file(fmt.Sprintf("anon %d\nfile %d\nshmem 0\nactive_file %d\ninactive_file %d\nkernel_stack 16384\npagetables 8192\nslab_reclaimable 4096\nslab_unreclaimable 4096\nsock 0\nworkingset_refault_file 7\n", anon, file_, active, file_-active))
	fsys[dir+"/memory.events"] = file("low 0\nhigh 0\nmax 0\noom 1\noom_kill 1\noom_group_kill 0\n")
	fsys[dir+"/memory.pressure"] = file("some avg10=0.00 avg60=0.00 avg300=0.00 total=1500\nfull avg10=0.00 avg60=0.00 avg300=0.00 total=200\n")
	fsys[dir+"/memory.peak"] = file(fmt.Sprint(2 * (anon + file_)))
	fsys[dir+"/memory.max"] = file("max\n")
	fsys[dir+"/memory.high"] = file("max\n")
}

func status(name string, rss uint64) *fstest.MapFile {
	return file(fmt.Sprintf("Name:\t%s\nTgid:\t1\nPPid:\t1\nUid:\t0\t0\t0\t0\nVmRSS:\t%d kB\nRssAnon:\t%d kB\n", name, rss, rss))
}

// fixture is a small Ubuntu-like box: a service, a Compose container, a login
// session, an empty unit left behind, and one process in the root cgroup.
func fixture() fstest.MapFS {
	f := fstest.MapFS{
		"proc/meminfo":                               file("MemTotal: 8000000 kB\nMemAvailable: 6000000 kB\nSlab: 100000 kB\nPageTables: 10000 kB\nKernelStack: 5000 kB\nSwapTotal: 0 kB\nSwapFree: 0 kB\n"),
		"proc/vmstat":                                file("pswpin 0\noom_kill 3\n"),
		"proc/pressure/memory":                       file("some avg10=0.00 avg60=0.00 avg300=0.00 total=10\nfull avg10=0.00 avg60=0.00 avg300=0.00 total=5\n"),
		"proc/stat":                                  file("cpu  100 0 50 1000 10 0 5 0 0 0\nbtime 1793417075\n"),
		"proc/self/mountinfo":                        file(mountinfo),
		"proc/sys/kernel/hostname":                   file("web-1\n"),
		"proc/sys/kernel/osrelease":                  file("6.8.0-45-generic\n"),
		"proc/sys/vm/overcommit_memory":              file("0\n"),
		"proc/sys/vm/swappiness":                     file("60\n"),
		"proc/uptime":                                file("864000.12 100.00\n"),
		"proc/swaps":                                 file("Filename\t\t\t\tType\t\tSize\t\tUsed\t\tPriority\n"),
		"proc/net/dev":                               file("Inter-|   Receive |  Transmit\n face |bytes |bytes\n    lo: 100 1 0 0 0 0 0 0 100 1 0 0 0 0 0 0\n  eth0: 9000 1 0 0 0 0 0 0 864000 1 0 0 0 0 0 0\nveth1: 1 1 0 0 0 0 0 0 999999 1 0 0 0 0 0 0\n"),
		"sys/class/net/eth0/device/uevent":           file(""),
		"etc/os-release":                             file("PRETTY_NAME=\"Ubuntu 24.04.1 LTS\"\nID=ubuntu\nVERSION_ID=\"24.04\"\n"),
		"sys/devices/system/cpu/online":              file("0-1\n"),
		"sys/kernel/mm/transparent_hugepage/enabled": file("always [madvise] never\n"),
		"sys/class/dmi/id/sys_vendor":                file("DigitalOcean\n"),
		"sys/fs/cgroup/cgroup.controllers":           file("cpuset cpu io memory pids\n"),
		"sys/fs/cgroup/memory.reclaim":               file(""),
		"sys/fs/cgroup/cgroup.procs":                 file("2\n900\n"),
		"proc/2/status":                              file("Name:\tkthreadd\nTgid:\t2\nPPid:\t0\nUid:\t0\t0\t0\t0\n"),
		"proc/900/status":                            status("tini", 2048),
		"proc/900/smaps_rollup":                      file("0-1 ---p 0 00:00 0 [rollup]\nRss: 4096 kB\nPss: 3072 kB\nPss_Anon: 2048 kB\nPss_File: 1024 kB\nPss_Shmem: 0 kB\nAnonymous: 2048 kB\nSwapPss: 0 kB\n"),
		"proc/100/status":                            status("nginx", 10240),
		"proc/101/status":                            status("nginx", 10240),
		"proc/200/status":                            status("node", 200000),
		"proc/200/cmdline":                           file("node\x00--max-old-space-size=1536\x00server.js\x00--db-password=hunter2\x00"),
		"proc/300/status":                            status("bash", 4000),
	}
	cgroupFiles(f, "sys/fs/cgroup/system.slice/nginx.service", "100\n101\n", 20<<20, 10<<20, 4<<20)
	cgroupFiles(f, "sys/fs/cgroup/system.slice/docker-"+cid+".scope", "200\n", 400<<20, 100<<20, 60<<20)
	cgroupFiles(f, "sys/fs/cgroup/user.slice/user-1000.slice/session-3.scope", "300\n", 5<<20, 1<<20, 1<<20)
	f["sys/fs/cgroup/system.slice/gone.service/cgroup.procs"] = file("")
	f["sys/fs/cgroup/system.slice/gone.service/memory.current"] = file("0\n")
	f["sys/fs/cgroup/system.slice/docker-"+cid+".scope/memory.max"] = file("1073741824\n")
	return f
}

type fakeDocker map[string]ContainerMeta

func (f fakeDocker) Containers(_ context.Context, ids []string) (map[string]ContainerMeta, error) {
	if !slices.Equal(ids, []string{cid}) {
		return nil, fmt.Errorf("asked about %q, want the one container in the cgroup tree", ids)
	}
	return f, nil
}

type failingDocker struct{}

func (failingDocker) Containers(context.Context, []string) (map[string]ContainerMeta, error) {
	return nil, errors.New("permission denied while trying to connect to the Docker daemon socket")
}

func TestDiscover(t *testing.T) {
	c, err := New(Options{FS: fixture(), PageSize: 4096, Containers: fakeDocker{cid: {Name: "shop-app-1", Image: "node:24", Compose: model.Compose{Project: "shop", Service: "app"}, Restarts: 2}}})
	if err != nil {
		t.Fatal(err)
	}
	if c.CgroupRoot() != "sys/fs/cgroup" {
		t.Fatalf("cgroup root %q", c.CgroupRoot())
	}
	if err := c.Discover(context.Background()); err != nil {
		t.Fatal(err)
	}
	ws := c.Workloads()
	ids := slices.Sorted(func(yield func(string) bool) {
		for id := range ws {
			if !yield(id) {
				return
			}
		}
	})
	want := []string{"/system.slice/docker-" + cid + ".scope", "/system.slice/nginx.service", "/user.slice/user-1000.slice/session-3.scope", "proc:tini"}
	if !slices.Equal(ids, want) {
		t.Fatalf("workloads %q, want %q", ids, want)
	}
	app := ws["/system.slice/docker-"+cid+".scope"]
	if app.Kind != model.KindContainer || app.Name != "shop-app-1" || app.Compose.Service != "app" || app.Restarts != 2 {
		t.Errorf("container %+v", app)
	}
	if !app.Max.Set || app.Max.Bytes != 1<<30 || app.High.Set {
		t.Errorf("container limits max %+v high %+v", app.Max, app.High)
	}
	if app.Runtime != "node" || !slices.Equal(app.HeapFlags, []string{"--max-old-space-size=1536"}) {
		t.Errorf("runtime %q flags %q; the password argument must never be kept", app.Runtime, app.HeapFlags)
	}
	if ng := ws["/system.slice/nginx.service"]; ng.Kind != model.KindService || !slices.Equal(ng.Programs, []string{"nginx"}) {
		t.Errorf("service %+v", ng)
	}
	if s := ws["/user.slice/user-1000.slice/session-3.scope"]; s.Kind != model.KindGroup {
		t.Errorf("session %+v", s)
	}
	if p := ws["proc:tini"]; !p.FromSmaps || p.Kind != model.KindGroup {
		t.Errorf("root process group %+v", p)
	}
}

// A workload's oldest process start moves later only when every process it
// had is gone, which is how the watcher tells a restart.
func TestOldestStart(t *testing.T) {
	f := fixture()
	stat := func(pid, start int) *fstest.MapFile {
		return file(fmt.Sprintf("%d (nginx: worker) S 1 1 1 0 -1 4194560 1 0 0 0 1 1 0 0 20 0 1 0 %d 1000 10\n", pid, start))
	}
	f["proc/100/stat"], f["proc/101/stat"] = stat(100, 5000), stat(101, 7000)
	c, err := New(Options{FS: f, PageSize: 4096})
	if err != nil {
		t.Fatal(err)
	}
	if err := c.Discover(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got, ok := c.OldestStart("/system.slice/nginx.service"); !ok || got != 5000 {
		t.Errorf("oldest start %d, %v; want 5000", got, ok)
	}
	if _, ok := c.OldestStart("/system.slice/missing.service"); ok {
		t.Error("a workload that isn't there has a start time")
	}
}

func TestDiscoverWithoutDocker(t *testing.T) {
	c, err := New(Options{FS: fixture(), PageSize: 4096, Containers: failingDocker{}})
	if err != nil {
		t.Fatal(err)
	}
	if err := c.Discover(context.Background()); err != nil {
		t.Fatal(err)
	}
	if w := c.Workloads()["/system.slice/docker-"+cid+".scope"]; w.Name != cid[:12] {
		t.Errorf("unnamed container %q", w.Name)
	}
	gaps := c.Gaps()
	if !slices.ContainsFunc(gaps, func(g Gap) bool { return strings.Contains(g.What, "Docker") }) {
		t.Errorf("no gap for Docker: %+v", gaps)
	}
}

func TestSample(t *testing.T) {
	c, err := New(Options{FS: fixture(), PageSize: 4096})
	if err != nil {
		t.Fatal(err)
	}
	if err := c.Discover(context.Background()); err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 10, 31, 9, 0, 0, 0, time.UTC)
	s := c.Sample(now)
	if !s.Time.Equal(now) || s.Box.MemTotal != 8000000*1024 || s.Box.OOMKills != 3 || !s.Box.HasPressure || s.Box.CPUTotal != 1165 {
		t.Errorf("box %+v", s.Box)
	}
	u := s.Workloads["/system.slice/nginx.service"]
	if u.Anon != 20<<20 || u.ActiveFile != 4<<20 || u.InactiveFile != 6<<20 || u.RefaultFile != 7 || u.OOMKills != 1 {
		t.Errorf("nginx usage %+v", u)
	}
	if !u.HasPeak || u.Peak != 60<<20 || u.PressureSome != 1500*time.Microsecond {
		t.Errorf("nginx peak %d pressure %v", u.Peak, u.PressureSome)
	}
	if u.Hard() != 20<<20+16384+8192+4096 {
		t.Errorf("hard = %d", u.Hard())
	}
	p := s.Workloads["proc:tini"]
	if p.Anon != 2048*1024 || p.ActiveFile != 1024*1024 || p.Current != 3072*1024 {
		t.Errorf("smaps group %+v", p)
	}
	if len(s.Workloads) != 4 {
		t.Errorf("%d workloads sampled", len(s.Workloads))
	}
}

func TestMachine(t *testing.T) {
	c, err := New(Options{FS: fixture(), PageSize: 4096})
	if err != nil {
		t.Fatal(err)
	}
	if err := c.Discover(context.Background()); err != nil {
		t.Fatal(err)
	}
	m := c.Machine(context.Background(), nil)
	if m.Hostname != "web-1" || m.Kernel.String() != "6.8" || m.OS != "Ubuntu 24.04.1 LTS" || m.CPUs != 2 {
		t.Errorf("identity %+v", m)
	}
	if !m.CgroupV2 || !m.MemoryCtl || !m.PSI || !m.Peak || !m.Reclaim {
		t.Errorf("features v2 %v memory %v psi %v peak %v reclaim %v", m.CgroupV2, m.MemoryCtl, m.PSI, m.Peak, m.Reclaim)
	}
	if m.THP != "madvise" || m.Overcommit != 0 || m.Swappiness != 60 || len(m.Swaps) != 0 || m.Zswap.Available {
		t.Errorf("settings %+v", m)
	}
	if m.Virt != "kvm" || m.Uptime != 864000*time.Second+120*time.Millisecond {
		t.Errorf("virt %q uptime %v", m.Virt, m.Uptime)
	}
	if want := uint64(864000 * 30.44 / 10); m.EgressPerMonth < want-1 || m.EgressPerMonth > want+1 {
		t.Errorf("egress %d, want about %d (eth0 only)", m.EgressPerMonth, want)
	}
}

func TestNoProc(t *testing.T) {
	if _, err := New(Options{FS: fstest.MapFS{}}); err == nil {
		t.Error("an empty filesystem was accepted as Linux")
	}
}

func TestNoCgroupV2(t *testing.T) {
	f := fixture()
	for name := range f {
		if strings.HasPrefix(name, "sys/fs/cgroup") {
			delete(f, name)
		}
	}
	f["proc/self/mountinfo"] = file("23 1 252:1 / / rw,relatime - ext4 /dev/vda1 rw\n")
	c, err := New(Options{FS: f, PageSize: 4096})
	if err != nil {
		t.Fatal(err)
	}
	if err := c.Discover(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(c.Workloads()) != 0 || !slices.ContainsFunc(c.Gaps(), func(g Gap) bool { return g.What == "cgroup v2" }) {
		t.Errorf("workloads %v gaps %v", c.Workloads(), c.Gaps())
	}
}

func TestDatabaseDetection(t *testing.T) {
	for in, want := range map[string]string{"postgres": "PostgreSQL", "mysqld": "MySQL", "redis-server": "Redis", "clickhouse-serv": "ClickHouse", "node": ""} {
		if got := databaseOf(in); got != want {
			t.Errorf("databaseOf(%q) = %q, want %q", in, got, want)
		}
	}
	for in, want := range map[string]string{"postgres:18": "PostgreSQL", "docker.io/library/redis:8@sha256:abc": "Redis", "ghcr.io/x/valkey": "Valkey", "nginx:1": ""} {
		if got := databaseOfImage(in); got != want {
			t.Errorf("databaseOfImage(%q) = %q, want %q", in, got, want)
		}
	}
}
