// Command snugboxd is Snugbox's watcher. It samples the box every 10 seconds,
// keeps 30 days of history, works out the snug size, warns before memory
// runs out, and verifies each applied fix, asking snugbox-helper to undo a
// limit that hurts. It reads and never writes system settings; fixes go
// through snugbox-helper, which also answers its questions about Docker.
// systemd runs it as the snugbox user with two read-only capabilities,
// CAP_SYS_PTRACE and CAP_DAC_READ_SEARCH.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"snugbox.dev/snugbox/agent/internal/collect"
	"snugbox.dev/snugbox/agent/internal/helperapi"
	"snugbox.dev/snugbox/agent/internal/localapi"
	"snugbox.dev/snugbox/agent/internal/provider"
	"snugbox.dev/snugbox/agent/internal/version"
	"snugbox.dev/snugbox/agent/internal/watch"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "snugboxd:", err)
		os.Exit(1)
	}
}

// coldAfter is when the watcher marks its own binary pages cold: by then it
// has found its workloads and sampled them, so what start-up alone read in
// is done with.
const coldAfter = 2 * time.Minute

func run() error {
	cfg := watch.Config{}
	flag.StringVar(&cfg.DB, "db", localapi.DefaultDB, "the history database")
	flag.StringVar(&cfg.Socket, "socket", localapi.DefaultSocket, "the local API socket")
	flag.StringVar(&cfg.Pause, "pause-file", localapi.DefaultPause, "the file whose presence means snugbox pause is in force")
	flag.BoolVar(&cfg.Offline, "offline", false, "never ask the cloud metadata service; SNUGBOX_OFFLINE=true does the same")
	showVersion := flag.Bool("version", false, "print the version and exit")
	flag.Parse()
	if *showVersion {
		_, err := fmt.Fprintf(os.Stdout, "snugboxd %s (commit %s, built %s)\n", version.Version, version.Commit, version.Date)
		return err
	}
	offline, offlineWarning := offlineSetting(os.Getenv("SNUGBOX_OFFLINE"))
	cfg.Offline = cfg.Offline || offline
	cfg.Webhook = os.Getenv("SNUGBOX_WEBHOOK_URL")

	level := slog.LevelInfo
	if strings.EqualFold(os.Getenv("SNUGBOX_LOG_LEVEL"), "debug") {
		level = slog.LevelDebug
	}
	log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: level}))
	if offlineWarning != "" {
		log.Warn(offlineWarning)
	}
	if err := adviseRandomReads(); err != nil {
		log.Debug("could not turn off read-ahead in the watcher's own binary", "err", err)
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	root := collect.NewLiveRoot()
	defer root.Close()
	helper := &helperapi.Client{}
	c, err := collect.New(collect.Options{FS: root, Containers: watch.NewHelperContainers(helper, time.Now, log, root)})
	if err != nil {
		return err
	}
	d, err := watch.New(ctx, cfg, watch.Deps{
		Collector: c,
		Host:      collect.LiveHost{},
		Prober:    provider.NewProber(root),
		FS:        root,
		Now:       time.Now,
		Log:       log,
		Helper:    helper,
	})
	if err != nil {
		return err
	}
	ln, err := watch.Listen(cfg.Socket)
	if err != nil {
		return errors.Join(err, d.Close(context.Background()))
	}
	served := make(chan error, 1)
	go func() { served <- d.Serve(ctx, ln) }()

	log.Info("snugboxd starting", "version", version.Version, "db", cfg.DB, "socket", cfg.Socket)
	go func() {
		select {
		case <-ctx.Done():
		case <-time.After(coldAfter):
			if err := adviseCold(); err != nil {
				log.Debug("could not mark the watcher's own binary cold", "err", err)
			}
		}
	}()
	runErr := d.Run(ctx)
	stop()
	closeCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	return errors.Join(runErr, <-served, d.Close(closeCtx))
}

// offlineSetting reads SNUGBOX_OFFLINE, which systemd passes from
// /etc/snugbox/snugboxd.env. It takes systemd's words for true and false. A
// value it doesn't know counts as offline, since whoever set it meant to
// change something, and the warning says so.
func offlineSetting(v string) (offline bool, warning string) {
	switch strings.ToLower(strings.TrimSpace(v)) {
	case "", "0", "n", "no", "f", "false", "off":
		return false, ""
	case "1", "y", "yes", "t", "true", "on":
		return true, ""
	}
	return true, fmt.Sprintf("SNUGBOX_OFFLINE is %q, which is neither true nor false; staying offline", v)
}
