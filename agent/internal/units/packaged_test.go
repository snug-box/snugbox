package units

import (
	"bytes"
	"flag"
	"os"
	"path/filepath"
	"testing"
)

var update = flag.Bool("update", false, "rewrite the packaged unit files in packaging/")

// PackageLibexec is where the .deb and .rpm packages put the three
// binaries; /usr/bin/snugbox links to the command there.
const PackageLibexec = "/usr/libexec/snugbox"

// The packages ship rendered copies of the unit templates, because
// GoReleaser can't render templates into packages. This test keeps the
// copies equal to the templates; go test ./agent/internal/units -update
// rewrites them.
func TestPackagedUnitsAreCurrent(t *testing.T) {
	watcher, err := Watcher(PackageLibexec)
	if err != nil {
		t.Fatal(err)
	}
	helper, err := Helper(PackageLibexec + "/snugbox-helper")
	if err != nil {
		t.Fatal(err)
	}
	root := filepath.Join("..", "..", "..", "packaging")
	for _, f := range []struct {
		path string
		want []byte
	}{
		{filepath.Join(root, "systemd", WatcherName), watcher},
		{filepath.Join(root, "systemd", HelperName), helper},
		{filepath.Join(root, "systemd", HelperSocket), Socket()},
		{filepath.Join(root, "sysusers.d", "snugbox.conf"), Sysusers()},
	} {
		if *update {
			if err := os.MkdirAll(filepath.Dir(f.path), 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(f.path, f.want, 0o644); err != nil {
				t.Fatal(err)
			}
			continue
		}
		got, err := os.ReadFile(f.path)
		if err != nil {
			t.Fatalf("%v; run go test ./agent/internal/units -update", err)
		}
		if !bytes.Equal(bytes.ReplaceAll(got, []byte("\r\n"), []byte("\n")), f.want) {
			t.Errorf("%s differs from its template; run go test ./agent/internal/units -update", f.path)
		}
	}
}
