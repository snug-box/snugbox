//go:build linux

package update

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"golang.org/x/sys/unix"

	"snugbox.dev/snugbox/agent/internal/safefs"
)

// Install puts a release's programs in dir, the folder of a script install.
// Each is first written beside the old one under a hidden name, and check
// runs the new snugbox from there; only when it answers is each renamed over
// the old one, in the order of Programs. A rename is atomic, so each program
// is always whole, old or new. No link in dir's path is followed.
func Install(dir string, files map[string][]byte, check func(staged string) error) error {
	d, err := safefs.OpenDir(dir, false, 0)
	if err != nil {
		return fmt.Errorf("update: %w", err)
	}
	defer d.Close()
	staged := make(map[string]string)
	defer func() {
		for _, name := range staged {
			_ = safefs.Remove(d, name)
		}
	}()
	for _, p := range Programs {
		name := fmt.Sprintf(".%s.update-%d", p, os.Getpid())
		if err := safefs.Remove(d, name); err != nil {
			return fmt.Errorf("update: %w", err)
		}
		if err := safefs.CreateNew(d, name, files[p], 0o755); err != nil {
			return fmt.Errorf("update: %w", err)
		}
		staged[p] = name
	}
	if err := check(filepath.Join(dir, staged["snugbox"])); err != nil {
		return fmt.Errorf("update: the new snugbox doesn't run, so nothing was replaced: %w", err)
	}
	var done []string
	for _, p := range Programs {
		if err := unix.Renameat(int(d.Fd()), staged[p], int(d.Fd()), p); err != nil {
			return errors.Join(fmt.Errorf("update: replacing %s failed after %v were replaced: %w", p, done, err))
		}
		delete(staged, p)
		done = append(done, p)
	}
	return d.Sync()
}
