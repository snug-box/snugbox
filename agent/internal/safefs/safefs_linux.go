//go:build linux

package safefs

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"golang.org/x/sys/unix"
)

// ErrLink is returned when a path holds a symbolic link where a folder or a
// file was expected.
var ErrLink = errors.New("refusing to follow a symbolic link")

func split(path string) ([]string, error) {
	if !filepath.IsAbs(path) {
		return nil, fmt.Errorf("safefs: %q is not absolute", path)
	}
	clean := filepath.Clean(path)
	if clean == "/" {
		return nil, nil
	}
	return strings.Split(strings.TrimPrefix(clean, "/"), "/"), nil
}

func openDirAt(parent int, name string) (int, error) {
	fd, err := unix.Openat2(parent, name, &unix.OpenHow{
		Flags:   unix.O_RDONLY | unix.O_DIRECTORY | unix.O_NOFOLLOW | unix.O_CLOEXEC,
		Resolve: unix.RESOLVE_NO_SYMLINKS | unix.RESOLVE_BENEATH,
	})
	if errors.Is(err, unix.ENOSYS) || errors.Is(err, unix.EPERM) {
		fd, err = unix.Openat(parent, name, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	}
	if errors.Is(err, unix.ELOOP) || errors.Is(err, unix.ENOTDIR) {
		var st unix.Stat_t
		if unix.Fstatat(parent, name, &st, unix.AT_SYMLINK_NOFOLLOW) == nil && st.Mode&unix.S_IFMT == unix.S_IFLNK {
			return -1, fmt.Errorf("safefs: %s: %w", name, ErrLink)
		}
	}
	return fd, err
}

// OpenDir opens a folder, refusing a symbolic link at every component. With
// create, missing folders are made with exactly mode, whatever the umask,
// owned by the caller.
func OpenDir(path string, create bool, mode os.FileMode) (*os.File, error) {
	parts, err := split(path)
	if err != nil {
		return nil, err
	}
	fd, err := unix.Open("/", unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC, 0)
	if err != nil {
		return nil, fmt.Errorf("safefs: open /: %w", err)
	}
	for _, p := range parts {
		next, err := openDirAt(fd, p)
		if errors.Is(err, unix.ENOENT) && create {
			made := unix.Mkdirat(fd, p, uint32(mode.Perm()))
			if made != nil && !errors.Is(made, unix.EEXIST) {
				return nil, errors.Join(fmt.Errorf("safefs: mkdir %s: %w", p, made), unix.Close(fd))
			}
			next, err = openDirAt(fd, p)
			if err == nil && made == nil {
				// mkdir takes the umask off the mode; a folder other
				// users must read, such as /etc/snugbox, needs it whole.
				if cerr := unix.Fchmod(next, uint32(mode.Perm())); cerr != nil {
					return nil, errors.Join(fmt.Errorf("safefs: chmod %s: %w", p, cerr), unix.Close(next), unix.Close(fd))
				}
			}
		}
		if cerr := unix.Close(fd); cerr != nil && err == nil {
			return nil, errors.Join(fmt.Errorf("safefs: close: %w", cerr), unix.Close(next))
		}
		if err != nil {
			return nil, fmt.Errorf("safefs: %s: %w", path, err)
		}
		fd = next
	}
	return os.NewFile(uintptr(fd), path), nil
}

// CreateNew writes data to a new file in dir with exactly mode, whatever the
// umask. It fails if the name exists in any form, so a planted link or file
// is never written through.
func CreateNew(dir *os.File, name string, data []byte, mode os.FileMode) error {
	if strings.ContainsRune(name, '/') || name == "" || name == "." || name == ".." {
		return fmt.Errorf("safefs: bad file name %q", name)
	}
	fd, err := unix.Openat(int(dir.Fd()), name, unix.O_WRONLY|unix.O_CREAT|unix.O_EXCL|unix.O_NOFOLLOW|unix.O_CLOEXEC, uint32(mode.Perm()))
	if err != nil {
		return fmt.Errorf("safefs: create %s: %w", name, err)
	}
	f := os.NewFile(uintptr(fd), name)
	if _, err := f.Write(data); err != nil {
		return errors.Join(fmt.Errorf("safefs: write %s: %w", name, err), f.Close())
	}
	if err := f.Chmod(mode.Perm()); err != nil {
		return errors.Join(fmt.Errorf("safefs: chmod %s: %w", name, err), f.Close())
	}
	if err := f.Sync(); err != nil {
		return errors.Join(fmt.Errorf("safefs: sync %s: %w", name, err), f.Close())
	}
	return f.Close()
}

// Owner is who a written file belongs to. A negative ID keeps the caller's.
type Owner struct{ UID, GID int }

// Replace writes data to name in dir atomically: a temporary file in the same
// folder, synced, then renamed over the target. The target must be absent or
// a regular file; a link, a folder or a device is refused.
func Replace(dir *os.File, name string, data []byte, mode os.FileMode, owner Owner) error {
	if strings.ContainsRune(name, '/') || name == "" || name == "." || name == ".." {
		return fmt.Errorf("safefs: bad file name %q", name)
	}
	dfd := int(dir.Fd())
	var st unix.Stat_t
	switch err := unix.Fstatat(dfd, name, &st, unix.AT_SYMLINK_NOFOLLOW); {
	case errors.Is(err, unix.ENOENT):
	case err != nil:
		return fmt.Errorf("safefs: stat %s: %w", name, err)
	case st.Mode&unix.S_IFMT == unix.S_IFLNK:
		return fmt.Errorf("safefs: %s: %w", name, ErrLink)
	case st.Mode&unix.S_IFMT != unix.S_IFREG:
		return fmt.Errorf("safefs: %s is not a regular file", name)
	}
	var tmp string
	var fd int
	for i := 0; ; i++ {
		tmp = fmt.Sprintf(".%s.snugbox-%d-%d", name, os.Getpid(), i)
		var err error
		fd, err = unix.Openat(dfd, tmp, unix.O_WRONLY|unix.O_CREAT|unix.O_EXCL|unix.O_NOFOLLOW|unix.O_CLOEXEC, uint32(mode.Perm()))
		if err == nil {
			break
		}
		if !errors.Is(err, unix.EEXIST) || i > 100 {
			return fmt.Errorf("safefs: temporary file for %s: %w", name, err)
		}
	}
	f := os.NewFile(uintptr(fd), tmp)
	cleanup := func(err error) error {
		return errors.Join(err, f.Close(), ignoreMissing(unix.Unlinkat(dfd, tmp, 0)))
	}
	if _, err := f.Write(data); err != nil {
		return cleanup(fmt.Errorf("safefs: write %s: %w", name, err))
	}
	if owner.UID >= 0 || owner.GID >= 0 {
		if err := f.Chown(owner.UID, owner.GID); err != nil {
			return cleanup(fmt.Errorf("safefs: chown %s: %w", name, err))
		}
	}
	if err := f.Chmod(mode.Perm()); err != nil {
		return cleanup(fmt.Errorf("safefs: chmod %s: %w", name, err))
	}
	if err := f.Sync(); err != nil {
		return cleanup(fmt.Errorf("safefs: sync %s: %w", name, err))
	}
	if err := f.Close(); err != nil {
		return errors.Join(fmt.Errorf("safefs: close %s: %w", name, err), ignoreMissing(unix.Unlinkat(dfd, tmp, 0)))
	}
	if err := unix.Renameat(dfd, tmp, dfd, name); err != nil {
		return errors.Join(fmt.Errorf("safefs: rename %s: %w", name, err), ignoreMissing(unix.Unlinkat(dfd, tmp, 0)))
	}
	return dir.Sync()
}

// Read returns a file's contents, refusing a link and anything that is not a
// regular file.
func Read(dir *os.File, name string, limit int64) ([]byte, error) {
	fd, err := unix.Openat(int(dir.Fd()), name, unix.O_RDONLY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil {
		if errors.Is(err, unix.ELOOP) {
			return nil, fmt.Errorf("safefs: %s: %w", name, ErrLink)
		}
		return nil, fmt.Errorf("safefs: open %s: %w", name, err)
	}
	f := os.NewFile(uintptr(fd), name)
	defer f.Close()
	fi, err := f.Stat()
	if err != nil {
		return nil, err
	}
	if !fi.Mode().IsRegular() {
		return nil, fmt.Errorf("safefs: %s is not a regular file", name)
	}
	if fi.Size() > limit {
		return nil, fmt.Errorf("safefs: %s is larger than %d bytes", name, limit)
	}
	buf := make([]byte, fi.Size())
	n, err := f.ReadAt(buf, 0)
	if err != nil && n != len(buf) {
		return nil, err
	}
	return buf[:n], nil
}

// Remove deletes a regular file in dir; a missing file is not an error.
func Remove(dir *os.File, name string) error {
	return ignoreMissing(unix.Unlinkat(int(dir.Fd()), name, 0))
}

// RemoveDir deletes the folder name in dir if it is empty, and reports
// whether it did. A missing folder, or one that still holds anything, is
// left alone without an error. The kernel checks that the folder is empty
// as it removes it, so nothing can slip in between, and it never follows a
// link at name: a link there is refused with ErrLink.
func RemoveDir(dir *os.File, name string) (bool, error) {
	if strings.ContainsRune(name, '/') || name == "" || name == "." || name == ".." {
		return false, fmt.Errorf("safefs: bad folder name %q", name)
	}
	dfd := int(dir.Fd())
	err := unix.Unlinkat(dfd, name, unix.AT_REMOVEDIR)
	switch {
	case err == nil:
		return true, dir.Sync()
	case errors.Is(err, unix.ENOENT), errors.Is(err, unix.ENOTEMPTY), errors.Is(err, unix.EEXIST):
		return false, nil
	case errors.Is(err, unix.ENOTDIR):
		var st unix.Stat_t
		if unix.Fstatat(dfd, name, &st, unix.AT_SYMLINK_NOFOLLOW) == nil && st.Mode&unix.S_IFMT == unix.S_IFLNK {
			return false, fmt.Errorf("safefs: %s: %w", name, ErrLink)
		}
		return false, fmt.Errorf("safefs: %s is not a folder", name)
	}
	return false, fmt.Errorf("safefs: remove %s: %w", name, err)
}

// OpenAppend opens name in dir for reading and appending, creating it with
// mode if it is missing. A link or anything but a regular file owned by the
// caller is refused.
func OpenAppend(dir *os.File, name string, mode os.FileMode) (*os.File, error) {
	if strings.ContainsRune(name, '/') || name == "" || name == "." || name == ".." {
		return nil, fmt.Errorf("safefs: bad file name %q", name)
	}
	fd, err := unix.Openat(int(dir.Fd()), name, unix.O_RDWR|unix.O_APPEND|unix.O_CREAT|unix.O_NOFOLLOW|unix.O_CLOEXEC, uint32(mode.Perm()))
	if err != nil {
		if errors.Is(err, unix.ELOOP) {
			return nil, fmt.Errorf("safefs: %s: %w", name, ErrLink)
		}
		return nil, fmt.Errorf("safefs: open %s: %w", name, err)
	}
	f := os.NewFile(uintptr(fd), name)
	var st unix.Stat_t
	if err := unix.Fstat(fd, &st); err != nil {
		return nil, errors.Join(fmt.Errorf("safefs: stat %s: %w", name, err), f.Close())
	}
	if st.Mode&unix.S_IFMT != unix.S_IFREG || int(st.Uid) != os.Geteuid() {
		return nil, errors.Join(fmt.Errorf("safefs: %s is not a regular file of this user", name), f.Close())
	}
	return f, nil
}

// CreateSized makes a new file of size bytes in dir, its blocks allocated
// with fallocate, and fails if the name exists in any form.
func CreateSized(dir *os.File, name string, size int64, mode os.FileMode) error {
	if strings.ContainsRune(name, '/') || name == "" || name == "." || name == ".." {
		return fmt.Errorf("safefs: bad file name %q", name)
	}
	fd, err := unix.Openat(int(dir.Fd()), name, unix.O_WRONLY|unix.O_CREAT|unix.O_EXCL|unix.O_NOFOLLOW|unix.O_CLOEXEC, uint32(mode.Perm()))
	if err != nil {
		return fmt.Errorf("safefs: create %s: %w", name, err)
	}
	f := os.NewFile(uintptr(fd), name)
	if err := unix.Fallocate(fd, 0, 0, size); err != nil {
		return errors.Join(fmt.Errorf("safefs: allocate %d bytes for %s: %w", size, name, err), f.Close(), Remove(dir, name))
	}
	if err := f.Sync(); err != nil {
		return errors.Join(fmt.Errorf("safefs: sync %s: %w", name, err), f.Close(), Remove(dir, name))
	}
	return f.Close()
}

func ignoreMissing(err error) error {
	if errors.Is(err, unix.ENOENT) {
		return nil
	}
	return err
}
