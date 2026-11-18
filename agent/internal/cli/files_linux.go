//go:build linux

package cli

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"golang.org/x/sys/unix"

	"snugbox.dev/snugbox/agent/internal/safefs"
)

// noFollow keeps a write in the current folder from following a link
// planted at the file's name.
const noFollow = unix.O_NOFOLLOW

// saveFile creates a new file in dir, making dir if needed, without
// following a link at any step. The folder may belong to the snugbox user;
// the new file belongs to root.
func saveFile(dir, name string, data []byte) error {
	d, err := safefs.OpenDir(dir, true, 0o750)
	if err != nil {
		return err
	}
	return errors.Join(safefs.CreateNew(d, name, data, 0o640), d.Close())
}

// writeSystemFile replaces a system file such as a unit atomically, owned by
// root.
func writeSystemFile(path string, data []byte, mode os.FileMode) error {
	d, err := safefs.OpenDir(filepath.Dir(path), true, 0o755)
	if err != nil {
		return err
	}
	return errors.Join(safefs.Replace(d, filepath.Base(path), data, mode, safefs.Owner{UID: 0, GID: 0}), d.Close())
}

// createFile creates a file owned by root with exactly mode, making its
// folder 0755 if needed, and reports whether it already existed; an existing
// file is left as it is.
func createFile(path string, data []byte, mode os.FileMode) (existed bool, err error) {
	d, err := safefs.OpenDir(filepath.Dir(path), true, 0o755)
	if err != nil {
		return false, err
	}
	defer d.Close()
	err = safefs.CreateNew(d, filepath.Base(path), data, mode)
	if errors.Is(err, unix.EEXIST) {
		return true, nil
	}
	return false, err
}

// privateDir makes sure path is a folder of this user that nobody else can
// write, making it 0700 if it is missing, without following a link. The
// update cache must be one: a root.json planted there would be trusted.
func privateDir(path string) error {
	d, err := safefs.OpenDir(path, true, 0o700)
	if err != nil {
		return err
	}
	defer d.Close()
	var st unix.Stat_t
	if err := unix.Fstat(int(d.Fd()), &st); err != nil {
		return err
	}
	if int(st.Uid) != os.Geteuid() || st.Mode&0o022 != 0 {
		return fmt.Errorf("%s must belong to root and be writable by root alone, so nothing planted in it is trusted; it is uid %d, mode %o", path, st.Uid, st.Mode&0o7777)
	}
	return nil
}
