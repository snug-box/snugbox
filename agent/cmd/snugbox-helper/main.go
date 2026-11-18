//go:build linux

// Command snugbox-helper is the only part of Snugbox that changes the box.
// systemd starts it as root when a request arrives on
// /run/snugbox/helper.sock, and it exits after 60 seconds without one.
// It accepts requests only from root and the snugbox user, checks the
// sender of every message, and runs nothing outside the fix catalogue.
//
// Two verbs are for systemd-run only, which starts them as the owner of a
// Compose project folder: write-override and remove-override.
package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"net"
	"os"
	"os/signal"
	"os/user"
	"strconv"
	"syscall"
	"time"

	"snugbox.dev/snugbox/agent/internal/helper"
	"snugbox.dev/snugbox/agent/internal/helperapi"
	"snugbox.dev/snugbox/agent/internal/journal"
	"snugbox.dev/snugbox/agent/internal/kernel"
	"snugbox.dev/snugbox/agent/internal/localapi"
	"snugbox.dev/snugbox/agent/internal/safefs"
	"snugbox.dev/snugbox/agent/internal/version"
	helperv1 "snugbox.dev/snugbox/gen/snugbox/helper/v1"
)

const (
	idleExit     = 60 * time.Second
	readTimeout  = 5 * time.Second
	writeTimeout = 10 * time.Second
	requestLimit = 15 * time.Minute
	overrideName = "compose.override.yaml"
	maxOverride  = 64 << 10
)

func main() {
	if len(os.Args) > 1 {
		switch os.Args[1] {
		case "write-override":
			os.Exit(writeOverride(os.Args[2:], os.Stdin, os.Stderr))
		case "remove-override":
			os.Exit(removeOverride(os.Args[2:], os.Stdout, os.Stderr))
		case "--version", "-version", "version":
			fmt.Fprintf(os.Stdout, "snugbox-helper %s (commit %s, built %s)\n", version.Version, version.Commit, version.Date)
			return
		}
		fmt.Fprintln(os.Stderr, "snugbox-helper takes no arguments; systemd starts it through snugbox-helper.socket")
		os.Exit(2)
	}
	log := slog.New(slog.NewTextHandler(os.Stderr, nil))
	if err := serve(log); err != nil {
		log.Error("snugbox-helper stopped", "err", err)
		os.Exit(1)
	}
}

func serve(log *slog.Logger) error {
	if os.Geteuid() != 0 {
		return errors.New("snugbox-helper must run as root, started by systemd")
	}
	ln, err := helperapi.ActivationListener()
	if err != nil {
		return err
	}
	defer ln.Close()
	if err := helperapi.PassCredentials(ln); err != nil {
		return fmt.Errorf("SO_PASSCRED: %w", err)
	}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, os.Interrupt)
	defer stop()
	// systemd stops the helper with SIGTERM. Closing this process's copy of
	// the socket wakes Accept at once; systemd keeps listening on its own.
	unblock := context.AfterFunc(ctx, func() { _ = ln.Close() })
	defer unblock()

	snugbox := -1
	if u, err := user.Lookup("snugbox"); err == nil {
		if id, err := strconv.Atoi(u.Uid); err == nil && id > 0 {
			snugbox = id
		}
	}
	handle, err := setup(ctx, log)
	if err != nil {
		var be *journal.BrokenError
		if !errors.As(err, &be) {
			return err
		}
		log.Error("the journal fails its check; every request is refused until a person looks", "err", err)
		msg := fmt.Sprintf("the journal %s fails its check (%v), so the helper changes nothing; see https://snugbox.dev/docs/journal/", helper.Journal, err)
		handle = func(context.Context, *helperv1.Request, helper.Caller, func(*helperv1.Progress)) *helperv1.Response {
			return &helperv1.Response{Kind: &helperv1.Response_Error{Error: &helperv1.Error{Code: helperv1.ErrorCode_ERROR_CODE_STUCK, Message: msg}}}
		}
	}
	for {
		if ctx.Err() != nil {
			return nil
		}
		if err := ln.SetDeadline(time.Now().Add(idleExit)); err != nil {
			if ctx.Err() != nil {
				return nil
			}
			return err
		}
		c, err := helperapi.Accept(ln)
		var ne net.Error
		switch {
		case ctx.Err() != nil:
			if c != nil {
				_ = c.Close()
			}
			return nil
		case errors.As(err, &ne) && ne.Timeout():
			log.Debug("idle; exiting until the next request")
			return nil
		case err != nil:
			log.Warn("accept failed", "err", err)
			continue
		}
		serveConn(ctx, log, c, snugbox, handle)
	}
}

type handler func(context.Context, *helperv1.Request, helper.Caller, func(*helperv1.Progress)) *helperv1.Response

// setup opens the journal, finishes any transaction a stop cut short, and
// returns the engine's handler.
func setup(ctx context.Context, log *slog.Logger) (handler, error) {
	jr, torn, err := helper.OpenJournal()
	if err != nil {
		return nil, err
	}
	if torn > 0 {
		log.Warn("the last journal line was cut short by a crash and was removed", "bytes", torn)
	}
	raw, err := os.ReadFile("/proc/meminfo")
	if err != nil {
		return nil, err
	}
	mi, _ := kernel.ParseMeminfo(raw)
	exe, err := os.Executable()
	if err != nil {
		return nil, err
	}
	e := helper.New(helper.NewLive(), jr, helper.Config{Exe: exe, Pause: localapi.DefaultPause, MemTotal: mi.Get("MemTotal"), Log: log})
	if err := e.Recover(ctx); err != nil {
		return nil, err
	}
	return e.Handle, nil
}

