//go:build !linux

package collect

import (
	"io/fs"
	"os"
)

// LiveRoot is the live root filesystem. Snugbox measures Linux boxes only;
// elsewhere, for development, it is os.DirFS("/").
type LiveRoot struct{ fs.FS }

// NewLiveRoot returns the live root filesystem.
func NewLiveRoot() *LiveRoot { return &LiveRoot{FS: os.DirFS("/")} }

// Close does nothing.
func (*LiveRoot) Close() error { return nil }
