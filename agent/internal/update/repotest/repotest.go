// Package repotest makes a TUF repository for testing snugbox self-update:
// test keys on disk, a root, and a release per channel and architecture
// published as the update package expects. It is for tests and test
// machines only; the real repository's root keys live on the owner's
// hardware keys.
package repotest

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"crypto"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"maps"
	"os"
	"path"
	"path/filepath"
	"slices"
	"time"

	"github.com/sigstore/sigstore/pkg/signature"
	"github.com/theupdateframework/go-tuf/v2/metadata"

	"snugbox.dev/snugbox/agent/internal/update"
)

var roles = []string{metadata.ROOT, metadata.TARGETS, metadata.SNAPSHOT, metadata.TIMESTAMP}

// Repo is a repository in a folder: keys/ holds a test key per role,
// metadata/ what clients read, and targets/ the releases under their
// hashes.
type Repo struct {
	Dir  string
	keys map[string]ed25519.PrivateKey
	// Now is the time the metadata's expiry counts from.
	Now func() time.Time
}

// Init makes a new repository in dir with a fresh key for each role and
// the first root.
func Init(dir string) (*Repo, error) {
	r := &Repo{Dir: dir, keys: make(map[string]ed25519.PrivateKey), Now: time.Now}
	for _, d := range []string{"keys", "metadata", "targets"} {
		if err := os.MkdirAll(filepath.Join(dir, d), 0o755); err != nil {
			return nil, err
		}
	}
	for _, role := range roles {
		_, key, err := ed25519.GenerateKey(rand.Reader)
		if err != nil {
			return nil, err
		}
		if err := os.WriteFile(filepath.Join(dir, "keys", role+".key"), []byte(hex.EncodeToString(key.Seed())), 0o600); err != nil {
			return nil, err
		}
		r.keys[role] = key
	}
	root := metadata.Root(r.Now().AddDate(1, 0, 0))
	for _, role := range roles {
		key, err := metadata.KeyFromPublicKey(r.keys[role].Public())
		if err != nil {
			return nil, err
		}
		if err := root.Signed.AddKey(key, role); err != nil {
			return nil, err
		}
	}
	if err := r.sign(root, metadata.ROOT); err != nil {
		return nil, err
	}
	if err := root.ToFile(filepath.Join(dir, "metadata", "1.root.json"), true); err != nil {
		return nil, err
	}
	return r, r.write(metadata.Targets(r.Now().AddDate(0, 1, 0)))
}

// Open reads a repository Init made.
func Open(dir string) (*Repo, error) {
	r := &Repo{Dir: dir, keys: make(map[string]ed25519.PrivateKey), Now: time.Now}
	for _, role := range roles {
		b, err := os.ReadFile(filepath.Join(dir, "keys", role+".key"))
		if err != nil {
			return nil, err
		}
		seed, err := hex.DecodeString(string(b))
		if err != nil || len(seed) != ed25519.SeedSize {
			return nil, fmt.Errorf("repotest: %s.key is not a key", role)
		}
		r.keys[role] = ed25519.NewKeyFromSeed(seed)
	}
	return r, nil
}

// Root returns the repository's first root, which a client trusts.
func (r *Repo) Root() ([]byte, error) {
	return os.ReadFile(filepath.Join(r.Dir, "metadata", "1.root.json"))
}

func (r *Repo) sign(m interface {
	Sign(signature.Signer) (*metadata.Signature, error)
}, role string) error {
	s, err := signature.LoadSigner(r.keys[role], crypto.Hash(0))
	if err != nil {
		return err
	}
	_, err = m.Sign(s)
	return err
}

// Publish puts a release on a channel: its tarball for one architecture,
// and the channel's version file naming it, then signs new targets,
// snapshot and timestamp metadata.
func (r *Repo) Publish(channel, arch, version string, tarball []byte) error {
	targets, err := r.latestTargets()
	if err != nil {
		return err
	}
	for name, data := range map[string][]byte{update.TargetName(channel, arch): tarball, update.VersionName(channel): []byte(version + "\n")} {
		if err := r.add(targets, name, data); err != nil {
			return err
		}
	}
	targets.Signed.Version++
	targets.Signed.Expires = r.Now().AddDate(0, 1, 0)
	return r.write(targets)
}

// add lists a file in targets and stores it under its hash, as consistent
// snapshots name it.
func (r *Repo) add(targets *metadata.Metadata[metadata.TargetsType], name string, data []byte) error {
	tf, err := metadata.TargetFile().FromBytes(name, data, "sha256")
	if err != nil {
		return err
	}
	targets.Signed.Targets[name] = tf
	sum := sha256.Sum256(data)
	file := filepath.Join(r.Dir, "targets", filepath.FromSlash(path.Dir(name)), hex.EncodeToString(sum[:])+"."+path.Base(name))
	if err := os.MkdirAll(filepath.Dir(file), 0o755); err != nil {
		return err
	}
	return os.WriteFile(file, data, 0o644)
}

