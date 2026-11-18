package doctor

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"strings"
	"testing"
	"testing/fstest"

	"snugbox.dev/snugbox/agent/internal/collect"
	"snugbox.dev/snugbox/agent/internal/docker"
	"snugbox.dev/snugbox/agent/internal/kernel"
)

// healthy is a box with everything Snugbox reads.
func healthy() Inputs {
	return Inputs{
		FS: fstest.MapFS{"run/systemd/system": &fstest.MapFile{Mode: fs.ModeDir | 0o755}},
		Machine: collect.Machine{
			Kernel:    kernel.Release{Major: 6, Minor: 8},
			CgroupV2:  true,
			MemoryCtl: true,
			PSI:       true,
			Swaps:     []kernel.Swap{{Filename: "/swapfile", Size: 2 << 30}},
			Zswap:     collect.Zswap{Available: true, Enabled: true, Compressor: "zstd"},
			Virt:      "kvm",
			OOMDaemon: "systemd-oomd",
		},
		Root:       true,
		CgroupRoot: "sys/fs/cgroup",
		Readable:   func(string) bool { return true },
		Docker:     func(context.Context) (string, error) { return "29.8.0", nil },
	}
}

func TestHealthyBox(t *testing.T) {
	checks := Run(context.Background(), healthy())
	want := []Check{
		{Pass, "Linux 6.8."},
		{Pass, "cgroup v2 at /sys/fs/cgroup with the memory controller on."},
		{Pass, "Pressure stall information is on."},
		{Info, "Swap: 2 GiB in 1 area."},
		{Info, "zswap is on, with zstd."},
		{Pass, "Virtual machine (kvm) with its own kernel."},
		{Pass, "systemd is the init system."},
		{Pass, "Early out-of-memory protection is on: systemd-oomd."},
		{Pass, "Running as root."},
		{Pass, "Process memory details are readable."},
		{Pass, "The kernel log is readable, so kill times are known."},
		{Pass, "Docker Engine 29.8.0 answers, so containers get their names."},
	}
	if len(checks) != len(want) {
		t.Fatalf("%d checks, want %d:\n%s", len(checks), len(want), dump(checks))
	}
	for i := range want {
		if checks[i] != want[i] {
			t.Errorf("check %d = %v %q, want %v %q", i, checks[i].Status, checks[i].Text, want[i].Status, want[i].Text)
		}
	}
}

func TestEachProblemIsNamed(t *testing.T) {
	cases := []struct {
		name   string
		change func(*Inputs)
		status Status
		text   string
	}{
		{"unreadable kernel version", func(in *Inputs) { in.Machine.Kernel = kernel.Release{} }, Fail, "Couldn't read the kernel version"},
		{"kernel too old", func(in *Inputs) { in.Machine.Kernel = kernel.Release{Major: 5, Minor: 3} }, Fail, "Linux 5.3 is older than 5.4"},
		{"kernel without memory.peak", func(in *Inputs) { in.Machine.Kernel = kernel.Release{Major: 5, Minor: 15} }, Warn, "memory.peak and active probing need 5.19"},
		{"cgroup v1", func(in *Inputs) { in.Machine.CgroupV2 = false }, Fail, "No cgroup v2."},
		{"memory controller off", func(in *Inputs) { in.Machine.MemoryCtl = false }, Fail, "the memory controller is off"},
		{"no pressure files", func(in *Inputs) { in.Machine.PSI = false }, Warn, "Couldn't read /proc/pressure/memory"},
		{"no swap", func(in *Inputs) { in.Machine.Swaps = nil }, Info, "No swap."},
		{"two swap areas", func(in *Inputs) {
			in.Machine.Swaps = []kernel.Swap{{Size: 1 << 30}, {Size: 512 << 20}}
		}, Info, "Swap: 1.5 GiB in 2 areas."},
		{"no zswap in the kernel", func(in *Inputs) { in.Machine.Zswap = collect.Zswap{} }, Info, "zswap is not built into this kernel"},
		{"zswap off", func(in *Inputs) { in.Machine.Zswap.Enabled = false }, Info, "zswap is available and off."},
		{"unknown machine", func(in *Inputs) { in.Machine.Virt = "" }, Info, "Couldn't tell what kind of machine this is."},
		{"WSL", func(in *Inputs) { in.Machine.Virt = "wsl" }, Warn, "This is WSL."},
		{"container", func(in *Inputs) { in.Machine.Virt = "lxc" }, Warn, "This box is a container (lxc): the kernel belongs to the host"},
		{"bare metal", func(in *Inputs) { in.Machine.Virt = "none" }, Pass, "Bare metal."},
		{"no systemd", func(in *Inputs) { in.FS = fstest.MapFS{} }, Warn, "systemd is not running."},
		{"no early OOM protection", func(in *Inputs) { in.Machine.OOMDaemon = "" }, Info, "No early out-of-memory protection is running."},
		{"not root", func(in *Inputs) { in.Root = false }, Warn, "Not running as root."},
		{"process details unreadable", func(in *Inputs) {
			in.Readable = func(p string) bool { return p != "/proc/1/smaps_rollup" }
		}, Warn, "Couldn't read /proc/1/smaps_rollup"},
		{"kernel log unreadable", func(in *Inputs) {
			in.Readable = func(p string) bool { return p != "/dev/kmsg" }
		}, Warn, "Couldn't read /dev/kmsg"},
		{"no Docker", func(in *Inputs) { in.Docker = nil }, Info, "No Docker socket at /var/run/docker.sock."},
		{"Docker too old", func(in *Inputs) {
			in.Docker = func(context.Context) (string, error) {
				return "20.10.24", fmt.Errorf("%w: engine 20.10.24 speaks API 1.12 to 1.41", docker.ErrUnsupported)
			}
		}, Warn, "Docker engine is older than 25.0"},
		{"Docker not answering", func(in *Inputs) {
			in.Docker = func(context.Context) (string, error) { return "", errors.New("connection reset") }
		}, Warn, "Docker didn't answer (connection reset)"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			in := healthy()
			c.change(&in)
			checks := Run(context.Background(), in)
			for _, got := range checks {
				if strings.Contains(got.Text, c.text) {
					if got.Status != c.status {
						t.Errorf("%q is %v, want %v", got.Text, got.Status, c.status)
					}
					return
				}
			}
			t.Errorf("no check says %q:\n%s", c.text, dump(checks))
		})
	}
}

func TestChecksThatNeedAnInputAreLeftOut(t *testing.T) {
	in := healthy()
	in.FS, in.Readable = nil, nil
	for _, c := range Run(context.Background(), in) {
		for _, s := range []string{"systemd", "readable", "Couldn't read /"} {
			if strings.Contains(c.Text, s) && !strings.Contains(c.Text, "systemd-oomd") {
				t.Errorf("without its input, a check still says %q", c.Text)
			}
		}
	}
}

func TestStatusNames(t *testing.T) {
	for s, want := range map[Status]string{Pass: "ok", Info: "info", Warn: "warn", Fail: "fail"} {
		if s.String() != want {
			t.Errorf("%d.String() = %q, want %q", s, s.String(), want)
		}
	}
	if upperFirst("") != "" || upperFirst("docker") != "Docker" {
		t.Error("upperFirst")
	}
}

func dump(checks []Check) string {
	var b strings.Builder
	for _, c := range checks {
		fmt.Fprintf(&b, "  %-4v %s\n", c.Status, c.Text)
	}
	return b.String()
}
