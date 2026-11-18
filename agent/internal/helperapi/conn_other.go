//go:build !linux

package helperapi

import (
	"context"
	"errors"
	"net"
)

func dial(context.Context, string, int) (*net.UnixConn, error) {
	return nil, errors.New("snugbox-helper runs only on Linux")
}
