// Package update finds and fetches a newer release of Snugbox for a script
// install, verified through The Update Framework (blueprint section 6.3).
// The root of trust is built into the binary; every metadata file and the
// release itself are checked against it before anything is used. Packages
// from apt or dnf update through their package manager instead.
//
// A repository holds two files per channel: VersionName, a line with the
// channel's newest version, such as "stable/version", and the tarball of
// that release for each architecture under TargetName, such as
// "stable/snugbox_linux_amd64.tar.gz". Both are plain files, so any TUF
// repository tool can publish them. The repository uses consistent
// snapshots, so each file is fetched under its hash.
package update

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"embed"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net"
	"net/http"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"slices"
	"strings"

	"github.com/theupdateframework/go-tuf/v2/metadata"
	"github.com/theupdateframework/go-tuf/v2/metadata/config"
	"github.com/theupdateframework/go-tuf/v2/metadata/updater"
)

//go:embed trust
var trustFS embed.FS

// ErrNoTrust means the build carries no update root, so it can't update
// itself.
var ErrNoTrust = errors.New("this build of Snugbox carries no update root, so it can't update itself; run the install script again to update it")

// Channels are the release channels of blueprint section 9.6.
var Channels = []string{"stable", "beta", "nightly"}

// Repository is where releases are published, and the root that signs them.
type Repository struct {
	Root        []byte `json:"-"`
	MetadataURL string `json:"metadata_url"`
	TargetsURL  string `json:"targets_url"`
}

// Embedded returns the repository built into this release, or ErrNoTrust.
func Embedded() (Repository, error) { return FromFS(trustFS, "trust") }

// FromFS reads a repository from the root.json and repository.json in dir.
func FromFS(fsys fs.FS, dir string) (Repository, error) {
	var r Repository
	root, err := fs.ReadFile(fsys, path.Join(dir, "root.json"))
	if errors.Is(err, fs.ErrNotExist) {
		return r, ErrNoTrust
	}
	if err != nil {
		return r, err
	}
	raw, err := fs.ReadFile(fsys, path.Join(dir, "repository.json"))
	if errors.Is(err, fs.ErrNotExist) {
		return r, ErrNoTrust
	}
	if err != nil {
		return r, err
	}
	if err := json.Unmarshal(raw, &r); err != nil {
		return r, fmt.Errorf("update: repository.json: %w", err)
	}
	r.Root = root
	return r, r.check()
}

func (r Repository) check() error {
	for _, u := range []string{r.MetadataURL, r.TargetsURL} {
		p, err := url.Parse(u)
		if err != nil || p.Host == "" {
			return fmt.Errorf("update: %q is not a URL", u)
		}
		// TUF's signatures protect the files whatever carries them; HTTPS
		// keeps what a box asks for private. Plain HTTP is for a repository
		// on the box itself, in tests.
		if p.Scheme != "https" && (p.Scheme != "http" || !loopback(p.Hostname())) {
			return fmt.Errorf("update: %s must use https", u)
		}
	}
	if _, err := rootVersion(r.Root); err != nil {
		return fmt.Errorf("update: root.json: %w", err)
	}
	return nil
}

