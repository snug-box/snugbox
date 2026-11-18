// Package docker is a small client for the Docker Engine API. It can list,
// inspect and watch containers and change their memory limits, and it has no
// way to do anything else: no exec, no create, no pull. Blueprint section 5.4
// limits Snugbox's Docker access to exactly these calls.
package docker

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// APIVersion is the Engine API version every request asks for. Docker Engine
// 25 introduced it, and it is the oldest version Snugbox supports.
const APIVersion = "1.44"

// DefaultSocket is where Docker Engine listens on Linux.
const DefaultSocket = "/var/run/docker.sock"

const (
	requestTimeout = 5 * time.Second
	maxListBytes   = 8 << 20
	maxObjectBytes = 1 << 20
)

// ErrUnsupported means the engine is older than APIVersion.
var ErrUnsupported = errors.New("docker engine is older than 25.0 (API " + APIVersion + ")")

var idPattern = regexp.MustCompile(`^[0-9a-f]{64}$`)

// ValidID reports whether id is a full 64-character container ID. Only full
// IDs reach a request path, so a name can never be read as part of the URL.
func ValidID(id string) bool { return idPattern.MatchString(id) }

// Client talks to one engine over its Unix socket.
type Client struct {
	hc   *http.Client
	base string
}

// New returns a client for the engine listening on socket.
func New(socket string) *Client {
	dialer := &net.Dialer{Timeout: 2 * time.Second}
	tr := &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			return dialer.DialContext(ctx, "unix", socket)
		},
		MaxIdleConns:    2,
		IdleConnTimeout: 30 * time.Second,
	}
	return &Client{hc: &http.Client{Transport: tr, CheckRedirect: noRedirects}, base: "http://docker/v" + APIVersion}
}

func noRedirects(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }

// Version is the engine's answer to GET /version.
type Version struct {
	Version       string
	APIVersion    string `json:"ApiVersion"`
	MinAPIVersion string `json:"MinAPIVersion"`
}

// Version asks the engine which API versions it speaks, and returns
// ErrUnsupported when APIVersion is not among them.
func (c *Client) Version(ctx context.Context) (Version, error) {
	var v Version
	if err := c.get(ctx, "http://docker/version", maxObjectBytes, &v); err != nil {
		return Version{}, err
	}
	if !versionAtLeast(v.APIVersion, APIVersion) || (v.MinAPIVersion != "" && !versionAtLeast(APIVersion, v.MinAPIVersion)) {
		return v, fmt.Errorf("%w: engine %s speaks API %s to %s", ErrUnsupported, v.Version, v.MinAPIVersion, v.APIVersion)
	}
	return v, nil
}

// Summary is one entry of the container list.
type Summary struct {
	ID     string `json:"Id"`
	Names  []string
	Image  string
	State  string
	Labels map[string]string
}

// List returns every container, running or not.
func (c *Client) List(ctx context.Context) ([]Summary, error) {
	var out []Summary
	err := c.get(ctx, c.base+"/containers/json?all=1", maxListBytes, &out)
	return out, err
}

// Container is the part of GET /containers/{id}/json that Snugbox reads.
type Container struct {
	ID           string `json:"Id"`
	Name         string
	RestartCount int
	State        struct {
		Status    string
		Running   bool
		OOMKilled bool
		StartedAt time.Time
	}
	Config struct {
		Image  string
		Labels map[string]string
	}
	HostConfig HostConfig
}

// NoLimit is the memory limit Snugbox gives a container to take its limit
// away, since Docker can't remove one: the kernel reads it as no limit.
const NoLimit = math.MaxInt64

// Limited reports whether a memory limit Docker reports is a real one. Zero
// is none, and so is anything from 4 EiB up, which is how NoLimit and other
// raised limits come back.
func Limited(bytes int64) bool { return bytes > 0 && bytes < 1<<62 }

// HostConfig is the part of a container's host configuration Snugbox reads,
// in bytes.
type HostConfig struct {
	Memory            int64
	MemoryReservation int64
	MemorySwap        int64
}

// UnmarshalJSON reads the memory fields as numbers that may not fit an
// int64. Docker 29 answers a request for an older API version, such as
// the 1.44 Snugbox asks for, through a generic map, so a count above 2^53
// comes back rounded as a float64. NoLimit, the largest int64, comes back
// as 9223372036854776000, which is above it. Such a count reads as
// NoLimit. Checked with Docker 29.1.3 and API 1.52 on 1 November 2026.
func (h *HostConfig) UnmarshalJSON(data []byte) error {
	var raw struct{ Memory, MemoryReservation, MemorySwap json.Number }
	if err := json.Unmarshal(data, &raw); err != nil {
		return err
	}
	for _, f := range []struct {
		in  json.Number
		out *int64
	}{{raw.Memory, &h.Memory}, {raw.MemoryReservation, &h.MemoryReservation}, {raw.MemorySwap, &h.MemorySwap}} {
		v, err := byteCount(f.in)
		if err != nil {
			return err
		}
		*f.out = v
	}
	return nil
}

