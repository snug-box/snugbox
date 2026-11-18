//go:build !windows

package localapi

import "syscall"

// refused are the errors a dial gets when nothing listens at the socket's
// path: a socket file left behind by a watcher that stopped, or none at all.
var refused = []error{syscall.ECONNREFUSED, syscall.ENOENT}
