//go:build !linux

package update

import "errors"

// Install updates a script install, which exists on Linux only.
func Install(string, map[string][]byte, func(string) error) error {
	return errors.New("update: Snugbox updates itself on Linux only")
}