// Tarball makes a release tarball holding files, each a program with mode
// 0755, as a release archive holds them.
func Tarball(files map[string]string) ([]byte, error) {
	var b bytes.Buffer
	zw := gzip.NewWriter(&b)
	tw := tar.NewWriter(zw)
	names := slices.Sorted(maps.Keys(files))
	for _, name := range names {
		if err := tw.WriteHeader(&tar.Header{Name: name, Mode: 0o755, Size: int64(len(files[name])), Typeflag: tar.TypeReg}); err != nil {
			return nil, err
		}
		if _, err := tw.Write([]byte(files[name])); err != nil {
			return nil, err
		}
	}
	if err := tw.Close(); err != nil {
		return nil, err
	}
	if err := zw.Close(); err != nil {
		return nil, err
	}
	return b.Bytes(), nil
}

// Tamper replaces the stored bytes of a published target, as a mirror that
// was broken into would, leaving the signed metadata as it is.
func (r *Repo) Tamper(name string) error {
	targets, err := r.latestTargets()
	if err != nil {
		return err
	}
	tf, ok := targets.Signed.Targets[name]
	if !ok {
		return fmt.Errorf("repotest: no target %s", name)
	}
	file := filepath.Join(r.Dir, "targets", filepath.FromSlash(path.Dir(name)), hex.EncodeToString(tf.Hashes["sha256"])+"."+path.Base(name))
	b, err := os.ReadFile(file)
	if err != nil {
		return err
	}
	b[len(b)/2] ^= 0xff
	return os.WriteFile(file, b, 0o644)
}

// Expire signs a new timestamp that has expired already, as a repository
// whose signing stopped would serve.
func (r *Repo) Expire() error {
	ts, err := (&metadata.Metadata[metadata.TimestampType]{}).FromFile(filepath.Join(r.Dir, "metadata", "timestamp.json"))
	if err != nil {
		return err
	}
	ts.Signed.Version++
	ts.Signed.Expires = r.Now().Add(-time.Hour)
	ts.ClearSignatures()
	if err := r.sign(ts, metadata.TIMESTAMP); err != nil {
		return err
	}
	return ts.ToFile(filepath.Join(r.Dir, "metadata", "timestamp.json"), true)
}

func (r *Repo) latestTargets() (*metadata.Metadata[metadata.TargetsType], error) {
	snap, err := (&metadata.Metadata[metadata.SnapshotType]{}).FromFile(filepath.Join(r.Dir, "metadata", "snapshot.json"))
	if err != nil {
		return nil, err
	}
	v := snap.Signed.Meta["targets.json"].Version
	return (&metadata.Metadata[metadata.TargetsType]{}).FromFile(filepath.Join(r.Dir, "metadata", fmt.Sprintf("%d.targets.json", v)))
}

// write signs targets and new snapshot and timestamp metadata pointing at
// it, and stores all three the way consistent snapshots name them.
func (r *Repo) write(targets *metadata.Metadata[metadata.TargetsType]) error {
	md := filepath.Join(r.Dir, "metadata")
	targets.ClearSignatures()
	if err := r.sign(targets, metadata.TARGETS); err != nil {
		return err
	}
	if err := targets.ToFile(filepath.Join(md, fmt.Sprintf("%d.targets.json", targets.Signed.Version)), true); err != nil {
		return err
	}
	snap := metadata.Snapshot(r.Now().AddDate(0, 0, 7))
	if old, err := (&metadata.Metadata[metadata.SnapshotType]{}).FromFile(filepath.Join(md, "snapshot.json")); err == nil {
		snap.Signed.Version = old.Signed.Version + 1
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	snap.Signed.Meta["targets.json"] = metadata.MetaFile(targets.Signed.Version)
	if err := r.sign(snap, metadata.SNAPSHOT); err != nil {
		return err
	}
	for _, name := range []string{fmt.Sprintf("%d.snapshot.json", snap.Signed.Version), "snapshot.json"} {
		if err := snap.ToFile(filepath.Join(md, name), true); err != nil {
			return err
		}
	}
	ts := metadata.Timestamp(r.Now().AddDate(0, 0, 1))
	if old, err := (&metadata.Metadata[metadata.TimestampType]{}).FromFile(filepath.Join(md, "timestamp.json")); err == nil {
		ts.Signed.Version = old.Signed.Version + 1
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	ts.Signed.Meta["snapshot.json"] = metadata.MetaFile(snap.Signed.Version)
	if err := r.sign(ts, metadata.TIMESTAMP); err != nil {
		return err
	}
	return ts.ToFile(filepath.Join(md, "timestamp.json"), true)
}
