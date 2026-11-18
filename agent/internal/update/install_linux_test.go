//go:build linux

package update_test

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"snugbox.dev/snugbox/agent/internal/update"
)

func installed(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	for _, p := range update.Programs {
		if err := os.WriteFile(filepath.Join(dir, p), []byte("old "+p), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

func newFiles() map[string][]byte {
	out := make(map[string][]byte)
	for _, p := range update.Programs {
		out[p] = []byte("new " + p)
	}
	return out
}

func TestInstallReplacesEveryProgram(t *testing.T) {
	dir := installed(t)
	var checked string
	if err := update.Install(dir, newFiles(), func(staged string) error {
		b, err := os.ReadFile(staged)
		checked = string(b)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if checked != "new snugbox" {
		t.Errorf("the check ran on %q", checked)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != len(update.Programs) {
		t.Errorf("files left behind: %v", entries)
	}
	for _, p := range update.Programs {
		b, err := os.ReadFile(filepath.Join(dir, p))
		fi, _ := os.Stat(filepath.Join(dir, p))
		if err != nil || string(b) != "new "+p || fi.Mode().Perm() != 0o755 {
			t.Errorf("%s: %q, %v, %v", p, b, fi.Mode(), err)
		}
	}
}

// A new snugbox that doesn't run replaces nothing.
func TestInstallStopsWhenTheCheckFails(t *testing.T) {
	dir := installed(t)
	err := update.Install(dir, newFiles(), func(string) error { return errors.New("exec format error") })
	if err == nil || !strings.Contains(err.Error(), "nothing was replaced") {
		t.Fatalf("install: %v", err)
	}
	entries, _ := os.ReadDir(dir)
	if len(entries) != len(update.Programs) {
		t.Errorf("staged files left behind: %v", entries)
	}
	for _, p := range update.Programs {
		if b, _ := os.ReadFile(filepath.Join(dir, p)); string(b) != "old "+p {
			t.Errorf("%s is %q after a failed check", p, b)
		}
	}
}

func TestInstallRefusesALinkedFolder(t *testing.T) {
	dir := installed(t)
	link := filepath.Join(t.TempDir(), "libexec")
	if err := os.Symlink(dir, link); err != nil {
		t.Fatal(err)
	}
	if err := update.Install(link, newFiles(), func(string) error { return nil }); err == nil {
		t.Error("install went through a link")
	}
}