func reply(c *helperapi.Conn, r *helperv1.Response) error {
	if err := c.SetWriteDeadline(time.Now().Add(writeTimeout)); err != nil {
		return err
	}
	return helperapi.WriteFrame(c, r, helperapi.MaxResponse)
}

func refusal(code helperv1.ErrorCode, msg string) *helperv1.Response {
	return &helperv1.Response{Kind: &helperv1.Response_Error{Error: &helperv1.Error{Code: code, Message: msg}}}
}

func serveConn(ctx context.Context, log *slog.Logger, c *helperapi.Conn, snugbox int, handle handler) {
	defer c.Close()
	var by helper.Caller
	switch {
	case c.Peer.UID == 0:
		by = helper.Root
	case snugbox > 0 && c.Peer.UID == snugbox:
		by = helper.Watcher
	default:
		log.Warn("refused a caller", "uid", c.Peer.UID, "pid", c.Peer.PID)
		if err := reply(c, refusal(helperv1.ErrorCode_ERROR_CODE_DENIED, "only root and the snugbox user may use the helper")); err != nil {
			log.Debug("could not tell the caller", "err", err)
		}
		return
	}
	if err := c.SetReadDeadline(time.Now().Add(readTimeout)); err != nil {
		return
	}
	var req helperv1.Request
	if err := helperapi.ReadFrame(c.Reader(), &req, helperapi.MaxRequest); err != nil {
		log.Warn("refused a request", "uid", c.Peer.UID, "pid", c.Peer.PID, "err", err)
		if rerr := reply(c, refusal(helperv1.ErrorCode_ERROR_CODE_INVALID, err.Error())); rerr != nil {
			log.Debug("could not tell the caller", "err", rerr)
		}
		return
	}
	ctx, cancel := context.WithTimeout(ctx, requestLimit)
	defer cancel()
	gone := false
	progress := func(p *helperv1.Progress) {
		if gone {
			return
		}
		if err := reply(c, &helperv1.Response{Kind: &helperv1.Response_Progress{Progress: p}}); err != nil {
			gone = true
			log.Info("the client left; the request runs to its end", "err", err)
		}
	}
	resp := handle(ctx, &req, by, progress)
	if !gone {
		if err := reply(c, resp); err != nil {
			log.Info("the client left before the answer", "err", err)
		}
	}
}

// writeOverride creates dir/compose.override.yaml from in. It runs as the
// folder's owner, so a link planted in the folder gains nothing, and it
// never replaces a file.
func writeOverride(args []string, in io.Reader, stderr io.Writer) int {
	if len(args) != 2 || args[1] != overrideName {
		fmt.Fprintln(stderr, "usage: snugbox-helper write-override <folder> "+overrideName)
		return 2
	}
	data, err := io.ReadAll(io.LimitReader(in, maxOverride+1))
	if err != nil || len(data) > maxOverride {
		fmt.Fprintln(stderr, "snugbox-helper: the override must be under 64 KiB")
		return 1
	}
	dir, err := safefs.OpenDir(args[0], false, 0)
	if err != nil {
		fmt.Fprintln(stderr, "snugbox-helper:", err)
		return 1
	}
	defer dir.Close()
	if err := safefs.CreateNew(dir, overrideName, data, 0o644); err != nil {
		fmt.Fprintln(stderr, "snugbox-helper:", err)
		return 1
	}
	return 0
}

// removeOverride deletes dir/compose.override.yaml if it still holds what
// Snugbox wrote, whose SHA-256 is the third argument. A changed file is left
// alone, and a sentence on stdout says so.
func removeOverride(args []string, stdout, stderr io.Writer) int {
	if len(args) != 3 || args[1] != overrideName || len(args[2]) != 2*sha256.Size {
		fmt.Fprintln(stderr, "usage: snugbox-helper remove-override <folder> "+overrideName+" <sha256>")
		return 2
	}
	dir, err := safefs.OpenDir(args[0], false, 0)
	if errors.Is(err, fs.ErrNotExist) {
		return 0
	}
	if err != nil {
		fmt.Fprintln(stderr, "snugbox-helper:", err)
		return 1
	}
	defer dir.Close()
	b, err := safefs.Read(dir, overrideName, maxOverride)
	if errors.Is(err, fs.ErrNotExist) {
		return 0
	}
	if err != nil {
		fmt.Fprintln(stderr, "snugbox-helper:", err)
		return 1
	}
	sum := sha256.Sum256(b)
	if hex.EncodeToString(sum[:]) != args[2] {
		// The helper reads this line back through systemd-run --pipe and
		// passes it to the person as a note.
		fmt.Fprintf(stdout, "%s/%s was changed after Snugbox wrote it, so it was left as it is.\n", args[0], overrideName)
		return 0
	}
	if err := safefs.Remove(dir, overrideName); err != nil {
		fmt.Fprintln(stderr, "snugbox-helper:", err)
		return 1
	}
	return 0
}
