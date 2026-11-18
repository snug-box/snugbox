//go:build !linux

// Command snugbox-helper applies Snugbox's fixes on a Linux server.
package main

import (
	"fmt"
	"os"
)

func main() {
	fmt.Fprintln(os.Stderr, "snugbox-helper runs only on Linux")
	os.Exit(1)
}
