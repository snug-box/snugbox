package provider

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"testing/fstest"
)

type metadataServer struct {
	mu    sync.Mutex
	paths []string
	kind  string
}

func (m *metadataServer) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	m.mu.Lock()
	m.paths = append(m.paths, r.Method+" "+r.URL.Path)
	m.mu.Unlock()
	switch {
	case m.kind == Hetzner && r.URL.Path == "/hetzner/v1/metadata/instance-id":
		w.Write([]byte("4242\n"))
	case m.kind == Hetzner && r.URL.Path == "/hetzner/v1/metadata/availability-zone":
		w.Write([]byte("fsn1-dc14"))
	case m.kind == DigitalOcean && r.URL.Path == "/metadata/v1/id":
		w.Write([]byte("2756294"))
	case m.kind == DigitalOcean && r.URL.Path == "/metadata/v1/region":
		w.Write([]byte("fra1"))
	case m.kind == AWS && r.Method == http.MethodPut && r.URL.Path == "/latest/api/token":
		if r.Header.Get("X-aws-ec2-metadata-token-ttl-seconds") == "" {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		w.Write([]byte("tok123"))
	case m.kind == AWS && r.Header.Get("X-aws-ec2-metadata-token") != "tok123":
		w.WriteHeader(http.StatusUnauthorized)
	case m.kind == AWS && r.URL.Path == "/latest/meta-data/instance-type":
		w.Write([]byte("t3.medium"))
	case m.kind == AWS && r.URL.Path == "/latest/meta-data/placement/region":
		w.Write([]byte("eu-central-1"))
	case m.kind == "redirect":
		http.Redirect(w, r, "http://example.org/", http.StatusFound)
	case m.kind == "huge":
		w.Write([]byte(strings.Repeat("a", maxBody+10)))
	case m.kind == "multiline":
		w.Write([]byte("a\nb"))
	default:
		w.WriteHeader(http.StatusNotFound)
	}
}

func prober(t *testing.T, kind, vendor string) (*Prober, *metadataServer) {
	t.Helper()
	m := &metadataServer{kind: kind}
	srv := httptest.NewServer(m)
	t.Cleanup(srv.Close)
	fsys := fstest.MapFS{}
	if vendor != "" {
		fsys["sys/class/dmi/id/sys_vendor"] = &fstest.MapFile{Data: []byte(vendor + "\n")}
	}
	c := srv.Client()
	c.CheckRedirect = refuseRedirect
	return &Prober{BaseURL: srv.URL, Client: c, FS: fsys}, m
}

func TestDetect(t *testing.T) {
	cases := []struct {
		kind, vendor string
		want         Info
	}{
		{Hetzner, "Hetzner", Info{Provider: Hetzner, Region: "fsn1", FromMetadata: true}},
		{DigitalOcean, "DigitalOcean", Info{Provider: DigitalOcean, Region: "fra1", FromMetadata: true}},
		{AWS, "Amazon EC2", Info{Provider: AWS, Region: "eu-central-1", InstanceType: "t3.medium", FromMetadata: true}},
		{AWS, "", Info{Provider: AWS, Region: "eu-central-1", InstanceType: "t3.medium", FromMetadata: true}},
		{DigitalOcean, "", Info{Provider: DigitalOcean, Region: "fra1", FromMetadata: true}},
		{"none", "Vultr", Info{Provider: Vultr}},
		{"none", "Linode", Info{Provider: Linode}},
		{"none", "Hetzner", Info{Provider: Hetzner}},
		{"none", "", Info{}},
	}
	for _, c := range cases {
		p, _ := prober(t, c.kind, c.vendor)
		if got := p.Detect(context.Background()); got != c.want {
			t.Errorf("metadata %s, vendor %q: got %+v, want %+v", c.kind, c.vendor, got, c.want)
		}
	}
}

func TestDetectAsksOnlyFixedPaths(t *testing.T) {
	allowed := map[string]bool{
		"GET /hetzner/v1/metadata/instance-id":       true,
		"GET /hetzner/v1/metadata/availability-zone": true,
		"GET /metadata/v1/id":                        true,
		"GET /metadata/v1/region":                    true,
		"PUT /latest/api/token":                      true,
		"GET /latest/meta-data/instance-type":        true,
		"GET /latest/meta-data/placement/region":     true,
	}
	for _, kind := range []string{Hetzner, DigitalOcean, AWS, "none"} {
		p, m := prober(t, kind, "")
		p.Detect(context.Background())
		for _, path := range m.paths {
			if !allowed[path] {
				t.Errorf("%s: requested %s", kind, path)
			}
		}
	}
}

func TestRefusals(t *testing.T) {
	for _, kind := range []string{"redirect", "huge", "multiline"} {
		p, _ := prober(t, kind, "")
		if _, err := p.get(context.Background(), "/metadata/v1/id", nil); err == nil {
			t.Errorf("%s answer accepted", kind)
		}
	}
	live := NewProber(fstest.MapFS{})
	if live.Client.Transport.(*http.Transport).Proxy != nil {
		t.Error("the live prober would honour HTTP_PROXY")
	}
	if _, err := live.Client.Transport.(*http.Transport).DialContext(context.Background(), "tcp", "10.0.0.1:80"); err == nil {
		t.Error("the live prober dialled an address other than the metadata service")
	}
}
