package collect

import (
	"io"
	"io/fs"
	"path"
	"regexp"
	"strings"

	"snugbox.dev/snugbox/agent/internal/cron"
)

// The system crontab, the folder of crontabs packages add, and the folder of
// users' own: Debian's cron keeps them in crontabs/, cronie straight in
// /var/spool/cron.
const (
	systemCrontab = "etc/crontab"
	cronD         = "etc/cron.d"
	debianSpool   = "var/spool/cron/crontabs"
	cronieSpool   = "var/spool/cron"
)

// debianCronName is the only kind of name Debian's cron reads in
// /etc/cron.d, the naming rule of run-parts(8).
var debianCronName = regexp.MustCompile(`^[A-Za-z0-9_-]+$`)

// CronJobs reads every crontab the box's cron daemon reads, when one is
// installed, and returns their jobs. Debian's cron reads a file in
// /etc/cron.d only when its name has nothing but letters, digits, _ and -;
// cronie skips names that start with a dot or end like a backup. A file
// larger than a unit file may be is skipped. The watcher reads users'
// crontabs through CAP_DAC_READ_SEARCH, and keeps only each job's schedule
// and program name.
func CronJobs(root fs.FS) []cron.Job {
	debian := exists(root, "usr/sbin/cron")
	if !debian && !exists(root, "usr/sbin/crond") {
		return nil
	}
	var jobs []cron.Job
	read := func(p string, system bool) {
		if data, ok := readSmall(root, p); ok {
			jobs = append(jobs, cron.Parse("/"+p, data, system)...)
		}
	}
	read(systemCrontab, true)
	for _, name := range regularFiles(root, cronD) {
		if (debian && !debianCronName.MatchString(name)) || (!debian && skippedByCronie(name)) {
			continue
		}
		read(path.Join(cronD, name), true)
	}
	users := cronieSpool
	if debian {
		users = debianSpool
	}
	for _, name := range regularFiles(root, users) {
		if !skippedByCronie(name) {
			read(path.Join(users, name), false)
		}
	}
	return jobs
}

func exists(root fs.FS, p string) bool {
	_, err := fs.Stat(root, p)
	return err == nil
}

// skippedByCronie reports a name cronie leaves out: hidden files and the
// copies editors and package managers leave behind.
func skippedByCronie(name string) bool {
	if strings.HasPrefix(name, ".") || strings.HasSuffix(name, "~") || strings.HasPrefix(name, "#") {
		return true
	}
	for _, s := range []string{".rpmsave", ".rpmorig", ".rpmnew", ".dpkg-old", ".dpkg-dist", ".dpkg-new", ".swp"} {
		if strings.HasSuffix(name, s) {
			return true
		}
	}
	return false
}

// regularFiles lists the regular files in dir, without following links.
func regularFiles(root fs.FS, dir string) []string {
	entries, err := fs.ReadDir(root, dir)
	if err != nil {
		return nil
	}
	var out []string
	for _, e := range entries {
		if e.Type().IsRegular() {
			out = append(out, e.Name())
		}
	}
	return out
}

func readSmall(root fs.FS, p string) ([]byte, bool) {
	f, err := root.Open(p)
	if err != nil {
		return nil, false
	}
	defer f.Close()
	data, err := io.ReadAll(io.LimitReader(f, maxUnitFile+1))
	if err != nil || len(data) > maxUnitFile {
		return nil, false
	}
	return data, true
}
