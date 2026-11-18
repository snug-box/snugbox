package kernel

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"
)

func readTestdataFile(name string) ([]byte, error) {
	return os.ReadFile(filepath.Join("testdata", name))
}

func readTestdata(t *testing.T, name string) []byte {
	t.Helper()
	b, err := readTestdataFile(name)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func TestParseMeminfo(t *testing.T) {
	s, skipped := ParseMeminfo(readTestdata(t, "meminfo-6.18.txt"))
	if skipped != 0 {
		t.Errorf("skipped %d lines of a real meminfo", skipped)
	}
	want := map[string]uint64{
		"MemTotal":        12156536 * 1024,
		"Active(file)":    115488 * 1024,
		"SUnreclaim":      49552 * 1024,
		"VmallocTotal":    34359738367 * 1024,
		"HugePages_Total": 0,
		"Hugepagesize":    2048 * 1024,
	}
	for k, v := range want {
		if got, ok := s[k]; !ok || got != v {
			t.Errorf("%s = %d (present %v), want %d", k, got, ok, v)
		}
	}
	if s.Has("Zswap") {
		t.Error("Zswap reported on a kernel built without it")
	}
}

func TestParseMeminfoSkipsBadLines(t *testing.T) {
	in := "MemTotal: 100 kB\nweird line\nMemFree: 12 MB\nBig: 18446744073709551615 kB\n: 5\nHugePages_Free: 3\n"
	s, skipped := ParseMeminfo([]byte(in))
	if skipped != 4 {
		t.Errorf("skipped = %d, want 4", skipped)
	}
	if s.Get("MemTotal") != 102400 || s.Get("HugePages_Free") != 3 {
		t.Errorf("got %v", s)
	}
}

func TestParseStat(t *testing.T) {
	s, skipped := ParseStat(readTestdata(t, "memory.stat-6.18.txt"))
	if skipped != 0 {
		t.Errorf("skipped %d lines", skipped)
	}
	for k, v := range map[string]uint64{"anon": 262144, "file": 86016, "shmem": 4096, "active_file": 8192, "slab_unreclaimable": 28024, "workingset_refault_file": 0} {
		if s.Get(k) != v || !s.Has(k) {
			t.Errorf("%s = %d, want %d", k, s.Get(k), v)
		}
	}
	vm, skipped := ParseStat(readTestdata(t, "vmstat-6.18.txt"))
	if skipped != 0 || !vm.Has("oom_kill") || !vm.Has("pswpin") {
		t.Errorf("vmstat: skipped %d, oom_kill %v, pswpin %v", skipped, vm.Has("oom_kill"), vm.Has("pswpin"))
	}
	_, skipped = ParseStat([]byte("a 1\nb -2\nc\nd 3 4\n\n"))
	if skipped != 3 {
		t.Errorf("skipped = %d, want 3", skipped)
	}
}

func TestParsePressure(t *testing.T) {
	p, err := ParsePressure([]byte("some avg10=1.50 avg60=0.25 avg300=0.00 total=1234567\nfull avg10=0.10 avg60=0.00 avg300=0.00 total=1000\n"))
	if err != nil {
		t.Fatal(err)
	}
	want := Pressure{
		Some:    PressureLine{Avg10: 1.5, Avg60: 0.25, Total: 1234567 * time.Microsecond},
		Full:    PressureLine{Avg10: 0.1, Total: time.Millisecond},
		HasFull: true,
	}
	if p != want {
		t.Errorf("got %+v, want %+v", p, want)
	}
	for _, bad := range []string{
		"",
		"full avg10=0.00 avg60=0.00 avg300=0.00 total=0\n",
		"some avg10=0.00 avg60=0.00 total=0\n",
		"some avg10=NaN avg60=0.00 avg300=0.00 total=0\n",
		"some avg10=101 avg60=0.00 avg300=0.00 total=0\n",
		"some avg10=0.00 avg60=0.00 avg300=0.00 total=99999999999999999999\n",
		"some avg10 avg60=0.00 avg300=0.00 total=0\n",
	} {
		if _, err := ParsePressure([]byte(bad)); err == nil {
			t.Errorf("ParsePressure(%q) succeeded, want an error", bad)
		}
	}
}

func TestParseLimitAndUint(t *testing.T) {
	l, err := ParseLimit([]byte("max\n"))
	if err != nil || !l.Unlimited {
		t.Errorf("max: %+v, %v", l, err)
	}
	l, err = ParseLimit([]byte("536870912\n"))
	if err != nil || l.Unlimited || l.Bytes != 536870912 {
		t.Errorf("number: %+v, %v", l, err)
	}
	if _, err := ParseLimit([]byte("-1")); err == nil {
		t.Error("negative limit accepted")
	}
	n, err := ParseUint([]byte("1572864\n"))
	if err != nil || n != 1572864 {
		t.Errorf("ParseUint = %d, %v", n, err)
	}
	if _, err := ParseUint([]byte("max")); err == nil {
		t.Error(`ParseUint accepted "max"`)
	}
}

func TestParseChoice(t *testing.T) {
	for in, want := range map[string]string{
		"always [madvise] never\n":                     "madvise",
		"always defer defer+madvise [madvise] never\n": "madvise",
		"[always] madvise never":                       "always",
	} {
		got, err := ParseChoice([]byte(in))
		if err != nil || got != want {
			t.Errorf("ParseChoice(%q) = %q, %v; want %q", in, got, err, want)
		}
	}
	for _, bad := range []string{"always madvise never", "[]", ""} {
		if _, err := ParseChoice([]byte(bad)); err == nil {
			t.Errorf("ParseChoice(%q) succeeded", bad)
		}
	}
}

func TestParseSmapsRollup(t *testing.T) {
	r, err := ParseSmapsRollup(readTestdata(t, "smaps_rollup-6.18.txt"))
	if err != nil {
		t.Fatal(err)
	}
	want := SmapsRollup{Rss: 13176 * 1024, Pss: 5337 * 1024, PssAnon: 3380 * 1024, PssFile: 1957 * 1024, Anonymous: 3380 * 1024, HasPssSplit: true}
	if r != want {
		t.Errorf("got %+v, want %+v", r, want)
	}
	old := "00400000-7fff0000 ---p 00000000 00:00 0 [rollup]\nRss: 100 kB\nPss: 50 kB\nAnonymous: 40 kB\nSwap: 0 kB\n"
	r, err = ParseSmapsRollup([]byte(old))
	if err != nil || r.HasPssSplit || r.Pss != 51200 {
		t.Errorf("5.4-style rollup: %+v, %v", r, err)
	}
	if _, err := ParseSmapsRollup([]byte("Rss: lots kB\n")); err == nil {
		t.Error("bad size accepted")
	}
}

// The first line is /proc/1/stat from the WSL 6.18 test kernel; the second
// has a command name with spaces and a closing parenthesis.
func TestParseProcStart(t *testing.T) {
	cases := map[string]uint64{
		"1 (systemd) S 0 1 1 0 -1 4194560 5167 56131 13 139 26 27 87 58 20 0 1 0 160 22433792 3169 18446744073709551615 1 1 0 0 0 0 671173123 4096 1260 0 0 0 17 4 0 0 0 0 0 0 0 0 0 0 0 0 0\n": 160,
		"4242 (web (worker) 2) R 1 4242 4242 0 -1 4194304 10 0 0 0 1 2 0 0 20 0 9 0 98765 1000 10 18446744073709551615\n":                                                                       98765,
	}
	for line, want := range cases {
		got, err := ParseProcStart([]byte(line))
		if err != nil || got != want {
			t.Errorf("ParseProcStart(%.30q) = %d, %v; want %d", line, got, err, want)
		}
	}
	for _, bad := range []string{"", "1 systemd S 0", "1 (x) S 0 1 2", "1 (x) S 0 1 1 0 -1 4194560 5167 56131 13 139 26 27 87 58 20 0 1 0 -5 1"} {
		if _, err := ParseProcStart([]byte(bad)); err == nil {
			t.Errorf("ParseProcStart(%q) accepted", bad)
		}
	}
}

func TestParseProcStatus(t *testing.T) {
	st, err := ParseProcStatus(readTestdata(t, "status-6.18.txt"))
	if err != nil {
		t.Fatal(err)
	}
	want := ProcStatus{Name: "systemd", Tgid: 1, VmRSS: 13176 * 1024, RssAnon: 3380 * 1024, RssFile: 9796 * 1024, Threads: 1}
	if st != want {
		t.Errorf("got %+v, want %+v", st, want)
	}
	if _, err := ParseProcStatus([]byte("Name:\tx\nThreads:\t-3\n")); err == nil {
		t.Error("a negative thread count was accepted")
	}
	kthread := "Name:\tkworker/0:1\nTgid:\t12\nPPid:\t2\nUid:\t0\t0\t0\t0\nThreads:\t1\n"
	st, err = ParseProcStatus([]byte(kthread))
	if err != nil || !st.Kernel || st.PPid != 2 {
		t.Errorf("kernel thread: %+v, %v", st, err)
	}
	if _, err := ParseProcStatus([]byte("Tgid:\t1\n")); err == nil {
		t.Error("status without Name accepted")
	}
}

func TestParseProcCgroup(t *testing.T) {
	cases := map[string]struct {
		path string
		ok   bool
	}{
		"0::/init.scope\n": {"/init.scope", true},
		"12:memory:/user.slice\n1:name=systemd:/user.slice/x\n0::/user.slice/user-1000.slice/session-3.scope\n": {"/user.slice/user-1000.slice/session-3.scope", true},
		"12:memory:/docker/abc\n": {"", false},
		"0::relative\n":           {"", false},
	}
	for in, want := range cases {
		path, ok := ParseProcCgroup([]byte(in))
		if path != want.path || ok != want.ok {
			t.Errorf("ParseProcCgroup(%q) = %q, %v; want %q, %v", in, path, ok, want.path, want.ok)
		}
	}
}

func TestParseSwaps(t *testing.T) {
	got, err := ParseSwaps(readTestdata(t, "swaps.txt"))
	if err != nil {
		t.Fatal(err)
	}
	want := []Swap{{Filename: "/dev/sdc", Type: "partition", Size: 3145728 * 1024, Priority: -2}}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("got %+v, want %+v", got, want)
	}
	got, err = ParseSwaps([]byte("Filename Type Size Used Priority\n/var/lib/my\\040swap file 1024 512 -3\n"))
	if err != nil || len(got) != 1 || got[0].Filename != "/var/lib/my swap" || got[0].Used != 512*1024 {
		t.Errorf("escaped name: %+v, %v", got, err)
	}
	empty, err := ParseSwaps([]byte("Filename\t\t\t\tType\t\tSize\t\tUsed\t\tPriority\n"))
	if err != nil || len(empty) != 0 {
		t.Errorf("no swap: %+v, %v", empty, err)
	}
	if _, err := ParseSwaps([]byte("Filename Type Size Used Priority\n/swap file x 0 -2\n")); err == nil {
		t.Error("bad size accepted")
	}
}

