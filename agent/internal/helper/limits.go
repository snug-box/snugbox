package helper

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"path"
	"strconv"
	"strings"
)

// The service limits fix keeps MemoryHigh and MemoryMax in a drop-in.
// systemd 252 and 255 put a drop-in's memory settings on the running
// service's cgroup when they reload; systemd 257 only loads them, and the
// cgroup keeps its old values until the service restarts (measured on
// Debian 13 with systemd 257.13, 3 Nov 2026; systemd.unit(5) says a
// reloaded setting "might not be in effect immediately"). systemctl
// set-property --runtime changes a running unit's cgroup without a restart,
// but it also leaves one file per setting in ControlDir, which would keep
// the value in force after an undo until the next reboot. So the helper
// calls it only for a setting the cgroup doesn't hold yet, and removes the
// files that call leaves.

// memorySettings are the unit settings the fix sets and the cgroup file
// each one becomes.
var memorySettings = []struct{ name, file string }{
	{"MemoryHigh", "memory.high"},
	{"MemoryMax", "memory.max"},
}

// runtimeFile is where systemctl set-property --runtime keeps a unit's
// setting.
func runtimeFile(unit, setting string) string {
	return path.Join(ControlDir, unit+".d", "50-"+setting+".conf")
}

// limits returns an OpLimit step's values by setting, as systemctl takes
// them.
func (s *Step) limits() map[string]string {
	return map[string]string{
		"MemoryHigh": strconv.FormatUint(s.High, 10),
		"MemoryMax":  strconv.FormatUint(s.Max, 10),
	}
}

// cgroupValue is how the kernel shows a systemd memory setting: "max" for
// infinity, the byte count otherwise.
func cgroupValue(setting string) string {
	if setting == "infinity" {
		return "max"
	}
	return setting
}

// runtimeValue reads the value a set-property file holds, or "" when the
// file has no line for setting.
func runtimeValue(content, setting string) string {
	v := ""
	for line := range strings.Lines(content) {
		if val, ok := strings.CutPrefix(strings.TrimSpace(line), setting+"="); ok {
			v = val
		}
	}
	return v
}

// setLive puts want on unit's running cgroup. A setting the cgroup holds
// already is left alone, so on a systemd that applied it at the reload
// nothing runs. A unit with no cgroup takes its settings when it starts. A
// setting someone set at runtime is theirs, so it is skipped and the note
// says so.
func setLive(ctx context.Context, sys System, unit string, want map[string]string) (string, error) {
	props, err := showUnit(ctx, sys, unit, "ControlGroup")
	if err != nil {
		return "", err
	}
	cg := strings.TrimPrefix(first(props, "ControlGroup"), "/")
	if cg == "" {
		return "", nil
	}
	var args, made, theirs []string
	for _, m := range memorySettings {
		b, err := sys.ReadFile(path.Join(CgroupFS, cg, m.file))
		if errors.Is(err, fs.ErrNotExist) {
			return "", nil
		}
		if err != nil {
			return "", err
		}
		if strings.TrimSpace(string(b)) == cgroupValue(want[m.name]) {
			continue
		}
		f := runtimeFile(unit, m.name)
		_, exists, err := readOptional(sys, f)
		if err != nil {
			return "", err
		}
		if exists {
			theirs = append(theirs, m.name)
			continue
		}
		args = append(args, m.name+"="+want[m.name])
		made = append(made, f)
	}
	note := ""
	if len(theirs) > 0 {
		note = fmt.Sprintf("%s on %s was set at runtime with systemctl set-property since Snugbox checked, so it was left as it is.", strings.Join(theirs, " and "), unit)
	}
	if len(args) == 0 {
		return note, nil
	}
	// The folder for systemd's runtime settings is made when the helper
	// starts; without it, the files set-property leaves couldn't be removed.
	if _, err := sys.ListDir(ControlDir); err != nil {
		return note, fmt.Errorf("can't set the limits on the running %s: %w", unit, err)
	}
	if _, err := sys.Systemctl(ctx, append([]string{"set-property", "--runtime", "--", unit}, args...)...); err != nil {
		return note, err
	}
	for _, f := range made {
		if err := sys.RemoveFile(f); err != nil {
			return note, err
		}
	}
	if _, err := sys.RemoveDir(path.Dir(made[0])); err != nil {
		return note, err
	}
	// systemd forgets the removed files only when it reloads.
	_, err = sys.Systemctl(ctx, "daemon-reload")
	return note, err
}

// clearRuntime removes the files an OpLimit step's set-property call left
// when the helper stopped before removing them: they hold the step's
// values, or "infinity" from the undo that put the unit's own limits back,
// since preflight only limits a unit that had none. A file holding anything
// else was made by a person, and stays.
func clearRuntime(sys System, s *Step) (string, error) {
	want := s.limits()
	var kept []string
	removed := false
	for _, m := range memorySettings {
		f := runtimeFile(s.Target, m.name)
		content, exists, err := readOptional(sys, f)
		if err != nil {
			return "", err
		}
		if !exists {
			continue
		}
		if v := runtimeValue(content, m.name); v != want[m.name] && v != "infinity" {
			kept = append(kept, f)
			continue
		}
		if err := sys.RemoveFile(f); err != nil {
			return "", err
		}
		removed = true
	}
	if removed {
		if _, err := sys.RemoveDir(path.Join(ControlDir, s.Target+".d")); err != nil {
			return "", err
		}
	}
	if len(kept) > 0 {
		return strings.Join(kept, " and ") + " was changed by someone else, so it was left as it is.", nil
	}
	return "", nil
}

// ownLimits reads the MemoryHigh and MemoryMax a unit has from its own
// configuration, once Snugbox's drop-in is gone and systemd reloaded. It
// returns nil for a unit systemd no longer knows.
func ownLimits(ctx context.Context, sys System, unit string) (map[string]string, error) {
	props, err := showUnit(ctx, sys, unit, "LoadState", "MemoryHigh", "MemoryMax")
	if err != nil {
		return nil, err
	}
	if first(props, "LoadState") != "loaded" {
		return nil, nil
	}
	return map[string]string{"MemoryHigh": first(props, "MemoryHigh"), "MemoryMax": first(props, "MemoryMax")}, nil
}
