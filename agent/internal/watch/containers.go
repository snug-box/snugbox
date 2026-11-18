package watch

import (
	"context"
	"errors"
	"io/fs"
	"log/slog"
	"strings"
	"sync"
	"time"

	"snugbox.dev/snugbox/agent/internal/collect"
	"snugbox.dev/snugbox/agent/internal/docker"
	"snugbox.dev/snugbox/agent/internal/model"
	helperv1 "snugbox.dev/snugbox/gen/snugbox/helper/v1"
)

// Helper is the part of snugbox-helper's protocol the watcher uses.
// *helperapi.Client implements it; tests replace it.
type Helper interface {
	List(ctx context.Context) ([]*helperv1.Transaction, error)
	Containers(ctx context.Context) (*helperv1.ContainersResponse, error)
	Advance(ctx context.Context, tx string, to helperv1.State, reason string) (*helperv1.Transaction, error)
	Undo(ctx context.Context, tx, reason string, progress func(*helperv1.Progress)) (*helperv1.UndoResponse, error)
}

// ContainerRefresh is how long the watcher keeps the helper's answer about
// Docker containers, blueprint section 5.5's "every 5 min".
const ContainerRefresh = 5 * time.Minute

// containerRetry is how soon a failed question to the helper is asked again.
const containerRetry = time.Minute

// Even when local file metadata is unchanged, confirm the helper's view.
const helperResync = time.Hour

// HelperContainers names containers through snugbox-helper, the only part
// of Snugbox allowed to talk to Docker (blueprint section 5.4). It keeps an
// answer for ContainerRefresh and asks sooner when a container it has not
// asked about appears in the cgroup tree, which is how a new container gets
// its name within a minute.
type HelperContainers struct {
	h    Helper
	now  func() time.Time
	log  *slog.Logger
	root fs.FS

	mu    sync.Mutex
	next  time.Time
	read  time.Time
	meta  map[string]collect.ContainerMeta
	asked map[string]bool
}

// NewHelperContainers returns a container source that asks h.
func NewHelperContainers(h Helper, now func() time.Time, log *slog.Logger, root fs.FS) *HelperContainers {
	return &HelperContainers{h: h, now: now, log: log, root: root}
}

// metadataUnchanged checks timestamps only. Docker owns the inventory and
// its settings. A missing state file, custom data root or restarting
// container falls back to querying Docker through the helper.
func (c *HelperContainers) metadataUnchanged(now time.Time) bool {
	if c.root == nil || c.read.IsZero() || now.Before(c.read) || now.Sub(c.read) >= helperResync {
		return false
	}
	socket, err := fs.Stat(c.root, strings.TrimPrefix(docker.DefaultSocket, "/"))
	if errors.Is(err, fs.ErrNotExist) {
		return len(c.meta) == 0
	}
	if err != nil || !socket.ModTime().Before(c.read) || len(c.meta) == 0 {
		return false
	}
	const dir = "var/lib/docker/containers"
	older := func(path string) bool {
		info, err := fs.Stat(c.root, path)
		return err == nil && info.ModTime().Before(c.read)
	}
	if !older(dir) {
		return false
	}
	for id, m := range c.meta {
		if m.Restarting || !docker.ValidID(id) || !older(dir+"/"+id+"/config.v2.json") || !older(dir+"/"+id+"/hostconfig.json") {
			return false
		}
	}
	return true
}

// Containers implements collect.ContainerSource.
func (c *HelperContainers) Containers(ctx context.Context, ids []string) (map[string]collect.ContainerMeta, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	now := c.now()
	unknown := false
	for _, id := range ids {
		unknown = unknown || !c.asked[id]
	}
	if !unknown && c.meta != nil {
		if now.Before(c.next) {
			return c.meta, nil
		}
		if c.metadataUnchanged(now) {
			c.next = now.Add(ContainerRefresh)
			return c.meta, nil
		}
	}
	asked := make(map[string]bool, len(ids))
	for _, id := range ids {
		asked[id] = true
	}
	resp, err := c.h.Containers(ctx)
	if err != nil {
		c.read = time.Time{}
		c.asked, c.next = asked, now.Add(containerRetry)
		if c.meta != nil {
			c.log.Warn("could not ask snugbox-helper about containers; keeping the last answer", "err", err)
			return c.meta, nil
		}
		return nil, err
	}
	meta := make(map[string]collect.ContainerMeta, len(resp.GetContainers()))
	for _, ct := range resp.GetContainers() {
		m := collect.ContainerMeta{
			Name:        ct.GetName(),
			Image:       ct.GetImage(),
			Compose:     model.Compose{Project: ct.GetComposeProject(), Service: ct.GetComposeService(), WorkingDir: ct.GetComposeWorkingDir()},
			Reservation: uint64(max(ct.GetMemoryReservationBytes(), 0)),
			Restarts:    int(ct.GetRestartCount()),
			Restarting:  ct.GetRestarting(),
		}
		if ct.GetStarted().IsValid() {
			m.Started = ct.GetStarted().AsTime()
		}
		meta[ct.GetId()] = m
	}
	c.meta, c.asked, c.next, c.read = meta, asked, now.Add(ContainerRefresh), now
	return meta, nil
}

// Fresh asks the helper about containers now, for verification, which
// needs Docker's restart count and out-of-memory flag of the containers a
// fix changed, and returns Docker's view of each container by ID.
func (c *HelperContainers) Fresh(ctx context.Context) (map[string]*helperv1.Container, error) {
	resp, err := c.h.Containers(ctx)
	if err != nil {
		return nil, err
	}
	out := make(map[string]*helperv1.Container, len(resp.GetContainers()))
	for _, ct := range resp.GetContainers() {
		out[ct.GetId()] = ct
	}
	return out, nil
}