func TestUnescapeOctal(t *testing.T) {
	for in, want := range map[string]string{
		`plain`:       "plain",
		`a\040b`:      "a b",
		`C:\134`:      `C:\`,
		`tail\04`:     `tail\04`,
		`high\477`:    `high\477`,
		`\011\012\\x`: "\t\n\\\\x",
	} {
		if got := unescapeOctal(in); got != want {
			t.Errorf("unescapeOctal(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestParseRelease(t *testing.T) {
	cases := map[string]Release{
		"6.18.33.2-microsoft-standard-WSL2": {6, 18, 33, "6.18.33.2-microsoft-standard-WSL2"},
		"5.4.0-216-generic\n":               {5, 4, 0, "5.4.0-216-generic"},
		"6.12":                              {6, 12, 0, "6.12"},
		"5.14.0-503.el9.x86_64":             {5, 14, 0, "5.14.0-503.el9.x86_64"},
	}
	for in, want := range cases {
		got, err := ParseRelease(in)
		if err != nil || got != want {
			t.Errorf("ParseRelease(%q) = %+v, %v; want %+v", in, got, err, want)
		}
	}
	for _, bad := range []string{"", "6", "x.y", "linux-6.1"} {
		if _, err := ParseRelease(bad); err == nil {
			t.Errorf("ParseRelease(%q) succeeded", bad)
		}
	}
	r := Release{Major: 5, Minor: 19}
	if !r.AtLeast(5, 19) || !r.AtLeast(4, 20) || r.AtLeast(6, 8) || r.AtLeast(5, 20) {
		t.Errorf("AtLeast is wrong for %v", r)
	}
}

func TestParseOSRelease(t *testing.T) {
	m := ParseOSRelease(readTestdata(t, "os-release-ubuntu-24.04.txt"))
	if m["ID"] != "ubuntu" || m["VERSION_ID"] != "24.04" || m["PRETTY_NAME"] != "Ubuntu 24.04.5 LTS" {
		t.Errorf("got %v", m)
	}
	m = ParseOSRelease([]byte("# comment\nNAME='Alma Linux'\nVERSION=\"9.4 \\\"Seafoam\\\"\"\nBROKEN\n"))
	if m["NAME"] != "Alma Linux" || m["VERSION"] != `9.4 "Seafoam"` || len(m) != 2 {
		t.Errorf("got %v", m)
	}
}

func TestParseMountinfo(t *testing.T) {
	mounts, err := ParseMountinfo(readTestdata(t, "mountinfo-wsl.txt"))
	if err != nil {
		t.Fatal(err)
	}
	var cg, root, win *Mount
	for i := range mounts {
		switch mounts[i].MountPoint {
		case "/sys/fs/cgroup":
			cg = &mounts[i]
		case "/":
			root = &mounts[i]
		case "/mnt/c":
			win = &mounts[i]
		}
	}
	if cg == nil || cg.FSType != "cgroup2" {
		t.Errorf("cgroup mount: %+v", cg)
	}
	if root == nil || root.FSType != "ext4" || root.Source != "/dev/sde" {
		t.Errorf("root mount: %+v", root)
	}
	if win == nil || win.Source != `C:\` {
		t.Errorf("escaped source: %+v", win)
	}
	if _, err := ParseMountinfo([]byte("1 2 3:4 / /x rw\n")); err == nil {
		t.Error("short line accepted")
	}
}

func TestParseProcStat(t *testing.T) {
	ps, err := ParseProcStat(readTestdata(t, "stat-6.18.txt"))
	if err != nil {
		t.Fatal(err)
	}
	if ps.BootTime.IsZero() || ps.CPU.Total() == 0 || ps.CPU.Busy() > ps.CPU.Total() {
		t.Errorf("got %+v", ps)
	}
	ps, err = ParseProcStat([]byte("cpu  10 1 5 100 4 2 3 7 0 0\nbtime 1793417075\n"))
	if err != nil {
		t.Fatal(err)
	}
	if ps.CPU.Busy() != 28 || ps.CPU.Total() != 132 || ps.BootTime.Unix() != 1793417075 {
		t.Errorf("busy %d total %d boot %v", ps.CPU.Busy(), ps.CPU.Total(), ps.BootTime)
	}
	if _, err := ParseProcStat([]byte("cpu 1 2 3\nbtime 1\n")); err == nil {
		t.Error("short cpu line accepted")
	}
}

func TestParseNetDev(t *testing.T) {
	m, err := ParseNetDev(readTestdata(t, "net_dev.txt"))
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := m["lo"]; !ok {
		t.Errorf("lo missing from %v", m)
	}
	if eth, ok := m["eth0"]; !ok || eth.RxBytes == 0 || eth.TxBytes == 0 {
		t.Errorf("eth0: %+v", eth)
	}
}

func TestParseCPUList(t *testing.T) {
	for in, want := range map[string]int{"0-7\n": 8, "0": 1, "0-3,6,8-9": 7} {
		got, err := ParseCPUList(in)
		if err != nil || got != want {
			t.Errorf("ParseCPUList(%q) = %d, %v; want %d", in, got, err, want)
		}
	}
	for _, bad := range []string{"", "3-1", "a", "0-9223372036854775807", "-1"} {
		if _, err := ParseCPUList(bad); err == nil {
			t.Errorf("ParseCPUList(%q) succeeded", bad)
		}
	}
}

func TestParseKmsg(t *testing.T) {
	r, err := ParseKmsgRecord([]byte("3,1042,7581210437,-;oom-kill:constraint=CONSTRAINT_MEMCG,nodemask=(null),cpuset=/,mems_allowed=0,oom_memcg=/system.slice/hog.service,task_memcg=/system.slice/hog.service,task=stress-ng-vm,pid=4242,uid=0\n SUBSYSTEM=memory\n"))
	if err != nil {
		t.Fatal(err)
	}
	if r.Priority != 3 || r.Seq != 1042 || r.SinceBoot != 7581210437*time.Microsecond {
		t.Errorf("record %+v", r)
	}
	k, ok := ParseOOMKill(r.Message)
	want := OOMKill{PID: 4242, Task: "stress-ng-vm", Memcg: "/system.slice/hog.service", Constraint: "CONSTRAINT_MEMCG"}
	if !ok || k != want {
		t.Errorf("ParseOOMKill = %+v, %v; want %+v", k, ok, want)
	}
	kp, ok := ParseKilledProcess("Memory cgroup out of memory: Killed process 4242 (stress-ng-vm) total-vm:1086356kB, anon-rss:1048576kB, file-rss:1280kB, shmem-rss:0kB, UID:0 pgtables:2108kB oom_score_adj:1000")
	if !ok || kp.PID != 4242 || kp.Task != "stress-ng-vm" || kp.AnonRSS != 1048576*1024 {
		t.Errorf("cgroup kill: %+v, %v", kp, ok)
	}
	kp, ok = ParseKilledProcess("Out of memory: Killed process 77 (java (main)) total-vm:10kB, anon-rss:8kB, file-rss:0kB")
	if !ok || kp.Task != "java (main)" || kp.AnonRSS != 8192 {
		t.Errorf("global kill: %+v, %v", kp, ok)
	}
	for _, bad := range []string{"no separator", "a,b;msg", "6,1;msg", "6,1,-5,-;msg"} {
		if _, err := ParseKmsgRecord([]byte(bad)); err == nil {
			t.Errorf("ParseKmsgRecord(%q) succeeded", bad)
		}
	}
	if _, ok := ParseOOMKill("oom-kill:constraint=CONSTRAINT_NONE,task=x"); ok {
		t.Error("summary without pid accepted")
	}
	if _, ok := ParseKilledProcess("Out of memory: Killed process x (y)"); ok {
		t.Error("non-numeric pid accepted")
	}
}
