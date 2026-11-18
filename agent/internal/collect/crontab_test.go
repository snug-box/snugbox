package collect

import (
	"slices"
	"strings"
	"testing"
	"testing/fstest"
)

func cronFiles(daemon string, files map[string]string) fstest.MapFS {
	m := fstest.MapFS{}
	if daemon != "" {
		m[daemon] = &fstest.MapFile{Data: []byte{}}
	}
	for p, s := range files {
		m[p] = &fstest.MapFile{Data: []byte(s)}
	}
	return m
}

func places(t *testing.T, root fstest.MapFS) []string {
	t.Helper()
	var out []string
	for _, j := range CronJobs(root) {
		out = append(out, j.File+":"+j.Program)
	}
	slices.Sort(out)
	return out
}

var crontabs = map[string]string{
	"etc/crontab":                  "SHELL=/bin/sh\n17 * * * * root cd / && run-parts --report /etc/cron.hourly\n",
	"etc/cron.d/backup":            "0 2 * * * root /usr/local/bin/backup.sh\n",
	"etc/cron.d/backup.dpkg-old":   "0 2 * * * root /usr/local/bin/old-backup.sh\n",
	"etc/cron.d/my.job":            "0 3 * * * root /usr/local/bin/dotted\n",
	"etc/cron.d/.hidden":           "0 4 * * * root /usr/local/bin/hidden\n",
	"var/spool/cron/crontabs/ana":  "30 1 * * * /home/ana/bin/sync.sh\n",
	"var/spool/cron/crontabs/ana~": "30 1 * * * /home/ana/bin/editor-copy\n",
	"var/spool/cron/root":          "45 1 * * * /root/bin/cleanup\n",
}

// Debian's cron reads only run-parts names in /etc/cron.d, so a file with
// a dot in its name is not a crontab there, and keeps users' crontabs in
// /var/spool/cron/crontabs.
func TestCronJobsOnDebian(t *testing.T) {
	got := places(t, cronFiles("usr/sbin/cron", crontabs))
	want := []string{"/etc/cron.d/backup:backup.sh", "/etc/crontab:run-parts", "/var/spool/cron/crontabs/ana:sync.sh"}
	if !slices.Equal(got, want) {
		t.Errorf("jobs\n got %v\nwant %v", got, want)
	}
}

// cronie reads any name but hidden files and backup copies, and keeps
// users' crontabs in /var/spool/cron itself.
func TestCronJobsWithCronie(t *testing.T) {
	got := places(t, cronFiles("usr/sbin/crond", crontabs))
	want := []string{"/etc/cron.d/backup:backup.sh", "/etc/cron.d/my.job:dotted", "/etc/crontab:run-parts", "/var/spool/cron/root:cleanup"}
	if !slices.Equal(got, want) {
		t.Errorf("jobs\n got %v\nwant %v", got, want)
	}
}

func TestCronJobsNeedACronDaemon(t *testing.T) {
	if got := places(t, cronFiles("", crontabs)); len(got) != 0 {
		t.Errorf("jobs without cron installed: %v", got)
	}
}

func TestCronJobsSkipAHugeFile(t *testing.T) {
	huge := strings.Repeat("0 2 * * * root /bin/true\n", maxUnitFile/20)
	if got := places(t, cronFiles("usr/sbin/cron", map[string]string{"etc/crontab": huge})); len(got) != 0 {
		t.Errorf("%d jobs read from a %d-byte crontab", len(got), len(huge))
	}
}
