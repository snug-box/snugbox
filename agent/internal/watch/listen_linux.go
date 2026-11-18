//go:build linux

package watch

import (
	"errors"
	"fmt"
	"net"
	"os"
	"syscall"
)

// Listen opens the API socket: a fresh Unix socket at path that only its
// owner and group can open. The umask is set around the bind so the socket is
// never briefly open to others.
func Listen(path string) (net.Listener, error) {
	if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return nil, fmt.Errorf("api socket: %w", err)
	}
	old := syscall.Umask(0o117)
	ln, err := net.Listen("unix", path)
	syscall.Umask(old)
	if err != nil {
		return nil, fmt.Errorf("api socket: %w", err)
	}
	return ln, nil
}
