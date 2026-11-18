package watch

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"strings"
	"testing"
	"testing/fstest"
	"time"

	helperv1 "snugbox.dev/snugbox/gen/snugbox/helper/v1"
)

type countingHelper struct {
	fakeHelper
	asks int
	fail bool
}

func TestContainerCacheChecksDockerMetadata(t *testing.T) {
	clk := &fakeClock{now: time.Date(2026, 11, 18, 9, 0, 0, 0, time.UTC)}
	id := strings.Repeat("b", 64)
	dir := "var/lib/docker/containers/" + id
	root := fstest.MapFS{
		"var/run/docker.sock": {}, dir + "/config.v2.json": {}, dir + "/hostconfig.json": {},
	}
	h := &countingHelper{}
	h.cts = []*helperv1.Container{{Id: id, Name: "app"}}
	src := NewHelperContainers(h, clk.Now, slog.New(slog.NewTextHandler(io.Discard, nil)), root)
	ask := func(want int) {
		t.Helper()
		if _, err := src.Containers(context.Background(), []string{id}); err != nil || h.asks != want {
			t.Fatalf("helper calls %d, want %d, error %v", h.asks, want, err)
		}
	}
	ask(1)
	clk.now = clk.now.Add(ContainerRefresh)
	ask(1)
	root[dir+"/hostconfig.json"].ModTime = clk.now
	clk.now = clk.now.Add(ContainerRefresh)
	ask(2)
	clk.now = clk.now.Add(helperResync)
	ask(3)
	delete(root, dir+"/config.v2.json")
	clk.now = clk.now.Add(ContainerRefresh)
	ask(4)
}

func (c *countingHelper) Containers(ctx context.Context) (*helperv1.ContainersResponse, error) {
	c.asks++
	if c.fail {
		return nil, errors.New("snugbox-helper is not running")
	}
	return c.fakeHelper.Containers(ctx)
}

func TestHelperContainersAsksRarely(t *testing.T) {
	a, b := strings.Repeat("a", 64), strings.Repeat("b", 64)
	clk := &fakeClock{now: time.Date(2026, 10, 31, 9, 0, 0, 0, time.UTC)}
	h := &countingHelper{}
	h.cts = []*helperv1.Container{{Id: a, Name: "shop-web-1", ComposeProject: "shop", ComposeService: "web", MemoryReservationBytes: 300 << 20, RestartCount: 2}}
	src := NewHelperContainers(h, clk.Now, slog.New(slog.NewTextHandler(io.Discard, nil)), nil)
	ctx := context.Background()

	meta, err := src.Containers(ctx, []string{a})
	if err != nil || meta[a].Name != "shop-web-1" || meta[a].Compose.Service != "web" || meta[a].Reservation != 300<<20 || meta[a].Restarts != 2 {
		t.Fatalf("first answer %+v, %v", meta, err)
	}
	clk.now = clk.now.Add(time.Minute)
	if _, err := src.Containers(ctx, []string{a}); err != nil || h.asks != 1 {
		t.Errorf("asked %d times for a container it knew, %v", h.asks, err)
	}
	if _, err := src.Containers(ctx, []string{a, b}); err != nil || h.asks != 2 {
		t.Errorf("a new container: asked %d times, %v", h.asks, err)
	}
	clk.now = clk.now.Add(time.Minute)
	if _, err := src.Containers(ctx, []string{a, b}); err != nil || h.asks != 2 {
		t.Errorf("a container Docker doesn't know made it ask again: %d asks", h.asks)
	}
	clk.now = clk.now.Add(ContainerRefresh)
	h.fail = true
	meta, err = src.Containers(ctx, []string{a, b})
	if err != nil || h.asks != 3 || meta[a].Name != "shop-web-1" {
		t.Errorf("after a failed ask: %+v, %v, %d asks; want the last answer kept", meta, err, h.asks)
	}
	clk.now = clk.now.Add(30 * time.Second)
	if _, err := src.Containers(ctx, []string{a, b}); err != nil || h.asks != 3 {
		t.Errorf("asked again %d times within a minute of a failure, %v", h.asks-3, err)
	}
	clk.now = clk.now.Add(time.Minute)
	if _, err := src.Containers(ctx, []string{a, b}); err != nil || h.asks != 4 {
		t.Errorf("did not ask again a minute after a failure: %d asks, %v", h.asks, err)
	}

	fresh := NewHelperContainers(&countingHelper{fail: true}, clk.Now, slog.New(slog.NewTextHandler(io.Discard, nil)), nil)
	if _, err := fresh.Containers(ctx, []string{a}); err == nil {
		t.Error("with no earlier answer, a failed ask must be an error, so the collector records the gap")
	}
}
