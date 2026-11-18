package cron

import (
	"slices"
	"testing"
	"time"
)

const system = `# /etc/crontab: system-wide crontab
SHELL=/bin/sh
PATH=/usr/local/sbin:/usr/local/bin:/sbin:/bin:/usr/sbin:/usr/bin

17 *	* * *	root	cd / && run-parts --report /etc/cron.hourly
25 6	* * *	root	test -x /usr/sbin/anacron || { cd / && run-parts --report /etc/cron.daily; }
0 2 * * * root /usr/local/bin/backup.sh --password=hunter2 > /var/log/backup.log 2>&1
@daily     postgres   pg_dump shop | gzip > /srv/dump.gz
@reboot    root       /usr/local/bin/warm-cache
30 3 * * mon-fri www-data php /srv/app/artisan schedule:run
not a line
0 4 * *
`

func TestParseSystemCrontab(t *testing.T) {
	jobs := Parse("/etc/crontab", []byte(system), true)
	type got struct {
		line          int
		spec, program string
	}
	var have []got
	for _, j := range jobs {
		if j.File != "/etc/crontab" {
			t.Errorf("file %q", j.File)
		}
		have = append(have, got{j.Line, j.Spec, j.Program})
	}
	want := []got{
		{5, "17 * * * *", "run-parts"},
		{6, "25 6 * * *", "run-parts"},
		{7, "0 2 * * *", "backup.sh"},
		{8, "@daily", "pg_dump"},
		{9, "@reboot", "warm-cache"},
		{10, "30 3 * * mon-fri", "php"},
	}
	if !slices.Equal(have, want) {
		t.Errorf("jobs:\n got %v\nwant %v", have, want)
	}
}

func TestParseUserCrontab(t *testing.T) {
	jobs := Parse("/var/spool/cron/crontabs/ana", []byte("MAILTO=ana@example.com\n*/10 * * * * /home/ana/bin/sync.sh%line one%line two\n"), false)
	if len(jobs) != 1 || jobs[0].Program != "sync.sh" || jobs[0].Spec != "*/10 * * * *" || jobs[0].Line != 2 {
		t.Fatalf("jobs %+v", jobs)
	}
}

// Times in a crontab with a zone of its own are not the box's.
func TestParseSkipsACrontabWithItsOwnZone(t *testing.T) {
	for _, zone := range []string{"CRON_TZ=Asia/Kolkata", "TZ=UTC"} {
		if jobs := Parse("/etc/cron.d/x", []byte(zone+"\n0 2 * * * root /bin/true\n"), true); jobs != nil {
			t.Errorf("%s: %+v", zone, jobs)
		}
	}
}

func TestBadFieldsAreLeftOut(t *testing.T) {
	for _, spec := range []string{"60 * * * *", "* 24 * * *", "* * 0 * *", "* * * 13 *", "* * * * 8", "5/10 * * * *", "10-5 * * * *",
		"*/0 * * * *", "+5 * * * *", "a * * * *", "* * * smarch *", "@fortnightly"} {
		if jobs := Parse("/tmp/x", []byte(spec+" /bin/true\n"), false); len(jobs) != 0 {
			t.Errorf("%q was read as %+v", spec, jobs)
		}
	}
}

func at(s string) time.Time {
	t, err := time.ParseInLocation("Mon 2006-01-02 15:04", s, time.UTC)
	if err != nil {
		panic(err)
	}
	return t
}

func TestStartsAt(t *testing.T) {
	for _, c := range []struct {
		spec, when string
		want       bool
	}{
		{"0 2 * * *", "Sat 2026-10-31 02:00", true},
		{"0 2 * * *", "Sat 2026-10-31 02:01", false},
		{"*/15 * * * *", "Sat 2026-10-31 13:45", true},
		{"*/15 * * * *", "Sat 2026-10-31 13:50", false},
		{"0 9 * * mon-fri", "Fri 2026-10-30 09:00", true},
		{"0 9 * * mon-fri", "Sat 2026-10-31 09:00", false},
		{"0 0 * * 7", "Sun 2026-11-01 00:00", true},
		{"0 0 * * 0", "Sun 2026-11-01 00:00", true},
		{"0 0 1 jan *", "Fri 2027-01-01 00:00", true},
		{"0 0 1 jan *", "Sun 2026-11-01 00:00", false},
		{"5,35 8-10/2 * * *", "Sat 2026-10-31 10:35", true},
		{"5,35 8-10/2 * * *", "Sat 2026-10-31 09:35", false},
		// Both day fields restricted: either is enough.
		{"0 0 15 * fri", "Fri 2026-10-30 00:00", true},
		{"0 0 15 * fri", "Sun 2026-11-15 00:00", true},
		{"0 0 15 * fri", "Sat 2026-10-31 00:00", false},
		// A day field that starts with "*" makes both count, as in Vixie
		// cron and cronie.
		{"0 0 */2 * fri", "Fri 2026-10-30 00:00", false},
		{"0 0 */2 * fri", "Fri 2026-11-13 00:00", true},
		{"@hourly", "Sat 2026-10-31 07:00", true},
		{"@weekly", "Sun 2026-11-01 00:00", true},
		{"@monthly", "Sun 2026-11-01 00:00", true},
		{"@reboot", "Sun 2026-11-01 00:00", false},
	} {
		jobs := Parse("/tmp/x", []byte(c.spec+" /bin/true\n"), false)
		if len(jobs) != 1 {
			t.Fatalf("%q: %d jobs", c.spec, len(jobs))
		}
		if got := jobs[0].StartsAt(at(c.when)); got != c.want {
			t.Errorf("%q at %s: %v, want %v", c.spec, c.when, got, c.want)
		}
	}
}

