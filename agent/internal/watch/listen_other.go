//go:build !linux

package watch

import (
	"errors"
	"net"
)

// Listen is available on Linux only, where snugboxd runs.
func Listen(string) (net.Listener, error) {
	return nil, errors.New("snugboxd runs on Linux only")
}
