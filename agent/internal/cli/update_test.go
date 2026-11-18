package cli

import (
	"context"
	"testing"

	"snugbox.dev/snugbox/agent/internal/update"
)

func TestSelfUpdateRefusals(t *testing.T) {
	h := newHarness(t)
	h.expect(t, ExitNeedsRoot, []string{"self-update"}, "run it with sudo")
	h.expect(t, ExitUsage, []string{"self-update", "--channel", "edge"}, "--channel is one of stable, beta, nightly")

	h.e.euid = 0
	h.e.update = updateEnv{
		repo:    func() (update.Repository, error) { return update.Repository{}, update.ErrNoTrust },
		version: "0.1.0",
		dir:     func() (string, error) { return "/usr/local/libexec/snugbox", nil },
		run:     func(context.Context, string, ...string) ([]byte, error) { return nil, nil },
	}
	h.expect(t, ExitError, []string{"self-update"}, "carries no update root", "run the install script again")

	h.e.update.repo = func() (update.Repository, error) { return update.Repository{MetadataURL: "https://u.example/m/"}, nil }
	h.e.update.version = "dev"
	h.expect(t, ExitError, []string{"self-update", "--check"}, "a development build, dev")

	h.e.update.version = "0.1.0"
	h.e.update.dir = func() (string, error) { return "/usr/libexec/snugbox", nil }
	h.expect(t, ExitError, []string{"self-update"}, "came from a .deb or .rpm package", "sudo apt upgrade snugbox")
}