func TestFires(t *testing.T) {
	jobs := Parse("/tmp/x", []byte("30 2 * * * /bin/true\n"), false)
	kolkata := time.FixedZone("IST", 5*3600+1800)
	from := time.Date(2026, 10, 30, 0, 0, 30, 0, time.UTC)
	got := jobs[0].Fires(from, from.Add(48*time.Hour), kolkata)
	want := []time.Time{time.Date(2026, 10, 30, 21, 0, 0, 0, time.UTC), time.Date(2026, 10, 31, 21, 0, 0, 0, time.UTC)}
	if len(got) != len(want) {
		t.Fatalf("fires %v, want %v", got, want)
	}
	for i := range got {
		if !got[i].Equal(want[i]) {
			t.Errorf("fire %d at %v, want %v", i, got[i], want[i])
		}
	}
}

func TestLater(t *testing.T) {
	for _, c := range []struct {
		spec    string
		minutes int
		want    string
		ok      bool
	}{
		{"0 2 * * *", 25, "25 2 * * *", true},
		{"50 2 * * 1-5", 20, "10 3 * * 1-5", true},
		{"50 23 * * *", 20, "10 0 * * *", true},
		{"50 23 * * 1-5", 20, "", false},
		{"0 * * * *", 25, "25 * * * *", true},
		{"45 * * * *", 20, "", false},
		{"*/5 2 * * *", 10, "", false},
		{"0 2,14 * * *", 10, "", false},
		{"@daily", 30, "30 0 * * *", true},
		{"@reboot", 30, "", false},
	} {
		jobs := Parse("/tmp/x", []byte(c.spec+" /bin/true\n"), false)
		got, ok := jobs[0].Later(c.minutes)
		if got != c.want || ok != c.ok {
			t.Errorf("%q + %d minutes: %q %v, want %q %v", c.spec, c.minutes, got, ok, c.want, c.ok)
		}
	}
}

func TestProgram(t *testing.T) {
	for line, want := range map[string]string{
		"/usr/local/bin/backup.sh > /var/log/b.log 2>&1":                                                          "backup.sh",
		"[ -x /usr/lib/php/sessionclean ] && if [ ! -d /run/systemd/system ]; then /usr/lib/php/sessionclean; fi": "sessionclean",
		"command -v debian-sa1 > /dev/null && debian-sa1 1 1":                                                     "debian-sa1",
		"timeout 3600 nice -n 10 /opt/jobs/reindex --all":                                                         "reindex",
		"PGPASSWORD=secret pg_dump shop":                                                                          "pg_dump",
		"sh -c 'cd /srv && ./run.sh'":                                                                             "run.sh",
		"sudo -u postgres vacuumdb --all":                                                                         "vacuumdb",
		"%only input":                                                                                             "a command",
		"/usr/bin/curl -fsS https://example.com/ping\\%20me > /dev/null":                                          "curl",
		"flock -n /run/lock/sync.lock /usr/local/bin/sync-files":                                                  "sync-files",
	} {
		jobs := Parse("/tmp/x", []byte("0 1 * * * "+line+"\n"), false)
		if len(jobs) != 1 || jobs[0].Program != want {
			t.Errorf("%q: %+v, want program %q", line, jobs, want)
		}
	}
}

func FuzzParse(f *testing.F) {
	f.Add([]byte(system), true)
	f.Add([]byte("*/7 1-23/3 1,15 jan-jun mon-fri /bin/x\n@yearly y\n"), false)
	f.Fuzz(func(t *testing.T, data []byte, sys bool) {
		for _, j := range Parse("/tmp/fuzz", data, sys) {
			if j.Program == "" || j.Line < 1 {
				t.Fatalf("job %+v", j)
			}
			_ = j.StartsAt(at("Sat 2026-10-31 02:00"))
			if s, ok := j.Later(37); ok {
				if again := Parse("/tmp/again", []byte(s+" /bin/true\n"), false); len(again) != 1 {
					t.Fatalf("%q, moved from %q, does not read back", s, j.Spec)
				}
			}
		}
	})
}
