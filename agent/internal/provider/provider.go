// Package provider finds out which hosting provider and region a box runs in,
// from the DMI strings the firmware reports and the provider's metadata
// service.
//
// Metadata requests follow blueprint section 5.5: a fixed list of paths per
// provider, plain HTTP to 169.254.169.254 only, no proxy, no redirects, a
// two-second limit and 64 KiB at most. Only the provider, region and instance
// type are kept. Paths that can hold secrets, such as user data or AWS
// credentials, are never requested.
package provider

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"
)

// Provider IDs, matching the plan catalogue.
const (
	Hetzner      = "hetzner"
	DigitalOcean = "digitalocean"
	AWS          = "aws"
	Vultr        = "vultr"
	Linode       = "linode"
)

// MetadataAddr is the link-local address every supported provider serves its
// metadata on.
const MetadataAddr = "169.254.169.254"

const (
	requestTimeout = 2 * time.Second
	maxBody        = 64 << 10
)

// Info is what Snugbox learned about the box's provider.
type Info struct {
	Provider string
	Region   string
	// InstanceType is the AWS instance type; other providers' metadata does
	// not name the plan.
	InstanceType string
	// FromMetadata is true when the metadata service answered, and false when
	// the provider was only guessed from DMI strings.
	FromMetadata bool
}

// Known reports whether the provider was identified.
func (i Info) Known() bool { return i.Provider != "" }

// Prober asks the metadata services.
type Prober struct {
	// BaseURL is "http://169.254.169.254" unless a test points it elsewhere.
	BaseURL string
	Client  *http.Client
	// FS is the filesystem rooted at "/" holding sys/class/dmi/id.
	FS fs.FS
}

// NewProber returns a Prober for the live box.
func NewProber(fsys fs.FS) *Prober {
	dialer := &net.Dialer{Timeout: requestTimeout}
	tr := &http.Transport{
		Proxy: nil,
		DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
			if addr != MetadataAddr+":80" {
				return nil, fmt.Errorf("metadata: refusing to dial %s", addr)
			}
			return dialer.DialContext(ctx, network, addr)
		},
		DisableKeepAlives:     true,
		ResponseHeaderTimeout: requestTimeout,
	}
	return &Prober{
		BaseURL: "http://" + MetadataAddr,
		Client:  &http.Client{Transport: tr, Timeout: requestTimeout, CheckRedirect: refuseRedirect},
		FS:      fsys,
	}
}

func refuseRedirect(*http.Request, []*http.Request) error {
	return errors.New("metadata: redirects are refused")
}

// Detect identifies the provider. It reads the DMI strings first, then asks
// the metadata services of the providers that answer at the link-local
// address, all at once, and keeps the first that answers.
func (p *Prober) Detect(ctx context.Context) Info {
	guess := p.fromDMI()
	ctx, cancel := context.WithTimeout(ctx, requestTimeout)
	defer cancel()

	probes := map[string]func(context.Context) (Info, error){
		Hetzner:      p.hetzner,
		DigitalOcean: p.digitalOcean,
		AWS:          p.aws,
	}
	if probe, ok := probes[guess.Provider]; ok {
		if info, err := probe(ctx); err == nil {
			return info
		}
		return guess
	}
	if guess.Known() {
		return guess
	}
	var (
		mu    sync.Mutex
		found Info
		wg    sync.WaitGroup
	)
	for _, probe := range probes {
		wg.Go(func() {
			info, err := probe(ctx)
			if err != nil {
				return
			}
			mu.Lock()
			defer mu.Unlock()
			if !found.Known() {
				found = info
			}
		})
	}
	wg.Wait()
	return found
}

func (p *Prober) fromDMI() Info {
	read := func(name string) string {
		b, err := fs.ReadFile(p.FS, "sys/class/dmi/id/"+name)
		if err != nil {
			return ""
		}
		return strings.ToLower(strings.TrimSpace(string(b)))
	}
	vendor := read("sys_vendor")
	switch {
	case strings.Contains(vendor, "hetzner"):
		return Info{Provider: Hetzner}
	case strings.Contains(vendor, "digitalocean"):
		return Info{Provider: DigitalOcean}
	case strings.Contains(vendor, "amazon"):
		return Info{Provider: AWS}
	case strings.Contains(vendor, "vultr"):
		return Info{Provider: Vultr}
	case strings.Contains(vendor, "linode"), strings.Contains(vendor, "akamai"):
		return Info{Provider: Linode}
	}
	return Info{}
}

func (p *Prober) hetzner(ctx context.Context) (Info, error) {
	if _, err := p.get(ctx, "/hetzner/v1/metadata/instance-id", nil); err != nil {
		return Info{}, err
	}
	zone, err := p.get(ctx, "/hetzner/v1/metadata/availability-zone", nil)
	if err != nil {
		return Info{}, err
	}
	location, _, _ := strings.Cut(zone, "-")
	return Info{Provider: Hetzner, Region: location, FromMetadata: true}, nil
}

func (p *Prober) digitalOcean(ctx context.Context) (Info, error) {
	if _, err := p.get(ctx, "/metadata/v1/id", nil); err != nil {
		return Info{}, err
	}
	region, err := p.get(ctx, "/metadata/v1/region", nil)
	if err != nil {
		return Info{}, err
	}
	return Info{Provider: DigitalOcean, Region: region, FromMetadata: true}, nil
}

// aws uses an IMDSv2 session token, which also works where IMDSv1 is
// switched off.
func (p *Prober) aws(ctx context.Context) (Info, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPut, p.BaseURL+"/latest/api/token", nil)
	if err != nil {
		return Info{}, err
	}
	req.Header.Set("X-aws-ec2-metadata-token-ttl-seconds", "60")
	token, err := p.do(req)
	if err != nil {
		return Info{}, err
	}
	hdr := http.Header{"X-Aws-Ec2-Metadata-Token": {token}}
	typ, err := p.get(ctx, "/latest/meta-data/instance-type", hdr)
	if err != nil {
		return Info{}, err
	}
	region, err := p.get(ctx, "/latest/meta-data/placement/region", hdr)
	if err != nil {
		return Info{}, err
	}
	return Info{Provider: AWS, Region: region, InstanceType: typ, FromMetadata: true}, nil
}

func (p *Prober) get(ctx context.Context, path string, hdr http.Header) (string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, p.BaseURL+path, nil)
	if err != nil {
		return "", err
	}
	for k, v := range hdr {
		req.Header[k] = v
	}
	return p.do(req)
}

func (p *Prober) do(req *http.Request) (string, error) {
	resp, err := p.Client.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxBody+1))
	if err != nil {
		return "", err
	}
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("metadata %s: status %d", req.URL.Path, resp.StatusCode)
	}
	if len(body) > maxBody {
		return "", fmt.Errorf("metadata %s: answer over %d bytes", req.URL.Path, maxBody)
	}
	v := strings.TrimSpace(string(body))
	if v == "" || strings.ContainsAny(v, "\n\r") {
		return "", fmt.Errorf("metadata %s: unexpected answer", req.URL.Path)
	}
	return v, nil
}
