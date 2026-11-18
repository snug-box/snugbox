package collect

import (
	"io"
	"io/fs"
	"path"
	"strings"

	"snugbox.dev/snugbox/agent/internal/helperapi"
	"snugbox.dev/snugbox/agent/internal/model"
)

// UnitDirs are the folders systemd loads system units from, most important
// first (systemd.unit(5)), less the ones only systemd itself writes to.
// /lib/systemd/system is /usr/lib/systemd/system on distributions that
// merged /usr into one.
var UnitDirs = []string{"etc/systemd/system", "run/systemd/system", "usr/local/lib/systemd/system", "usr/lib/systemd/system", "lib/systemd/system"}

// maxUnitFile is the most of a unit file Timers reads. A larger one, or a
// link to a device that never ends, is skipped.
const maxUnitFile = 64 << 10

// Timers finds the timer that starts each of services, from the timer units
// in UnitDirs. As with systemd, a unit in an earlier folder hides one of the
// same name in a later one, and a timer without Unit= starts the service of
// its own name. An instance such as "scrub@data.service" is matched by
// "scrub@data.timer", or by the template "scrub@.timer" when that names no
// unit. A service that two timers start is left out, since moving one would
// leave it starting at the other's time. Moved is when the schedule fix's
// drop-in was written, for a timer Snugbox has moved.
func Timers(root fs.FS, services []string) map[string]model.Timer {
	starts := make(map[string]string)
	templates := make(map[string]bool)
	seen := make(map[string]bool)
	for _, dir := range UnitDirs {
		entries, err := fs.ReadDir(root, dir)
		if err != nil {
			continue
		}
		for _, e := range entries {
			name := e.Name()
			if !strings.HasSuffix(name, ".timer") || seen[name] {
				continue
			}
			seen[name] = true
			unit, ok := timerUnit(root, path.Join(dir, name))
			base := strings.TrimSuffix(name, ".timer")
			switch {
			case !ok || strings.Contains(unit, "%"):
				// Masked, unreadable, or naming its unit with specifiers
				// Snugbox doesn't expand.
			case strings.HasSuffix(base, "@"):
				templates[base] = unit == ""
			default:
				if unit == "" {
					unit = base + ".service"
				}
				if _, twice := starts[unit]; twice {
					name = ""
				}
				starts[unit] = name
			}
		}
	}
	out := make(map[string]model.Timer)
	for _, svc := range services {
		timer, ok := starts[svc]
		if prefix, _, instance := strings.Cut(svc, "@"); !ok && instance && templates[prefix+"@"] {
			timer, ok = strings.TrimSuffix(svc, ".service")+".timer", true
		}
		if !ok || timer == "" {
			continue
		}
		t := model.Timer{Name: timer, Service: svc}
		if info, err := fs.Stat(root, path.Join(UnitDirs[0], timer+".d", helperapi.ScheduleDropIn)); err == nil {
			t.Moved = info.ModTime()
		}
		out[svc] = t
	}
	return out
}

// timerUnit reads the unit a timer file starts: the last Unit= in its
// [Timer] section, or "" when it sets none. ok is false when the file has no
// [Timer] section, as a masked unit, a link to /dev/null, has none.
func timerUnit(root fs.FS, name string) (unit string, ok bool) {
	data, ok := readUnit(root, name)
	if !ok {
		return "", false
	}
	unit, _, ok = lastSetting(data, "Timer", "Unit")
	return unit, ok
}

// readUnit reads a unit file or drop-in of at most maxUnitFile bytes. Only
// regular files and character devices are opened, the second for a unit
// masked by a link to /dev/null, so a link to a FIFO can't stall a read.
func readUnit(root fs.FS, name string) ([]byte, bool) {
	info, err := fs.Stat(root, name)
	if err != nil || !info.Mode().IsRegular() && info.Mode()&fs.ModeCharDevice == 0 {
		return nil, false
	}
	f, err := root.Open(name)
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

// lastSetting returns the value of the last key= line in the [section]
// parts of a unit file, with set saying whether there was one and
// hasSection whether the file has that section at all. A backslash at the
// end of a line joins it to the next, with a space in between
// (systemd.syntax(7)).
func lastSetting(data []byte, section, key string) (value string, set, hasSection bool) {
	text := strings.ReplaceAll(strings.ReplaceAll(string(data), "\r\n", "\n"), "\\\n", " ")
	current := ""
	for line := range strings.Lines(text) {
		line = strings.TrimSpace(line)
		switch {
		case line == "" || line[0] == '#' || line[0] == ';':
		case line[0] == '[' && strings.HasSuffix(line, "]"):
			current = line[1 : len(line)-1]
			hasSection = hasSection || current == section
		case current == section:
			if k, v, found := strings.Cut(line, "="); found && strings.TrimSpace(k) == key {
				value, set = strings.TrimSpace(v), true
			}
		}
	}
	return value, set, hasSection
}
