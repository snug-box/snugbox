package kernel

import (
	"math"
	"strings"
	"testing"
)

func addTestdataSeeds(f *testing.F, names ...string) {
	f.Helper()
	for _, n := range names {
		b, err := readTestdataFile(n)
		if err != nil {
			f.Fatal(err)
		}
		f.Add(b)
	}
}

func FuzzParseMeminfo(f *testing.F) {
	addTestdataSeeds(f, "meminfo-6.18.txt")
	f.Add([]byte("MemTotal: 18014398509481983 kB\nHugePages_Total: 1\n"))
	f.Fuzz(func(t *testing.T, b []byte) {
		s, skipped := ParseMeminfo(b)
		if skipped < 0 {
			t.Fatal("negative skip count")
		}
		for k := range s {
			if strings.TrimSpace(k) != k || k == "" {
				t.Fatalf("key %q not trimmed", k)
			}
		}
	})
}

func FuzzParseStat(f *testing.F) {
	addTestdataSeeds(f, "memory.stat-6.18.txt", "vmstat-6.18.txt")
	f.Fuzz(func(t *testing.T, b []byte) {
		s, skipped := ParseStat(b)
		if skipped < 0 || len(s) > strings.Count(string(b), "\n")+1 {
			t.Fatalf("%d keys from %d lines", len(s), strings.Count(string(b), "\n")+1)
		}
	})
}

func FuzzParsePressure(f *testing.F) {
	f.Add([]byte("some avg10=0.00 avg60=0.00 avg300=0.00 total=0\nfull avg10=0.00 avg60=0.00 avg300=0.00 total=0\n"))
	f.Add([]byte("some avg10=99.99 avg60=1e2 avg300=0x1p-2 total=9223372036854775\n"))
	f.Fuzz(func(t *testing.T, b []byte) {
		p, err := ParsePressure(b)
		if err != nil {
			return
		}
		for _, l := range []PressureLine{p.Some, p.Full} {
			for _, v := range []float64{l.Avg10, l.Avg60, l.Avg300} {
				if math.IsNaN(v) || v < 0 || v > 100 {
					t.Fatalf("average %v out of range", v)
				}
			}
			if l.Total < 0 {
				t.Fatalf("negative total %v", l.Total)
			}
		}
	})
}

func FuzzParseSmapsRollup(f *testing.F) {
	addTestdataSeeds(f, "smaps_rollup-6.18.txt")
	f.Fuzz(func(t *testing.T, b []byte) {
		_, _ = ParseSmapsRollup(b)
	})
}

func FuzzParseProcStatus(f *testing.F) {
	addTestdataSeeds(f, "status-6.18.txt")
	f.Fuzz(func(t *testing.T, b []byte) {
		st, err := ParseProcStatus(b)
		if err == nil && st.Threads < 0 {
			t.Fatalf("negative thread count %d", st.Threads)
		}
	})
}

func FuzzParseProcStart(f *testing.F) {
	f.Add([]byte("1 (systemd) S 0 1 1 0 -1 4194560 5167 56131 13 139 26 27 87 58 20 0 1 0 160 22433792 3169\n"))
	f.Add([]byte("7 ()) S 0 1 1 0 -1 4 5 5 1 1 2 2 8 5 20 0 1 0 18446744073709551615 2\n"))
	f.Fuzz(func(t *testing.T, b []byte) {
		_, _ = ParseProcStart(b)
	})
}

func FuzzParseSwaps(f *testing.F) {
	addTestdataSeeds(f, "swaps.txt")
	f.Add([]byte("Filename Type Size Used Priority\n/a\\134\\040b file 18014398509481984 0 -2\n"))
	f.Fuzz(func(t *testing.T, b []byte) {
		swaps, err := ParseSwaps(b)
		if err != nil {
			return
		}
		for _, s := range swaps {
			if s.Size%1024 != 0 || s.Used%1024 != 0 {
				t.Fatalf("size not converted from KiB: %+v", s)
			}
		}
	})
}

func FuzzParseMountinfo(f *testing.F) {
	addTestdataSeeds(f, "mountinfo-wsl.txt")
	f.Fuzz(func(t *testing.T, b []byte) {
		_, _ = ParseMountinfo(b)
	})
}

func FuzzParseOSRelease(f *testing.F) {
	addTestdataSeeds(f, "os-release-ubuntu-24.04.txt")
	f.Fuzz(func(t *testing.T, b []byte) {
		_ = ParseOSRelease(b)
	})
}

func FuzzParseProcStat(f *testing.F) {
	addTestdataSeeds(f, "stat-6.18.txt")
	f.Fuzz(func(t *testing.T, b []byte) {
		_, _ = ParseProcStat(b)
	})
}

func FuzzParseNetDev(f *testing.F) {
	addTestdataSeeds(f, "net_dev.txt")
	f.Fuzz(func(t *testing.T, b []byte) {
		_, _ = ParseNetDev(b)
	})
}

func FuzzParseCPUList(f *testing.F) {
	f.Add("0-7")
	f.Add("0,2-3,65535")
	f.Fuzz(func(t *testing.T, s string) {
		n, err := ParseCPUList(s)
		if err == nil && (n <= 0 || n > maxCPUID) {
			t.Fatalf("count %d out of range", n)
		}
	})
}

func FuzzParseRelease(f *testing.F) {
	f.Add("6.18.33.2-microsoft-standard-WSL2")
	f.Add("5.4.0-216-generic")
	f.Fuzz(func(t *testing.T, s string) {
		r, err := ParseRelease(s)
		if err == nil && (r.Major < 0 || r.Minor < 0 || r.Patch < 0) {
			t.Fatalf("negative version %+v from %q", r, s)
		}
	})
}

func FuzzParseKmsg(f *testing.F) {
	f.Add([]byte("3,1042,7581210437,-;oom-kill:constraint=CONSTRAINT_MEMCG,task_memcg=/a,task=x,pid=1,uid=0\n"))
	f.Add([]byte("4,9,1,c;Memory cgroup out of memory: Killed process 1 (a)) anon-rss:1kB,\n"))
	f.Fuzz(func(t *testing.T, b []byte) {
		r, err := ParseKmsgRecord(b)
		if err != nil {
			return
		}
		if r.SinceBoot < 0 {
			t.Fatalf("negative timestamp %v", r.SinceBoot)
		}
		if k, ok := ParseOOMKill(r.Message); ok && k.PID <= 0 {
			t.Fatalf("pid %d", k.PID)
		}
		if kp, ok := ParseKilledProcess(r.Message); ok && kp.PID <= 0 {
			t.Fatalf("pid %d", kp.PID)
		}
	})
}

func FuzzUnescapeOctal(f *testing.F) {
	f.Add(`a\040b\134\0`)
	f.Fuzz(func(t *testing.T, s string) {
		if out := unescapeOctal(s); len(out) > len(s) {
			t.Fatalf("unescaped %q grew to %q", s, out)
		}
	})
}
