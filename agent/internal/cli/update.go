package cli

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"snugbox.dev/snugbox/agent/internal/units"
	"snugbox.dev/snugbox/agent/internal/update"
	"snugbox.dev/snugbox/agent/internal/version"
)

// UpdateCache is where snugbox self-update keeps the update metadata it has
// trusted so far, a folder only root can write.
const UpdateCache = "/var/lib/snugbox-update"

// packageDir is where the .deb and .rpm packages put the programs; their
// package manager updates them.
const packageDir = "/usr/libexec/snugbox"

// updateEnv is what snugbox self-update reaches outside the process; tests
// replace it.
type updateEnv struct {
	repo  func() (update.Repository, error)
	cache string
	// dir is the folder the running snugbox is in, which an update
	// replaces the programs of.
	dir  func() (string, error)
	http *http.Client
	// version is this build's version.
	version string
	// run runs a program and returns what it printed: the new snugbox, to
	// check it answers, and its watch install afterwards.
	run func(ctx context.Context, name string, args ...string) ([]byte, error)
	// watcherUnit is the watcher's unit a script install writes; when it
	// exists, the watcher is restarted on the new version.
	watcherUnit string
}

func defaultUpdateEnv() updateEnv {
	return updateEnv{
		repo: update.Embedded, cache: UpdateCache, dir: binDir, version: version.Version,
		http: &http.Client{Timeout: 2 * time.Minute}, run: runProgram,
		watcherUnit: filepath.Join(units.LocalUnitDir, units.WatcherName),
	}
}

// runProgram runs a program with a fixed environment and up to two minutes,
// and returns everything it printed.
func runProgram(ctx context.Context, name string, args ...string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	cmd := exec.CommandContext(ctx, name, args...) //nolint:gosec // name is the snugbox that self-update has just verified and installed
	cmd.Env = []string{"PATH=/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin", "LC_ALL=C", "NO_COLOR=1"}
	return cmd.CombinedOutput()
}

func newSelfUpdate(e *env) *cobra.Command {
	var check bool
	channel := "stable"
	cmd := &cobra.Command{
		Use:   "self-update",
		Short: "Update Snugbox to the newest release, verified through The Update Framework",
		Long: "snugbox self-update updates a Snugbox that came from the install script. It fetches the update\n" +
			"repository's signed metadata, checks it against the root of trust built into this release,\n" +
			"and installs a newer release only when its every byte matches that metadata. It never\n" +
			"installs an older release. The watcher is restarted on the new version. A Snugbox from a\n" +
			".deb or .rpm package is updated by apt or dnf instead.",
		Example: "  sudo snugbox self-update --check\n  sudo snugbox self-update\n  sudo snugbox self-update --channel beta",
		Args:    cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if !slices.Contains(update.Channels, channel) {
				return usageError(cmd, fmt.Errorf("--channel is one of %s", strings.Join(update.Channels, ", ")))
			}
			if err := e.requireRoot(); err != nil {
				return err
			}
			return selfUpdate(cmd.Context(), e, check, channel)
		},
	}
	cmd.Flags().BoolVar(&check, "check", false, "say whether a newer release is out, and change nothing")
	cmd.Flags().StringVar(&channel, "channel", channel, "the release channel: stable, beta or nightly")
	return cmd
}

func selfUpdate(ctx context.Context, e *env, check bool, channel string) error {
	u := e.update
	repo, err := u.repo()
	if err != nil {
		return err
	}
	if !update.ValidVersion(u.version) {
		return fmt.Errorf("this snugbox is a development build, %s, so there is no release to compare it with; install a release first", u.version)
	}
	dir, err := u.dir()
	if err != nil {
		return err
	}
	if dir == packageDir {
		return errors.New("this Snugbox came from a .deb or .rpm package, so its package manager updates it: sudo apt upgrade snugbox, or sudo dnf upgrade snugbox")
	}
	if err := privateDir(u.cache); err != nil {
		return err
	}
	rel, err := update.Latest(repo, update.Options{Channel: channel, Arch: runtime.GOARCH, CacheDir: u.cache, HTTP: u.http})
	if err != nil {
		return err
	}
	if !update.Newer(rel.Version, u.version) {
		fmt.Fprintf(e.stdout, "Snugbox %s is the newest release on the %s channel.\n", u.version, channel)
		return nil
	}
	if check {
		fmt.Fprintf(e.stdout, "Snugbox %s is out on the %s channel; this is %s. Install it with sudo snugbox self-update.\n", rel.Version, channel, u.version)
		return nil
	}
	tmp, err := os.MkdirTemp("", "snugbox-update-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(tmp)
	fmt.Fprintf(e.stdout, "Downloading Snugbox %s and checking it against the signed metadata.\n", rel.Version)
	data, err := rel.Download(tmp)
	if err != nil {
		return err
	}
	files, err := update.Unpack(data)
	if err != nil {
		return err
	}
	err = update.Install(dir, files, func(staged string) error {
		out, err := u.run(ctx, staged, "version")
		if err != nil {
			return fmt.Errorf("%w: %s", err, strings.TrimSpace(string(out)))
		}
		if want := "snugbox " + rel.Version + " "; !strings.HasPrefix(string(out), want) {
			return fmt.Errorf("it says %q, not snugbox %s", strings.TrimSpace(string(out)), rel.Version)
		}
		return nil
	})
	if err != nil {
		return err
	}
	fmt.Fprintf(e.stdout, "Installed Snugbox %s in %s, in place of %s.\n", rel.Version, dir, u.version)
	if _, err := os.Stat(u.watcherUnit); err != nil {
		return nil
	}
	out, err := u.run(ctx, filepath.Join(dir, "snugbox"), "watch", "install")
	_, _ = e.stdout.Write(out)
	if err != nil {
		return fmt.Errorf("the new version is installed, but the watcher didn't restart on it: %w; run sudo snugbox watch install", err)
	}
	return nil
}
