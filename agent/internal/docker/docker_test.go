package docker

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

const testID = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"

type fakeEngine struct {
	mu      sync.Mutex
	api     string
	updates []MemoryUpdate
	bigList bool
}

func (f *fakeEngine) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	switch {
	case r.URL.Path == "/version":
		fmt.Fprintf(w, `{"Version":"29.9.2","ApiVersion":%q,"MinAPIVersion":"1.24"}`, f.api)
	case r.URL.Path == "/v1.44/containers/json" && r.URL.Query().Get("all") == "1":
		if f.bigList {
			fmt.Fprintf(w, `[{"Id":%q,"Names":["/%s"]}]`, testID, strings.Repeat("x", maxListBytes))
			return
		}
		fmt.Fprintf(w, `[{"Id":%q,"Names":["/shop-app-1"],"Image":"node:24","State":"running","Labels":{"com.docker.compose.project":"shop"}}]`, testID)
	case r.URL.Path == "/v1.44/containers/"+testID+"/json":
		fmt.Fprintf(w, `{"Id":%q,"Name":"/shop-app-1","RestartCount":2,"State":{"Status":"running","Running":true,"OOMKilled":true,"StartedAt":"2026-10-30T08:00:00.123Z"},"Config":{"Image":"node:24","Labels":{"com.docker.compose.service":"app"}},"HostConfig":{"Memory":0,"MemoryReservation":0,"MemorySwap":0}}`, testID)
	case r.URL.Path == "/v1.44/containers/"+testID+"/update" && r.Method == http.MethodPost:
		var u MemoryUpdate
		if err := json.NewDecoder(r.Body).Decode(&u); err != nil {
			http.Error(w, `{"message":"bad body"}`, http.StatusBadRequest)
			return
		}
		f.updates = append(f.updates, u)
		fmt.Fprint(w, `{"Warnings":[]}`)
	case r.URL.Path == "/v1.44/events":
		if !strings.Contains(r.URL.Query().Get("filters"), `"oom"`) {
			http.Error(w, "missing filter", http.StatusBadRequest)
			return
		}
		fmt.Fprintf(w, `{"Action":"start","Actor":{"ID":%q},"timeNano":1793440800000000000}`+"\n", testID)
		fmt.Fprintf(w, `{"Action":"oom","Actor":{"ID":%q},"timeNano":1793440801000000000}`+"\n", testID)
	default:
		w.WriteHeader(http.StatusNotFound)
		fmt.Fprint(w, `{"message":"page not found"}`)
	}
}

func startEngine(t *testing.T, f *fakeEngine) *Client {
	t.Helper()
	sock := filepath.Join(t.TempDir(), "d.sock")
	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Skipf("unix sockets unavailable: %v", err)
	}
	srv := httptest.NewUnstartedServer(f)
	srv.Listener = ln
	srv.Start()
	t.Cleanup(srv.Close)
	return New(sock)
}

func TestClient(t *testing.T) {
	f := &fakeEngine{api: "1.52"}
	c := startEngine(t, f)
	ctx := context.Background()

	v, err := c.Version(ctx)
	if err != nil || v.Version != "29.9.2" {
		t.Fatalf("Version = %+v, %v", v, err)
	}
	list, err := c.List(ctx)
	if err != nil || len(list) != 1 || list[0].ID != testID || list[0].Labels["com.docker.compose.project"] != "shop" {
		t.Fatalf("List = %+v, %v", list, err)
	}
	ct, err := c.Inspect(ctx, testID)
	if err != nil {
		t.Fatal(err)
	}
	if ct.Name != "/shop-app-1" || ct.RestartCount != 2 || !ct.State.OOMKilled || ct.Config.Labels["com.docker.compose.service"] != "app" {
		t.Errorf("Inspect = %+v", ct)
	}
	if want := time.Date(2026, 10, 30, 8, 0, 0, 123e6, time.UTC); !ct.State.StartedAt.Equal(want) {
		t.Errorf("StartedAt = %v", ct.State.StartedAt)
	}
	upd := MemoryUpdate{Memory: 1792 << 20, MemoryReservation: 1280 << 20, MemorySwap: -1}
	if err := c.UpdateMemory(ctx, testID, upd); err != nil {
		t.Fatal(err)
	}
	if len(f.updates) != 1 || f.updates[0] != upd {
		t.Errorf("engine saw %+v", f.updates)
	}
	var events []Event
	if err := c.Events(ctx, func(e Event) { events = append(events, e) }); err != nil {
		t.Fatal(err)
	}
	if len(events) != 2 || events[1].Action != "oom" || events[1].ID != testID || events[1].Time.Unix() != 1793440801 {
		t.Errorf("events = %+v", events)
	}
}

