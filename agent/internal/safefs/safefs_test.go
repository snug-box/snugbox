//go:build linux

package safefs

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"golang.org/x/sys/unix"
)

func TestCreateAndReplace(t *testing.T) {
	root := t.TempDir()
	d, err := OpenDir(filepath.Join(root, "a", "b"), true, 0o750)
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	if err := CreateNew(d, "report.md", []byte("one"), 0o640); err != nil {
		t.Fatal(err)
	}
	if err := CreateNew(d, "report.md", []byte("two"), 0o640); err == nil {
		t.Error("CreateNew overwrote an existing file")
	}
	if err := Replace(d, "report.md", []byte("three"), 0o600, Owner{-1, -1}); err != nil {
		t.Fatal(err)
	}
	b, err := Read(d, "report.md", 1<<20)
	if err != nil || string(b) != "three" {
		t.Errorf("read %q, %v", b, err)
	}
	fi, err := os.Stat(filepath.Join(root, "a", "b", "report.md"))
	if err != nil || fi.Mode().Perm() != 0o600 {
		t.Errorf("mode %v, %v", fi.Mode(), err)
	}
	entries, err := os.ReadDir(filepath.Join(root, "a", "b"))
	if err != nil || len(entries) != 1 {
		t.Errorf("temporary files left behind: %v, %v", entries, err)
	}
	if err := Remove(d, "report.md"); err != nil {
		t.Fatal(err)
	}
	if err := Remove(d, "report.md"); err != nil {
		t.Errorf("removing a missing file: %v", err)
	}
}

// A link planted at any step of the path is refused, at the folders and at
// the file.
func TestRefusesLinks(t *testing.T) {
	root := t.TempDir()
	target := filepath.Join(root, "target")
	if err := os.MkdirAll(target, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, filepath.Join(root, "linkdir")); err != nil {
		t.Fatal(err)
	}
	if _, err := OpenDir(filepath.Join(root, "linkdir", "x"), true, 0o755); err == nil {
		t.Error("OpenDir followed a link to a folder")
	}
	if _, err := os.Stat(filepath.Join(target, "x")); err == nil {
		t.Error("a folder was made through the link")
	}

	d, err := OpenDir(root, false, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	secret := filepath.Join(target, "secret")
	if err := os.WriteFile(secret, []byte("keep"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(secret, filepath.Join(root, "file")); err != nil {
		t.Fatal(err)
	}
	if err := CreateNew(d, "file", []byte("evil"), 0o644); err == nil {
		t.Error("CreateNew wrote through a link")
	}
	if err := Replace(d, "file", []byte("evil"), 0o644, Owner{-1, -1}); !errors.Is(err, ErrLink) {
		t.Errorf("Replace over a link: %v", err)
	}
	if _, err := Read(d, "file", 100); !errors.Is(err, ErrLink) {
		t.Errorf("Read through a link: %v", err)
	}
	if b, _ := os.ReadFile(secret); string(b) != "keep" {
		t.Errorf("the link's target changed to %q", b)
	}
	if err := os.Mkdir(filepath.Join(root, "dir"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := Replace(d, "dir", []byte("x"), 0o644, Owner{-1, -1}); err == nil {
		t.Error("Replace over a folder")
	}
	for _, bad := range []string{"", ".", "..", "a/b"} {
		if err := CreateNew(d, bad, nil, 0o644); err == nil {
			t.Errorf("CreateNew(%q) accepted", bad)
		}
	}
	if _, err := OpenDir("relative/path", false, 0); err == nil {
		t.Error("a relative path was accepted")
	}
}

// RemoveDir takes away an empty folder only: one with anything in it stays,
// a missing one is no error, and a link in a folder's place is refused with
// the folder it points at left alone.
func TestRemoveDir(t *testing.T) {
	root := t.TempDir()
	d, err := OpenDir(root, false, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	for _, dir := range []string{"empty", "full", "target"} {
		if err := os.Mkdir(filepath.Join(root, dir), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(root, "full", "mine.conf"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(root, "target"), filepath.Join(root, "link")); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "file"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}

	if removed, err := RemoveDir(d, "empty"); !removed || err != nil {
		t.Errorf("empty folder: %v, %v", removed, err)
	}
	if _, err := os.Stat(filepath.Join(root, "empty")); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("the empty folder is still there: %v", err)
	}
	if removed, err := RemoveDir(d, "empty"); removed || err != nil {
		t.Errorf("missing folder: %v, %v", removed, err)
	}
	if removed, err := RemoveDir(d, "full"); removed || err != nil {
		t.Errorf("folder with a file: %v, %v", removed, err)
	}
	if _, err := os.Stat(filepath.Join(root, "full", "mine.conf")); err != nil {
		t.Errorf("the file in a full folder: %v", err)
	}
	if removed, err := RemoveDir(d, "link"); removed || !errors.Is(err, ErrLink) {
		t.Errorf("link to a folder: %v, %v", removed, err)
	}
	if _, err := os.Stat(filepath.Join(root, "target")); err != nil {
		t.Errorf("the link's target: %v", err)
	}
	if removed, err := RemoveDir(d, "file"); removed || err == nil {
		t.Errorf("a file in a folder's place: %v, %v", removed, err)
	}
	for _, bad := range []string{"", ".", "..", "a/b"} {
		if _, err := RemoveDir(d, bad); err == nil {
			t.Errorf("RemoveDir(%q) accepted", bad)
		}
	}
}

// New folders and files get exactly the mode asked for, even when sudo
// passes on a umask of 077, so the watcher can still see /etc/snugbox/paused.
// A folder that was there already keeps its own mode.
func TestModeIgnoresUmask(t *testing.T) {
	root := t.TempDir()
	if err := os.Chmod(root, 0o711); err != nil {
		t.Fatal(err)
	}
	defer unix.Umask(unix.Umask(0o077))
	d, err := OpenDir(filepath.Join(root, "etc", "snugbox"), true, 0o755)
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	if err := CreateNew(d, "paused", []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	for p, want := range map[string]os.FileMode{
		root:                                  0o711,
		filepath.Join(root, "etc"):            0o755,
		filepath.Join(root, "etc", "snugbox"): 0o755,
		filepath.Join(root, "etc", "snugbox", "paused"): 0o644,
	} {
		fi, err := os.Stat(p)
		if err != nil {
			t.Error(err)
			continue
		}
		if got := fi.Mode().Perm(); got != want {
			t.Errorf("%s has mode %v, want %v", p, got, want)
		}
	}
}
