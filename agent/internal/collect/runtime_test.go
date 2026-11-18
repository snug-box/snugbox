package collect

import (
	"context"
	"fmt"
	"os"
	"testing"
	"testing/fstest"
	"time"
)

// A service or a container whose programs name no runtime is a Go program
// when its executable's build information says so. This test binary is a Go
// program, so it stands in for one; a user's own services are never read.
func TestGoPrograms(t *testing.T) {
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	goBinary, err := os.ReadFile(self)
	if err != nil {
		t.Fatal(err)
	}
	f := fixture()
	f["proc/100/exe"] = &fstest.MapFile{Data: []byte("#!/bin/sh\nexec nginx\n")}
	f["proc/101/exe"] = &fstest.MapFile{Data: goBinary}
	cgroupFiles(f, "sys/fs/cgroup/system.slice/caddy.service", "400\n", 30<<20, 2<<20, 1<<20)
	f["proc/400/status"] = status("caddy", 30000)
	f["proc/400/exe"] = &fstest.MapFile{Data: goBinary, ModTime: time.Unix(1793000000, 0)}
	cgroupFiles(f, "sys/fs/cgroup/user.slice/user-1000.slice/user@1000.service/app.slice/tool.service", "500\n", 30<<20, 2<<20, 1<<20)
	f["proc/500/status"] = status("tool", 30000)
	f["proc/500/exe"] = &fstest.MapFile{Data: goBinary}

	c, err := New(Options{FS: f, PageSize: 4096})
	if err != nil {
		t.Fatal(err)
	}
	if err := c.Discover(context.Background()); err != nil {
		t.Fatal(err)
	}
	ws := c.Workloads()
	if got := ws["/system.slice/caddy.service"].Runtime; got != "go" {
		t.Errorf("caddy.service runtime %q, want go", got)
	}
	// nginx's two processes are the same size, so the first is read: a shell
	// script, not Go. The other is never read.
	if got := ws["/system.slice/nginx.service"].Runtime; got != "" {
		t.Errorf("nginx.service runtime %q, want none", got)
	}
	if got := ws["/user.slice/user-1000.slice/user@1000.service"].Runtime; got != "" {
		t.Errorf("a user's own service has runtime %q; its executable must not be read", got)
	}
	if got := ws["/system.slice/docker-"+cid+".scope"].Runtime; got != "node" {
		t.Errorf("the container's runtime %q: a runtime named by its program wins", got)
	}

	// The answer is kept while the process and its file stay the same, and
	// forgotten once the process is gone.
	junk := make([]byte, len(goBinary))
	copy(junk, "not a program any more")
	f["proc/400/exe"] = &fstest.MapFile{Data: junk, ModTime: time.Unix(1793000000, 0)}
	if err := c.Discover(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := c.Workloads()["/system.slice/caddy.service"].Runtime; got != "go" {
		t.Errorf("after a second look at the same file, runtime %q", got)
	}
	f["proc/400/exe"].ModTime = time.Unix(1793000600, 0)
	if err := c.Discover(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := c.Workloads()["/system.slice/caddy.service"].Runtime; got != "" {
		t.Errorf("a replaced executable was not read again: runtime %q", got)
	}
	delete(f, "proc/400/exe")
	if err := c.Discover(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, ok := c.goExes[400]; ok {
		t.Error("an executable that can't be read any more is still remembered")
	}
}

func TestBuiltWithGoSurvivesGarbage(t *testing.T) {
	c := &Collector{fsys: fstest.MapFS{
		"elf":   &fstest.MapFile{Data: []byte("\x7fELF\x02\x01\x01\x00garbage that claims to be ELF")},
		"empty": &fstest.MapFile{},
	}}
	for _, name := range []string{"elf", "empty", "missing"} {
		if c.builtWithGo(name) {
			t.Errorf("%s counted as a Go program", name)
		}
	}
}

func smapsPss(pss uint64) *fstest.MapFile {
	return file(fmt.Sprintf("0-1 ---p 0 00:00 0 [rollup]\nRss: %d kB\nPss: %d kB\nPss_Anon: %d kB\nPss_File: 0 kB\nPss_Shmem: 0 kB\n", pss+1024, pss, pss))
}

// PHP-FPM's workers are the php-fpm processes whose parent is the master,
// and the largest one seen is kept, so a quiet moment can't shrink it.
func TestPHPFPMWorkers(t *testing.T) {
	f := fixture()
	dir := "sys/fs/cgroup/system.slice/php8.3-fpm.service"
	cgroupFiles(f, dir, "600\n601\n602\n", 120<<20, 20<<20, 10<<20)
	f["proc/600/status"] = file("Name:\tphp-fpm8.3\nTgid:\t600\nPPid:\t1\nUid:\t0\t0\t0\t0\nVmRSS:\t30000 kB\n")
	f["proc/601/status"] = file("Name:\tphp-fpm8.3\nTgid:\t601\nPPid:\t600\nUid:\t33\t33\t33\t33\nVmRSS:\t60000 kB\n")
	f["proc/602/status"] = file("Name:\tphp-fpm8.3\nTgid:\t602\nPPid:\t600\nUid:\t33\t33\t33\t33\nVmRSS:\t70000 kB\n")
	f["proc/600/smaps_rollup"] = smapsPss(20 << 10)
	f["proc/601/smaps_rollup"] = smapsPss(40 << 10)
	f["proc/602/smaps_rollup"] = smapsPss(52 << 10)
	c, err := New(Options{FS: f, PageSize: 4096})
	if err != nil {
		t.Fatal(err)
	}
	if err := c.Discover(context.Background()); err != nil {
		t.Fatal(err)
	}
	w := c.Workloads()["/system.slice/php8.3-fpm.service"]
	if w.Runtime != "php-fpm" || w.Workers != 2 || w.WorkerBytes != 52<<20 {
		t.Errorf("runtime %q, %d workers, largest %d MiB; want php-fpm, 2 and 52 MiB, the master not counted", w.Runtime, w.Workers, w.WorkerBytes>>20)
	}
	f["proc/602/smaps_rollup"] = smapsPss(30 << 10)
	if err := c.Discover(context.Background()); err != nil {
		t.Fatal(err)
	}
	if w := c.Workloads()["/system.slice/php8.3-fpm.service"]; w.WorkerBytes != 52<<20 {
		t.Errorf("after the workers shrank, largest %d MiB; the largest seen must stay", w.WorkerBytes>>20)
	}
}