func loopback(host string) bool {
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

func rootVersion(b []byte) (int64, error) {
	m, err := (&metadata.Metadata[metadata.RootType]{}).FromBytes(b)
	if err != nil {
		return 0, err
	}
	return m.Signed.Version, nil
}

// trustedRoot returns the newer of the root built in and the one trusted
// last, which the cache folder keeps after the client followed the root's
// rotations.
func trustedRoot(embedded []byte, cacheDir string) ([]byte, error) {
	cached, err := os.ReadFile(filepath.Join(cacheDir, "root.json")) //nolint:gosec // the cache folder is root's own, which the caller checks
	if errors.Is(err, fs.ErrNotExist) {
		return embedded, nil
	}
	if err != nil {
		return nil, err
	}
	have, err := rootVersion(embedded)
	if err != nil {
		return nil, err
	}
	if v, err := rootVersion(cached); err == nil && v > have {
		return cached, nil
	}
	return embedded, nil
}

// TargetName is the repository's name for a channel's tarball for one
// architecture.
func TargetName(channel, arch string) string {
	return channel + "/snugbox_linux_" + arch + ".tar.gz"
}

// VersionName is the repository's name for the file that holds a channel's
// newest version.
func VersionName(channel string) string { return channel + "/version" }

// maxVersionFile is the largest version file Latest reads.
const maxVersionFile = 256

// Options say what to look for and where to keep what was trusted.
type Options struct {
	Channel, Arch string
	// CacheDir keeps the metadata trusted so far, so a mirror can't later
	// serve older metadata, or a root that came before a rotation. Only root
	// may write it.
	CacheDir string
	HTTP     *http.Client
}

// Release is the newest release on a channel, as the repository's signed
// metadata describes it.
type Release struct {
	Version string
	Target  string
	info    *metadata.TargetFiles
	up      *updater.Updater
}

// Latest fetches and verifies the repository's metadata, and returns the
// release it lists for the channel and architecture.
func Latest(repo Repository, o Options) (*Release, error) {
	if !slices.Contains(Channels, o.Channel) {
		return nil, fmt.Errorf("update: %q is not a channel; there are %s", o.Channel, strings.Join(Channels, ", "))
	}
	if err := os.MkdirAll(o.CacheDir, 0o700); err != nil {
		return nil, fmt.Errorf("update: %w", err)
	}
	root, err := trustedRoot(repo.Root, o.CacheDir)
	if err != nil {
		return nil, fmt.Errorf("update: %w", err)
	}
	cfg, err := config.New(repo.MetadataURL, root)
	if err != nil {
		return nil, fmt.Errorf("update: %w", err)
	}
	cfg.RemoteTargetsURL = repo.TargetsURL
	cfg.LocalMetadataDir = o.CacheDir
	cfg.LocalTargetsDir = o.CacheDir
	cfg.PrefixTargetsWithHash = true
	if o.HTTP != nil {
		if err := cfg.SetDefaultFetcherHTTPClient(o.HTTP); err != nil {
			return nil, fmt.Errorf("update: %w", err)
		}
	}
	up, err := updater.New(cfg)
	if err != nil {
		return nil, fmt.Errorf("update: the update root doesn't verify: %w", err)
	}
	if err := up.Refresh(); err != nil {
		return nil, fmt.Errorf("update: the update repository's metadata doesn't verify: %w", err)
	}
	vinfo, err := up.GetTargetInfo(VersionName(o.Channel))
	if err != nil {
		return nil, fmt.Errorf("update: the repository has no %s: %w", VersionName(o.Channel), err)
	}
	if vinfo.Length > maxVersionFile {
		return nil, fmt.Errorf("update: the repository's %s is %d bytes, too long for a version", VersionName(o.Channel), vinfo.Length)
	}
	tmp, err := os.MkdirTemp("", "snugbox-version-")
	if err != nil {
		return nil, fmt.Errorf("update: %w", err)
	}
	defer os.RemoveAll(tmp)
	_, raw, err := up.DownloadTarget(vinfo, filepath.Join(tmp, "version"), "")
	if err != nil {
		return nil, fmt.Errorf("update: %s didn't download, or doesn't match its signed metadata: %w", VersionName(o.Channel), err)
	}
	version := strings.TrimSpace(string(raw))
	if !ValidVersion(version) {
		return nil, fmt.Errorf("update: the repository's %s doesn't name a release version", VersionName(o.Channel))
	}
	name := TargetName(o.Channel, o.Arch)
	info, err := up.GetTargetInfo(name)
	if err != nil {
		return nil, fmt.Errorf("update: the repository has no %s: %w", name, err)
	}
	return &Release{Version: version, Target: name, info: info, up: up}, nil
}

// Download fetches the release's tarball into dir and returns it, checked
// against the length and hashes the signed metadata gives.
func (r *Release) Download(dir string) ([]byte, error) {
	_, data, err := r.up.DownloadTarget(r.info, filepath.Join(dir, path.Base(r.Target)), "")
	if err != nil {
		return nil, fmt.Errorf("update: %s didn't download, or doesn't match its signed metadata: %w", r.Target, err)
	}
	return data, nil
}

// Programs are the files an update replaces, in the order it replaces
// them: the helper and the watcher before the command that runs the update.
var Programs = []string{"snugbox-helper", "snugboxd", "snugbox"}

// maxProgram is the largest program Unpack takes out of a tarball.
const maxProgram = 128 << 20

// Unpack takes the programs out of a release tarball. Each must be a regular
// file at the top of the archive; anything else in it is left out.
func Unpack(tarball []byte) (map[string][]byte, error) {
	zr, err := gzip.NewReader(bytes.NewReader(tarball))
	if err != nil {
		return nil, fmt.Errorf("update: the release is not a gzip file: %w", err)
	}
	tr := tar.NewReader(zr)
	out := make(map[string][]byte)
	for {
		h, err := tr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("update: the release's tarball: %w", err)
		}
		name := strings.TrimPrefix(h.Name, "./")
		if !slices.Contains(Programs, name) {
			continue
		}
		if _, twice := out[name]; twice || h.Typeflag != tar.TypeReg || h.Size <= 0 || h.Size > maxProgram {
			return nil, fmt.Errorf("update: %s in the release is not one regular file of a sensible size", name)
		}
		data, err := io.ReadAll(io.LimitReader(tr, maxProgram))
		if err != nil || int64(len(data)) != h.Size {
			return nil, fmt.Errorf("update: %s in the release is cut short", name)
		}
		out[name] = data
	}
	for _, p := range Programs {
		if _, ok := out[p]; !ok {
			return nil, fmt.Errorf("update: the release has no %s", p)
		}
	}
	return out, nil
}
