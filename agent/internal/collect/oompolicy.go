package collect

import (
	"io/fs"
	"path"
	"slices"
	"strings"
)

// OOMPolicy is a systemd-oomd policy for the system or user slice that is
// set already, as Fedora's systemd-oomd-defaults package sets one for
// system.slice. The zero value means neither slice has one.
type OOMPolicy struct {
	// Slice is system.slice or user.slice.
	Slice string
	// Value is what ManagedOOMMemoryPressure= is set to, such as kill.
	Value string
	// File is the unit file or drop-in that set it last, from the root.
	File string
}

// dropInDirs are the folders systemd reads system drop-ins from, most
// important first (systemd.unit(5)): those systemctl set-property writes to,
// then UnitDirs.
var dropInDirs = append([]string{"etc/systemd/system.control", "run/systemd/system.control"}, UnitDirs...)

// oomPolicy finds the first of system.slice and user.slice whose
// ManagedOOMMemoryPressure= is set to anything but auto, when systemd-oomd is
// installed. The setting is read the way systemd reads it: the unit file
// first, then every drop-in in the order of its name, where a name in a more
// important folder hides the same name in a less important one, and the
// slice's own folder comes ahead of slice.d, which every slice reads. The
// last line wins, and an empty one sets the default, auto, back.
func (c *Collector) oomPolicy() OOMPolicy {
	if !c.exists("usr/lib/systemd/systemd-oomd") && !c.exists("lib/systemd/systemd-oomd") {
		return OOMPolicy{}
	}
	for _, slice := range []string{"system.slice", "user.slice"} {
		var files []string
		for _, dir := range UnitDirs {
			if p := path.Join(dir, slice); c.exists(p) {
				files = append(files, p)
				break
			}
		}
		dropIns := make(map[string]string)
		for _, dir := range dropInDirs {
			for _, d := range []string{slice + ".d", "slice.d"} {
				entries, err := fs.ReadDir(c.fsys, path.Join(dir, d))
				if err != nil {
					continue
				}
				for _, e := range entries {
					if _, seen := dropIns[e.Name()]; !seen && strings.HasSuffix(e.Name(), ".conf") {
						dropIns[e.Name()] = path.Join(dir, d, e.Name())
					}
				}
			}
		}
		names := make([]string, 0, len(dropIns))
		for n := range dropIns {
			names = append(names, n)
		}
		slices.Sort(names)
		for _, n := range names {
			files = append(files, dropIns[n])
		}
		var p OOMPolicy
		for _, f := range files {
			data, ok := readUnit(c.fsys, f)
			if !ok {
				continue
			}
			if v, set, _ := lastSetting(data, "Slice", "ManagedOOMMemoryPressure"); set {
				p.Value, p.File = v, "/"+f
			}
		}
		if p.Value != "" && p.Value != "auto" {
			p.Slice = slice
			return p
		}
	}
	return OOMPolicy{}
}
