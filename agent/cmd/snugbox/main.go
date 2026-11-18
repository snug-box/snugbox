// Command snugbox audits a Linux server's memory, applies fixes that can be
// undone, and names the cheapest plan that fits.
package main

import (
	"os"

	"snugbox.dev/snugbox/agent/internal/cli"
)

func main() {
	os.Exit(cli.Main(os.Args[1:], os.Stdout, os.Stderr))
}
