// Package units holds the systemd unit files and the sysusers.d entry that
// the packages install, and that snugbox watch install writes when Snugbox
// came from the install script instead.
package units

import (
	"bytes"
	_ "embed"
	"fmt"
	"path"
	"text/template"
)

//go:embed snugboxd.service
var watcherUnit string

//go:embed snugbox-helper.service
var helperUnit string

//go:embed snugbox-helper.socket
var helperSocket []byte

//go:embed sysusers.conf
var sysusers []byte

//go:embed snugboxd.env
var watcherSettings []byte

// Paths where the units live. Packages put theirs under /usr/lib; the
// install script's go under /etc, which systemd reads first.
const (
	PackageUnitDir = "/usr/lib/systemd/system"
	LocalUnitDir   = "/etc/systemd/system"
	SysusersFile   = "/usr/lib/sysusers.d/snugbox.conf"
	LocalSysusers  = "/etc/sysusers.d/snugbox.conf"
	WatcherName    = "snugboxd.service"
	HelperName     = "snugbox-helper.service"
	HelperSocket   = "snugbox-helper.socket"
	// SettingsFile is the watcher's EnvironmentFile=. It can hold a
	// webhook's secret token, so it is written 0600 and only systemd, as
	// root, reads it.
	SettingsFile = "/etc/snugbox/snugboxd.env"
)

func render(name, text string, data any) ([]byte, error) {
	t, err := template.New(name).Option("missingkey=error").Parse(text)
	if err != nil {
		return nil, fmt.Errorf("units: %w", err)
	}
	var b bytes.Buffer
	if err := t.Execute(&b, data); err != nil {
		return nil, fmt.Errorf("units: %w", err)
	}
	return b.Bytes(), nil
}

// Watcher renders snugboxd.service for binaries installed in bindir.
func Watcher(bindir string) ([]byte, error) {
	if !path.IsAbs(bindir) {
		return nil, fmt.Errorf("units: %q is not an absolute path", bindir)
	}
	return render(WatcherName, watcherUnit, struct{ Bindir string }{path.Clean(bindir)})
}

// Helper renders snugbox-helper.service for the helper binary at file.
func Helper(file string) ([]byte, error) {
	if !path.IsAbs(file) {
		return nil, fmt.Errorf("units: %q is not an absolute path", file)
	}
	return render(HelperName, helperUnit, struct{ Helper string }{path.Clean(file)})
}

// Socket returns snugbox-helper.socket.
func Socket() []byte { return bytes.Clone(helperSocket) }

// Sysusers returns the sysusers.d entry that creates the snugbox user.
func Sysusers() []byte { return bytes.Clone(sysusers) }

// Settings returns the watcher's settings file with every setting commented
// out. The packages install it as a configuration file, and snugbox watch
// install writes it when there is none.
func Settings() []byte { return bytes.Clone(watcherSettings) }
