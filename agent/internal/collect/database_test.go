package collect

import (
	"context"
	"slices"
	"testing"
)

// A database's memory settings come from its main process's command line,
// and nothing else of it is kept: not other settings, not a backend's
// rewritten title.
func TestDatabaseSettings(t *testing.T) {
	f := fixture()
	cgroupFiles(f, "sys/fs/cgroup/system.slice/postgresql.service", "700\n701\n", 300<<20, 100<<20, 50<<20)
	f["proc/700/status"] = file("Name:\tpostgres\nTgid:\t700\nPPid:\t1\nUid:\t110\t110\t110\t110\nVmRSS:\t40000 kB\n")
	f["proc/700/cmdline"] = file("/usr/lib/postgresql/16/bin/postgres\x00-D\x00/var/lib/postgresql/16/main\x00-c\x00shared_buffers=256MB\x00-c\x00password_encryption=scram-sha-256\x00-c\x00work_mem=16MB\x00")
	f["proc/701/status"] = file("Name:\tpostgres\nTgid:\t701\nPPid:\t700\nUid:\t110\t110\t110\t110\nVmRSS:\t90000 kB\n")
	f["proc/701/cmdline"] = file("postgres: shop shop 10.0.0.5(51234) idle\x00")
	c, err := New(Options{FS: f, PageSize: 4096})
	if err != nil {
		t.Fatal(err)
	}
	if err := c.Discover(context.Background()); err != nil {
		t.Fatal(err)
	}
	w := c.Workloads()["/system.slice/postgresql.service"]
	if want := []string{"shared_buffers=256MB", "work_mem=16MB"}; w.Database != "PostgreSQL" || !slices.Equal(w.DBSettings, want) {
		t.Errorf("database %q, settings %q; want PostgreSQL and %q", w.Database, w.DBSettings, want)
	}
}

func TestSettingFlags(t *testing.T) {
	for _, tc := range []struct {
		db, cmdline string
		want        []string
	}{
		{"MongoDB", "mongod\x00--bind_ip_all\x00--wiredTigerCacheSizeGB\x001.5\x00", []string{"--wiredTigerCacheSizeGB 1.5"}},
		{"MySQL", "mysqld\x00--innodb-buffer-pool-size=1G\x00--user=mysql\x00", []string{"--innodb-buffer-pool-size=1G"}},
		{"MySQL", "mysqld\x00--innodb_buffer_pool_size\x00512M\x00", []string{"--innodb_buffer_pool_size 512M"}},
		{"Redis", "redis-server *:6379\x00", nil},
		{"Redis", "redis-server\x00--maxmemory\x00", nil},
	} {
		if got := settingFlags([]byte(tc.cmdline), databaseFlags[tc.db]); !slices.Equal(got, tc.want) {
			t.Errorf("%s %q: %q, want %q", tc.db, tc.cmdline, got, tc.want)
		}
	}
}