func TestClientRefusals(t *testing.T) {
	f := &fakeEngine{api: "1.43"}
	c := startEngine(t, f)
	ctx := context.Background()
	if _, err := c.Version(ctx); !errors.Is(err, ErrUnsupported) {
		t.Errorf("API 1.43: err = %v, want ErrUnsupported", err)
	}
	for _, id := range []string{"shop-app-1", "../../images/json", testID[:12], strings.ToUpper(testID)} {
		if _, err := c.Inspect(ctx, id); err == nil {
			t.Errorf("Inspect(%q) accepted", id)
		}
		if err := c.UpdateMemory(ctx, id, MemoryUpdate{}); err == nil {
			t.Errorf("UpdateMemory(%q) accepted", id)
		}
	}
	missing := strings.Repeat("f", 64)
	if _, err := c.Inspect(ctx, missing); !errors.Is(err, ErrNotFound) || !strings.Contains(err.Error(), "page not found") {
		t.Errorf("missing container: %v, want ErrNotFound with the engine's message", err)
	}
	f.bigList = true
	if _, err := c.List(ctx); err == nil || !strings.Contains(err.Error(), "larger than") {
		t.Errorf("oversized list: %v", err)
	}
}

func TestVersionAtLeast(t *testing.T) {
	cases := []struct {
		have, want string
		ok         bool
	}{
		{"1.44", "1.44", true},
		{"1.52", "1.44", true},
		{"1.9", "1.44", false},
		{"2.0", "1.44", true},
		{"1.x", "1.44", false},
		{"", "1.44", false},
	}
	for _, c := range cases {
		if got := versionAtLeast(c.have, c.want); got != c.ok {
			t.Errorf("versionAtLeast(%q, %q) = %v", c.have, c.want, got)
		}
	}
}

// Docker 29 answers API 1.44 through a generic map, so NoLimit comes back
// above the largest int64; that, and any count past it, reads as NoLimit.
func TestHostConfigReadsRoundedCounts(t *testing.T) {
	for body, want := range map[string]HostConfig{
		`{"Memory":268435456,"MemoryReservation":134217728,"MemorySwap":-1}`:         {Memory: 268435456, MemoryReservation: 134217728, MemorySwap: -1},
		`{"Memory":9223372036854775807,"MemoryReservation":6291456,"MemorySwap":-1}`: {Memory: NoLimit, MemoryReservation: 6291456, MemorySwap: -1},
		`{"Memory":9223372036854776000,"MemoryReservation":6291456,"MemorySwap":-1}`: {Memory: NoLimit, MemoryReservation: 6291456, MemorySwap: -1},
		`{"Memory":9.223372036854776e+18}`:                                           {Memory: NoLimit},
		`{"Memory":9007199254740993}`:                                                {Memory: 9007199254740993},
		`{"Memory":0,"MemorySwap":null}`:                                             {},
		`{}`:                                                                         {},
	} {
		var c Container
		if err := json.Unmarshal([]byte(`{"Id":"x","HostConfig":`+body+`}`), &c); err != nil {
			t.Errorf("%s: %v", body, err)
			continue
		}
		if c.HostConfig != want {
			t.Errorf("%s reads as %+v, want %+v", body, c.HostConfig, want)
		}
	}
	var c Container
	if err := json.Unmarshal([]byte(`{"HostConfig":{"Memory":"lots"}}`), &c); err == nil {
		t.Error("a byte count that is not a number was read")
	}
}

func TestLimited(t *testing.T) {
	for bytes, want := range map[int64]bool{0: false, -1: false, 6 << 20: true, 1<<62 - 1: true, 1 << 62: false, NoLimit: false} {
		if got := Limited(bytes); got != want {
			t.Errorf("Limited(%d) = %v, want %v", bytes, got, want)
		}
	}
}
