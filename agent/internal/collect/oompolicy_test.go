package collect

import (
	"testing"
	"testing/fstest"
)

func TestOOMPolicy(t *testing.T) {
	const fedora = "[Slice]\nManagedOOMMemoryPressure=kill\nManagedOOMMemoryPressureLimit=80%\n"
	file := func(s string) *fstest.MapFile { return &fstest.MapFile{Data: []byte(s)} }
	// Fedora 44 as it ships: systemd-oomd-defaults sets system.slice, and
	// /lib is /usr/lib, so every file shows twice.
	base := func() fstest.MapFS {
		return fstest.MapFS{
			"usr/lib/systemd/systemd-oomd":                                          file(""),
			"usr/lib/systemd/system/system.slice":                                   file("[Unit]\nDescription=System Slice\n"),
			"usr/lib/systemd/system/system.slice.d/10-oomd-per-slice-defaults.conf": file(fedora),
			"lib/systemd/system/system.slice.d/10-oomd-per-slice-defaults.conf":     file(fedora),
		}
	}
	cases := []struct {
		name  string
		edit  func(fstest.MapFS)
		want  OOMPolicy
		empty bool
	}{
		{name: "Fedora's default", want: OOMPolicy{Slice: "system.slice", Value: "kill", File: "/usr/lib/systemd/system/system.slice.d/10-oomd-per-slice-defaults.conf"}},
		{name: "systemd-oomd not installed", edit: func(m fstest.MapFS) { delete(m, "usr/lib/systemd/systemd-oomd") }, empty: true},
		{name: "masked in /etc by an empty file of the same name", edit: func(m fstest.MapFS) {
			m["etc/systemd/system/system.slice.d/10-oomd-per-slice-defaults.conf"] = file("")
		}, empty: true},
		{name: "a later drop-in sets it back with an empty value", edit: func(m fstest.MapFS) {
			m["etc/systemd/system/system.slice.d/90-mine.conf"] = file("[Slice]\nManagedOOMMemoryPressure=\n")
		}, empty: true},
		{name: "a later drop-in sets auto", edit: func(m fstest.MapFS) {
			m["run/systemd/system/system.slice.d/90-mine.conf"] = file("[Slice]\nManagedOOMMemoryPressure=auto\n")
		}, empty: true},
		{name: "an earlier name loses to the distribution's", edit: func(m fstest.MapFS) {
			m["etc/systemd/system/system.slice.d/01-mine.conf"] = file("[Slice]\nManagedOOMMemoryPressure=auto\n")
		}, want: OOMPolicy{Slice: "system.slice", Value: "kill", File: "/usr/lib/systemd/system/system.slice.d/10-oomd-per-slice-defaults.conf"}},
		{name: "the setting outside [Slice] does nothing", edit: func(m fstest.MapFS) {
			for k := range m {
				if k != "usr/lib/systemd/systemd-oomd" && k != "usr/lib/systemd/system/system.slice" {
					delete(m, k)
				}
			}
			m["etc/systemd/system/system.slice.d/10-x.conf"] = file("[Service]\nManagedOOMMemoryPressure=kill\n")
		}, empty: true},
		{name: "set-property --runtime on user.slice", edit: func(m fstest.MapFS) {
			delete(m, "usr/lib/systemd/system/system.slice.d/10-oomd-per-slice-defaults.conf")
			delete(m, "lib/systemd/system/system.slice.d/10-oomd-per-slice-defaults.conf")
			m["run/systemd/system.control/user.slice.d/50-ManagedOOMMemoryPressure.conf"] = file("# This is a drop-in unit file extension, created via \"systemctl set-property\"\n[Slice]\nManagedOOMMemoryPressure=kill\n")
		}, want: OOMPolicy{Slice: "user.slice", Value: "kill", File: "/run/systemd/system.control/user.slice.d/50-ManagedOOMMemoryPressure.conf"}},
		{name: "slice.d reaches every slice, and the slice's own folder hides the same name", edit: func(m fstest.MapFS) {
			delete(m, "usr/lib/systemd/system/system.slice.d/10-oomd-per-slice-defaults.conf")
			delete(m, "lib/systemd/system/system.slice.d/10-oomd-per-slice-defaults.conf")
			m["usr/lib/systemd/system/slice.d/20-all.conf"] = file("[Slice]\nManagedOOMMemoryPressure=kill\n")
			m["usr/lib/systemd/system/system.slice.d/20-all.conf"] = file("[Slice]\nManagedOOMMemoryPressure=auto\n")
		}, want: OOMPolicy{Slice: "user.slice", Value: "kill", File: "/usr/lib/systemd/system/slice.d/20-all.conf"}},
		{name: "in the unit file itself", edit: func(m fstest.MapFS) {
			delete(m, "usr/lib/systemd/system/system.slice.d/10-oomd-per-slice-defaults.conf")
			delete(m, "lib/systemd/system/system.slice.d/10-oomd-per-slice-defaults.conf")
			m["etc/systemd/system/user.slice"] = file("[Slice]\nManagedOOMMemoryPressure=kill\n")
		}, want: OOMPolicy{Slice: "user.slice", Value: "kill", File: "/etc/systemd/system/user.slice"}},
		{name: "a file without .conf is not a drop-in", edit: func(m fstest.MapFS) {
			delete(m, "usr/lib/systemd/system/system.slice.d/10-oomd-per-slice-defaults.conf")
			delete(m, "lib/systemd/system/system.slice.d/10-oomd-per-slice-defaults.conf")
			m["etc/systemd/system/system.slice.d/10-x.conf.bak"] = file("[Slice]\nManagedOOMMemoryPressure=kill\n")
		}, empty: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			root := base()
			if tc.edit != nil {
				tc.edit(root)
			}
			got := (&Collector{fsys: root}).oomPolicy()
			if tc.empty {
				tc.want = OOMPolicy{}
			}
			if got != tc.want {
				t.Errorf("oomPolicy() = %+v, want %+v", got, tc.want)
			}
		})
	}
}
