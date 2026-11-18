package helperapi

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"syscall"
	"time"

	helperv1 "snugbox.dev/snugbox/gen/snugbox/helper/v1"
)

// DefaultSocket is where systemd listens for the helper.
const DefaultSocket = "/run/snugbox/helper.sock"

// StateDir holds the helper's root-owned state.
const StateDir = "/var/lib/snugbox-helper"

// JournalPath is the helper's append-only transaction log.
const JournalPath = StateDir + "/journal"

// ErrNoHelper means nothing listens on the helper's socket.
var ErrNoHelper = errors.New("snugbox-helper is not installed or its socket is not running; run sudo snugbox watch install")

// Client sends requests to the helper. The zero value talks to the installed
// helper.
type Client struct {
	Socket string
	// ServerUID is who must own the listening socket: root, which systemd
	// runs as. Tests set their own user ID.
	ServerUID int
}

func (c *Client) socket() string {
	if c.Socket == "" {
		return DefaultSocket
	}
	return c.Socket
}

// Do sends req and returns the helper's final answer, passing each progress
// step to progress, which may be nil. A refusal or failure comes back as an
// *Error.
func (c *Client) Do(ctx context.Context, req *helperv1.Request, progress func(*helperv1.Progress)) (*helperv1.Response, error) {
	conn, err := dial(ctx, c.socket(), c.ServerUID)
	if err != nil {
		var errno syscall.Errno
		if errors.Is(err, os.ErrNotExist) || (errors.As(err, &errno) && (errno == syscall.ECONNREFUSED || errno == syscall.ENOENT)) {
			return nil, ErrNoHelper
		}
		if errors.Is(err, os.ErrPermission) {
			return nil, errors.New("can't open the helper's socket; this needs root")
		}
		return nil, err
	}
	defer conn.Close()
	stop := context.AfterFunc(ctx, func() { _ = conn.SetDeadline(time.Now()) })
	defer stop()
	if err := WriteFrame(conn, req, MaxRequest); err != nil {
		return nil, err
	}
	r := bufio.NewReader(conn)
	for {
		var resp helperv1.Response
		if err := ReadFrame(r, &resp, MaxResponse); err != nil {
			if ctx.Err() != nil {
				return nil, ctx.Err()
			}
			if errors.Is(err, io.EOF) {
				return nil, errors.New("snugbox-helper closed the connection without an answer; see journalctl -u snugbox-helper")
			}
			return nil, err
		}
		switch k := resp.GetKind().(type) {
		case *helperv1.Response_Progress:
			if progress != nil {
				progress(k.Progress)
			}
		case *helperv1.Response_Error:
			return nil, &Error{Code: k.Error.GetCode(), Message: k.Error.GetMessage()}
		case nil:
			return nil, fmt.Errorf("snugbox-helper sent an empty answer")
		default:
			return &resp, nil
		}
	}
}

// List returns the helper's transactions, newest first.
func (c *Client) List(ctx context.Context) ([]*helperv1.Transaction, error) {
	resp, err := c.Do(ctx, &helperv1.Request{Kind: &helperv1.Request_List{List: &helperv1.ListRequest{}}}, nil)
	if err != nil {
		return nil, err
	}
	l := resp.GetList()
	if l == nil {
		return nil, errors.New("snugbox-helper answered a list request with something else")
	}
	return l.GetTransactions(), nil
}

// Containers asks the helper for the Docker containers' names, labels and
// limits.
func (c *Client) Containers(ctx context.Context) (*helperv1.ContainersResponse, error) {
	resp, err := c.Do(ctx, &helperv1.Request{Kind: &helperv1.Request_Containers{Containers: &helperv1.ContainersRequest{}}}, nil)
	if err != nil {
		return nil, err
	}
	if resp.GetContainers() == nil {
		return nil, errors.New("snugbox-helper answered a containers request with something else")
	}
	return resp.GetContainers(), nil
}

// Advance moves a transaction on after watching it.
func (c *Client) Advance(ctx context.Context, tx string, to helperv1.State, reason string) (*helperv1.Transaction, error) {
	resp, err := c.Do(ctx, &helperv1.Request{Kind: &helperv1.Request_Advance{Advance: &helperv1.AdvanceRequest{Transaction: tx, State: to, Reason: reason}}}, nil)
	if err != nil {
		return nil, err
	}
	if resp.GetAdvance() == nil {
		return nil, errors.New("snugbox-helper answered an advance request with something else")
	}
	return resp.GetAdvance().GetTransaction(), nil
}

// Undo puts a transaction back. An empty tx means the newest one that can
// be undone.
func (c *Client) Undo(ctx context.Context, tx, reason string, progress func(*helperv1.Progress)) (*helperv1.UndoResponse, error) {
	resp, err := c.Do(ctx, &helperv1.Request{Kind: &helperv1.Request_Undo{Undo: &helperv1.UndoRequest{Transaction: tx, Reason: reason}}}, progress)
	if err != nil {
		return nil, err
	}
	if resp.GetUndo() == nil {
		return nil, errors.New("snugbox-helper answered an undo request with something else")
	}
	return resp.GetUndo(), nil
}

// Apply runs a fix, or checks it without changing anything when dryRun is
// set.
func (c *Client) Apply(ctx context.Context, req *helperv1.ApplyRequest, progress func(*helperv1.Progress)) (*helperv1.ApplyResponse, error) {
	resp, err := c.Do(ctx, &helperv1.Request{Kind: &helperv1.Request_Apply{Apply: req}}, progress)
	if err != nil {
		return nil, err
	}
	if resp.GetApply() == nil {
		return nil, errors.New("snugbox-helper answered an apply request with something else")
	}
	return resp.GetApply(), nil
}
