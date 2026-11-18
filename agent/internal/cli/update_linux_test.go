//go:build linux

package cli

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"

	"snugbox.dev/snugbox/agent/internal/update"
	"snugbox.dev/snugbox/agent/internal/update/repotest"
)

type updateBox struct {
	repo *repotest.Repo
	dir  string
	mu   sync.Mutex
	ran  [][]string
}

// newUpdateBox serves a repository and points the harness's self-update at
// it, at a script install of version in a folder of its own, with the
// watcher installed.
func newUpdateBox(t *testing.T, h *harness, version string) *updateBox {
	t.Helper()
	r, err := repotest.Init(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(http.FileServer(http.Dir(r.Dir)))
	t.Cleanup(srv.Close)
	root, err := r.Root()
	if err != nil {
		t.Fatal(err)
	}
	b := &updateBox{repo: r, dir: t.TempDir()}
	for _, p := range update.Programs {
		if err := os.WriteFile(filepath.Join(b.dir, p), []byte(version), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	unit := filepath.Join(t.TempDir(), "snugboxd.service")
	if err := os.WriteFile(unit, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	h.e.euid = 0
	h.e.update = updateEnv{
		repo: func() (update.Repository, error) {
			return update.Repository{Root: root, MetadataURL: srv.URL + "/metadata/", TargetsURL: srv.URL + "/targets/"}, nil
		},
		cache: filepath.Join(t.TempDir(), "update"), version: version, http: srv.Client(),
		dir: func() (string, error) { return b.dir, nil },
		run: func(_ context.Context, name string, args ...string) ([]byte, error) {
			b.mu.Lock()
			defer b.mu.Unlock()
			b.ran = append(b.ran, append([]string{name}, args...))
			if len(args) == 1 && args[0] == "version" {
				v, err := os.ReadFile(name)
				return []byte("snugbox " + string(v) + " (commit c, built d)\n"), err
			}
			return []byte("snugboxd is running.\n"), nil
		},
		watcherUnit: unit,
	}
	return b
}

func (b *updateBox) publish(t *testing.T, version string) {
	t.Helper()
	files := map[string]string{"LICENSE": "Apache-2.0"}
	for _, p := range update.Programs {
		files[p] = version
	}
	tb, err := repotest.Tarball(files)
	if err != nil {
		t.Fatal(err)
	}
	if err := b.repo.Publish("stable", "amd64", version, tb); err != nil {
		t.Fatal(err)
	}
}

func (b *updateBox) programs(t *testing.T) string {
	t.Helper()
	var vs []string
	for _, p := range update.Programs {
		v, err := os.ReadFile(filepath.Join(b.dir, p))
		if err != nil {
			t.Fatal(err)
		}
		vs = append(vs, string(v))
	}
	return strings.Join(vs, " ")
}

func TestSelfUpdate(t *testing.T) {
	if runtime.GOARCH != "amd64" {
		t.Skip("the test repository holds an amd64 release only")
	}
	h := newHarness(t)
	b := newUpdateBox(t, h, "0.1.0")
	b.publish(t, "0.2.0")

	h.expect(t, ExitOK, []string{"self-update", "--check"}, "Snugbox 0.2.0 is out on the stable channel; this is 0.1.0.")
	if got := b.programs(t); got != "0.1.0 0.1.0 0.1.0" {
		t.Fatalf("--check changed the programs: %s", got)
	}
	h.expect(t, ExitOK, []string{"self-update"}, "Downloading Snugbox 0.2.0", "Installed Snugbox 0.2.0 in "+b.dir+", in place of 0.1.0.", "snugboxd is running.")
	if got := b.programs(t); got != "0.2.0 0.2.0 0.2.0" {
		t.Errorf("after the update the programs are %s", got)
	}
	last := b.ran[len(b.ran)-1]
	if strings.Join(last, " ") != filepath.Join(b.dir, "snugbox")+" watch install" {
		t.Errorf("the watcher was restarted with %q", last)
	}
	if fi, err := os.Stat(h.e.update.cache); err != nil || fi.Mode().Perm() != 0o700 {
		t.Errorf("the update cache is %v, %v", fi.Mode(), err)
	}

	h.e.update.version = "0.2.0"
	h.expect(t, ExitOK, []string{"self-update"}, "Snugbox 0.2.0 is the newest release on the stable channel.")
}

// A release whose bytes don't match the signed metadata replaces nothing.
func TestSelfUpdateRefusesATamperedRelease(t *testing.T) {
	if runtime.GOARCH != "amd64" {
		t.Skip("the test repository holds an amd64 release only")
	}
	h := newHarness(t)
	b := newUpdateBox(t, h, "0.1.0")
	b.publish(t, "0.2.0")
	if err := b.repo.Tamper(update.TargetName("stable", "amd64")); err != nil {
		t.Fatal(err)
	}
	h.expect(t, ExitError, []string{"self-update"}, "doesn't match its signed metadata")
	if got := b.programs(t); got != "0.1.0 0.1.0 0.1.0" {
		t.Errorf("a tampered release changed the programs: %s", got)
	}
}

// An update cache others can write would let them plant a root, so it is
// refused.
func TestSelfUpdateRefusesAnOpenCache(t *testing.T) {
	h := newHarness(t)
	newUpdateBox(t, h, "0.1.0")
	if err := os.MkdirAll(h.e.update.cache, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(h.e.update.cache, 0o777); err != nil {
		t.Fatal(err)
	}
	h.expect(t, ExitError, []string{"self-update", "--check"}, "writable by root alone")
}
