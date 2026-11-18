package update_test

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"testing/fstest"

	"snugbox.dev/snugbox/agent/internal/update"
	"snugbox.dev/snugbox/agent/internal/update/repotest"
)

type entry struct {
	name, body string
	link       bool
}

func tarball(t *testing.T, entries ...entry) []byte {
	t.Helper()
	var b bytes.Buffer
	zw := gzip.NewWriter(&b)
	tw := tar.NewWriter(zw)
	for _, e := range entries {
		h := &tar.Header{Name: e.name, Mode: 0o755, Size: int64(len(e.body)), Typeflag: tar.TypeReg}
		if e.link {
			h = &tar.Header{Name: e.name, Linkname: "/etc/shadow", Typeflag: tar.TypeSymlink}
		}
		if err := tw.WriteHeader(h); err != nil {
			t.Fatal(err)
		}
		if !e.link {
			if _, err := tw.Write([]byte(e.body)); err != nil {
				t.Fatal(err)
			}
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	return b.Bytes()
}

func release(t *testing.T, version string) []byte {
	return tarball(t, entry{name: "snugbox", body: "cli " + version}, entry{name: "snugboxd", body: "watcher " + version},
		entry{name: "snugbox-helper", body: "helper " + version}, entry{name: "LICENSE", body: "Apache-2.0"})
}

// served starts a repository with a release on the stable channel, and
// returns it and a Repository a client would trust.
func served(t *testing.T, version string) (*repotest.Repo, update.Repository) {
	t.Helper()
	r, err := repotest.Init(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if version != "" {
		if err := r.Publish("stable", "amd64", version, release(t, version)); err != nil {
			t.Fatal(err)
		}
	}
	srv := httptest.NewServer(http.FileServer(http.Dir(r.Dir)))
	t.Cleanup(srv.Close)
	root, err := r.Root()
	if err != nil {
		t.Fatal(err)
	}
	return r, update.Repository{Root: root, MetadataURL: srv.URL + "/metadata/", TargetsURL: srv.URL + "/targets/"}
}

func opts(t *testing.T) update.Options {
	return update.Options{Channel: "stable", Arch: "amd64", CacheDir: filepath.Join(t.TempDir(), "cache")}
}

func TestLatestAndDownload(t *testing.T) {
	_, repo := served(t, "0.2.0")
	o := opts(t)
	rel, err := update.Latest(repo, o)
	if err != nil {
		t.Fatal(err)
	}
	if rel.Version != "0.2.0" || rel.Target != "stable/snugbox_linux_amd64.tar.gz" {
		t.Errorf("release %+v", rel)
	}
	data, err := rel.Download(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	files, err := update.Unpack(data)
	if err != nil {
		t.Fatal(err)
	}
	if string(files["snugbox"]) != "cli 0.2.0" || string(files["snugbox-helper"]) != "helper 0.2.0" || len(files) != 3 {
		t.Errorf("programs %q", files)
	}
	// What was trusted stays, so the next check starts from it.
	for _, f := range []string{"root.json", "timestamp.json", "snapshot.json", "targets.json"} {
		if _, err := os.Stat(filepath.Join(o.CacheDir, f)); err != nil {
			t.Errorf("the cache has no %s: %v", f, err)
		}
	}
	if fi, err := os.Stat(o.CacheDir); err != nil || fi.Mode().Perm()&0o077 != 0 && os.PathSeparator == '/' {
		t.Errorf("the cache folder is %v, %v; only its owner may use it", fi.Mode(), err)
	}
}

// A mirror that changed the release's bytes is caught by the hash in the
// signed metadata.
func TestTamperedReleaseIsRefused(t *testing.T) {
	r, repo := served(t, "0.2.0")
	if err := r.Tamper(update.TargetName("stable", "amd64")); err != nil {
		t.Fatal(err)
	}
	rel, err := update.Latest(repo, opts(t))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := rel.Download(t.TempDir()); err == nil || !strings.Contains(err.Error(), "doesn't match its signed metadata") {
		t.Errorf("a tampered release downloaded: %v", err)
	}
}

// A repository whose signing stopped serves metadata that has expired,
// which the client refuses rather than install from a frozen view.
func TestExpiredMetadataIsRefused(t *testing.T) {
	r, repo := served(t, "0.2.0")
	if err := r.Expire(); err != nil {
		t.Fatal(err)
	}
	if _, err := update.Latest(repo, opts(t)); err == nil || !strings.Contains(err.Error(), "doesn't verify") {
		t.Errorf("expired metadata was used: %v", err)
	}
}

func TestAnotherRootIsRefused(t *testing.T) {
	_, repo := served(t, "0.2.0")
	other, _ := served(t, "")
	repo.Root, _ = other.Root()
	if _, err := update.Latest(repo, opts(t)); err == nil {
		t.Error("a repository signed by other keys was trusted")
	}
}

// Once a client has seen newer metadata, a mirror can't serve it the older
// one again, which would hide a newer release.
func TestRollbackIsRefused(t *testing.T) {
	r, repo := served(t, "0.2.0")
	saved := t.TempDir()
	if err := os.CopyFS(saved, os.DirFS(filepath.Join(r.Dir, "metadata"))); err != nil {
		t.Fatal(err)
	}
	if err := r.Publish("stable", "amd64", "0.3.0", release(t, "0.3.0")); err != nil {
		t.Fatal(err)
	}
	o := opts(t)
	if rel, err := update.Latest(repo, o); err != nil || rel.Version != "0.3.0" {
		t.Fatalf("latest %+v, %v", rel, err)
	}
	if err := os.RemoveAll(filepath.Join(r.Dir, "metadata")); err != nil {
		t.Fatal(err)
	}
	if err := os.CopyFS(filepath.Join(r.Dir, "metadata"), os.DirFS(saved)); err != nil {
		t.Fatal(err)
	}
	if rel, err := update.Latest(repo, o); err == nil {
		t.Errorf("older metadata was trusted after newer: %+v", rel)
	}
}

func TestChannelArchAndVersion(t *testing.T) {
	r, repo := served(t, "0.2.0")
	o := opts(t)
	o.Channel = "edge"
	if _, err := update.Latest(repo, o); err == nil || !strings.Contains(err.Error(), "not a channel") {
		t.Errorf("channel edge: %v", err)
	}
	o.Channel, o.Arch = "stable", "arm64"
	if _, err := update.Latest(repo, o); err == nil || !strings.Contains(err.Error(), "has no stable/snugbox_linux_arm64.tar.gz") {
		t.Errorf("a missing architecture: %v", err)
	}
	if err := r.Publish("beta", "amd64", "not-a-version", release(t, "x")); err != nil {
		t.Fatal(err)
	}
	o.Channel, o.Arch = "beta", "amd64"
	if _, err := update.Latest(repo, o); err == nil || !strings.Contains(err.Error(), "doesn't name a release version") {
		t.Errorf("a target without a version: %v", err)
	}
}

func TestEmbeddedTrust(t *testing.T) {
	if _, err := update.Embedded(); !errors.Is(err, update.ErrNoTrust) {
		t.Errorf("this build has no root, but Embedded says %v", err)
	}
	_, good := served(t, "")
	for name, c := range map[string]struct {
		files fstest.MapFS
		ok    bool
	}{
		"good":          {fstest.MapFS{"t/root.json": {Data: good.Root}, "t/repository.json": {Data: []byte(`{"metadata_url":"https://u.example/m/","targets_url":"https://u.example/t/"}`)}}, true},
		"loopback http": {fstest.MapFS{"t/root.json": {Data: good.Root}, "t/repository.json": {Data: []byte(`{"metadata_url":"http://127.0.0.1:8099/m/","targets_url":"http://localhost:8099/t/"}`)}}, true},
		"plain http":    {fstest.MapFS{"t/root.json": {Data: good.Root}, "t/repository.json": {Data: []byte(`{"metadata_url":"http://u.example/m/","targets_url":"https://u.example/t/"}`)}}, false},
		"bad root":      {fstest.MapFS{"t/root.json": {Data: []byte("{}")}, "t/repository.json": {Data: []byte(`{"metadata_url":"https://u.example/m/","targets_url":"https://u.example/t/"}`)}}, false},
		"no urls":       {fstest.MapFS{"t/root.json": {Data: good.Root}, "t/repository.json": {Data: []byte(`{}`)}}, false},
	} {
		_, err := update.FromFS(c.files, "t")
		if (err == nil) != c.ok {
			t.Errorf("%s: %v", name, err)
		}
	}
	if _, err := update.FromFS(fstest.MapFS{"t/root.json": {Data: good.Root}}, "t"); !errors.Is(err, update.ErrNoTrust) {
		t.Errorf("a root without repository.json: %v", err)
	}
}

func TestUnpackRefusesOddArchives(t *testing.T) {
	cli, watcher, helper := entry{name: "./snugbox", body: "cli"}, entry{name: "snugboxd", body: "w"}, entry{name: "snugbox-helper", body: "h"}
	if files, err := update.Unpack(tarball(t, cli, watcher, helper, entry{name: "docs/README.md", body: "x"})); err != nil || len(files) != 3 {
		t.Errorf("a good archive: %v, %d files", err, len(files))
	}
	for name, tb := range map[string][]byte{
		"a program missing": tarball(t, cli, watcher),
		"a link":            tarball(t, cli, watcher, entry{name: "snugbox-helper", link: true}),
		"a program twice":   tarball(t, cli, watcher, helper, helper),
		"an empty program":  tarball(t, cli, watcher, entry{name: "snugbox-helper"}),
		"not gzip":          []byte("snugbox"),
		"cut short":         tarball(t, cli, watcher, helper)[:40],
	} {
		if _, err := update.Unpack(tb); err == nil {
			t.Errorf("%s was unpacked", name)
		}
	}
}

func TestNewer(t *testing.T) {
	for _, c := range []struct {
		a, b string
		want bool
	}{
		{"0.2.0", "0.1.0", true},
		{"0.10.0", "0.9.9", true},
		{"1.0.0", "1.0.0", false},
		{"1.0.0", "1.0.0-rc.1", true},
		{"1.0.0-rc.2", "1.0.0-rc.1", true},
		{"1.0.0-rc.10", "1.0.0-rc.9", true},
		{"1.0.0-rc.1", "1.0.0-beta.9", true},
		{"1.0.0-alpha.1", "1.0.0-alpha", true},
		{"1.0.0-1", "1.0.0-alpha", false},
		{"1.2.3+build.5", "1.2.3", false},
		{"0.1.0", "0.2.0", false},
		{"dev", "0.1.0", false},
		{"0.2.0", "dev", false},
		{"01.2.3", "1.2.2", false},
		{"1.2", "1.1.9", false},
		{"1.2.3-", "1.2.2", false},
	} {
		if got := update.Newer(c.a, c.b); got != c.want {
			t.Errorf("Newer(%q, %q) = %v, want %v", c.a, c.b, got, c.want)
		}
	}
}