func byteCount(n json.Number) (int64, error) {
	if n == "" {
		return 0, nil
	}
	if i, err := n.Int64(); err == nil {
		return i, nil
	}
	f, err := n.Float64()
	switch {
	case err != nil:
		return 0, fmt.Errorf("byte count %q: %w", n, err)
	case f >= math.MaxInt64:
		return NoLimit, nil
	case f <= math.MinInt64:
		return math.MinInt64, nil
	}
	return int64(f), nil
}

// Inspect returns the details of one container.
func (c *Client) Inspect(ctx context.Context, id string) (Container, error) {
	if !ValidID(id) {
		return Container{}, fmt.Errorf("inspect: %q is not a full container ID", id)
	}
	var out Container
	err := c.get(ctx, c.base+"/containers/"+id+"/json", maxObjectBytes, &out)
	return out, err
}

// MemoryUpdate holds the memory fields of POST /containers/{id}/update, in
// bytes. Docker reads 0 as "leave as it is" and -1 in MemorySwap as unlimited
// swap.
type MemoryUpdate struct {
	Memory            int64 `json:"Memory"`
	MemoryReservation int64 `json:"MemoryReservation"`
	MemorySwap        int64 `json:"MemorySwap"`
}

// UpdateMemory changes a running container's memory limits, as docker update
// does.
func (c *Client) UpdateMemory(ctx context.Context, id string, u MemoryUpdate) error {
	if !ValidID(id) {
		return fmt.Errorf("update: %q is not a full container ID", id)
	}
	body, err := json.Marshal(u)
	if err != nil {
		return fmt.Errorf("update: %w", err)
	}
	ctx, cancel := context.WithTimeout(ctx, requestTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.base+"/containers/"+id+"/update", bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("update: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.hc.Do(req)
	if err != nil {
		return fmt.Errorf("update %s: %w", id[:12], err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return apiError("update "+id[:12], resp)
	}
	return nil
}

// Event is one container event from GET /events.
type Event struct {
	Action string
	ID     string
	Time   time.Time
}

// Events streams container start, die, oom, update and destroy events to fn
// until ctx ends or the engine closes the stream.
func (c *Client) Events(ctx context.Context, fn func(Event)) error {
	filters := `{"type":["container"],"event":["start","die","oom","update","destroy"]}`
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.base+"/events?"+url.Values{"filters": {filters}}.Encode(), nil)
	if err != nil {
		return fmt.Errorf("events: %w", err)
	}
	resp, err := c.hc.Do(req)
	if err != nil {
		return fmt.Errorf("events: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return apiError("events", resp)
	}
	dec := json.NewDecoder(io.LimitReader(resp.Body, 1<<40))
	for {
		var raw struct {
			Action   string
			Actor    struct{ ID string }
			TimeNano int64 `json:"timeNano"`
		}
		if err := dec.Decode(&raw); err != nil {
			if errors.Is(err, io.EOF) || ctx.Err() != nil {
				return ctx.Err()
			}
			return fmt.Errorf("events: %w", err)
		}
		fn(Event{Action: raw.Action, ID: raw.Actor.ID, Time: time.Unix(0, raw.TimeNano).UTC()})
	}
}

func (c *Client) get(ctx context.Context, rawURL string, limit int64, out any) error {
	ctx, cancel := context.WithTimeout(ctx, requestTimeout)
	defer cancel()
	op := "docker " + strings.TrimPrefix(rawURL, c.base)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return fmt.Errorf("%s: %w", op, err)
	}
	resp, err := c.hc.Do(req)
	if err != nil {
		return fmt.Errorf("%s: %w", op, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return apiError(op, resp)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, limit+1))
	if err != nil {
		return fmt.Errorf("%s: %w", op, err)
	}
	if int64(len(body)) > limit {
		return fmt.Errorf("%s: answer larger than %d bytes", op, limit)
	}
	if err := json.Unmarshal(body, out); err != nil {
		return fmt.Errorf("%s: %w", op, err)
	}
	return nil
}

// ErrNotFound means the engine has no container with the ID asked for.
var ErrNotFound = errors.New("no such container")

func apiError(op string, resp *http.Response) error {
	b, err := io.ReadAll(io.LimitReader(resp.Body, 4096))
	if err != nil {
		return fmt.Errorf("%s: engine answered %d", op, resp.StatusCode)
	}
	var msg struct{ Message string }
	if json.Unmarshal(b, &msg) != nil || msg.Message == "" {
		msg.Message = strings.TrimSpace(string(b))
	}
	if resp.StatusCode == http.StatusNotFound {
		return fmt.Errorf("%s: %w: %s", op, ErrNotFound, msg.Message)
	}
	return fmt.Errorf("%s: engine answered %d: %s", op, resp.StatusCode, msg.Message)
}

// versionAtLeast compares dotted API versions such as "1.44" numerically.
func versionAtLeast(have, want string) bool {
	hm, hn, ok1 := splitVersion(have)
	wm, wn, ok2 := splitVersion(want)
	if !ok1 || !ok2 {
		return false
	}
	return hm > wm || (hm == wm && hn >= wn)
}

func splitVersion(v string) (int, int, bool) {
	a, b, ok := strings.Cut(v, ".")
	if !ok {
		return 0, 0, false
	}
	major, err1 := strconv.Atoi(a)
	minor, err2 := strconv.Atoi(b)
	return major, minor, err1 == nil && err2 == nil
}
