// Package locate finds where a node really exits: from a probe through the
// node (Cloudflare's /cdn-cgi/trace) when there is one, else from the node's
// name.
package locate

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/oboo/moho/internal/geo"
	"github.com/oboo/moho/internal/mihomo"
)

// ProbeGroup is the selector the probe listener is bound to (core.ProbeGroup;
// repeated here so locate does not depend on core).
const ProbeGroup = "MOHO-PROBE"

// Direct is the node that measures the user's own location.
const Direct = "DIRECT"

// Sources of a Result.
const (
	SourceProbe   = "probe"
	SourceName    = "name"
	SourceUnknown = "unknown"
)

// TraceURL is the page that says where a request came from.
const TraceURL = "https://www.cloudflare.com/cdn-cgi/trace"

const (
	probeTimeout = 8 * time.Second
	// maxAge is how long a probe is trusted by Guess.
	maxAge = 24 * time.Hour
)

// Result is where a node exits, and how Moho knows.
type Result struct {
	Place  *geo.Place `json:"place,omitempty"`
	Source string     `json:"source"` // "probe", "name", "unknown"
	IP     string     `json:"ip,omitempty"`
	At     time.Time  `json:"at"`
}

// Locator caches probes by node and keeps them in a JSON file.
type Locator struct {
	path  string
	now   func() time.Time
	slot  chan struct{} // one probe at a time
	mu    sync.Mutex
	cache map[string]Result
	// named is geo.FromName by display name: a snapshot asks for every
	// node's, every 2 s, and names don't move.
	named map[string]namedPlace
}

type namedPlace struct {
	place geo.Place
	ok    bool
}

// New returns a locator whose probes are kept in cachePath (none if empty),
// loading the ones already there.
func New(cachePath string) *Locator {
	l := &Locator{path: cachePath, now: time.Now, slot: make(chan struct{}, 1), cache: map[string]Result{}}
	if cachePath != "" {
		if b, err := os.ReadFile(cachePath); err == nil {
			_ = json.Unmarshal(b, &l.cache)
		}
		if l.cache == nil {
			l.cache = map[string]Result{}
		}
	}
	return l
}

// Guess answers without the network: a cached probe (at most 24 h old), else
// geo.FromName(display), else an older probe, else unknown. An old probe is
// still better than nothing for a node whose name says nothing.
//
// Fresh says whether a probe of node is recent enough for Guess to use first.
func (l *Locator) Guess(node, display string) Result {
	now := l.now()
	l.mu.Lock()
	r, ok := l.cache[node]
	l.mu.Unlock()
	if ok && r.Place != nil && now.Sub(r.At) <= maxAge {
		return r
	}
	l.mu.Lock()
	n, seen := l.named[display]
	l.mu.Unlock()
	if !seen {
		n.place, n.ok = geo.FromName(display)
		l.mu.Lock()
		if l.named == nil || len(l.named) >= 4096 { // names that come and go
			l.named = map[string]namedPlace{}
		}
		l.named[display] = n
		l.mu.Unlock()
	}
	if n.ok {
		p := n.place
		return Result{Place: &p, Source: SourceName, At: now}
	}
	if ok && r.Place != nil {
		return r
	}
	return Result{Source: SourceUnknown, At: now}
}

func (l *Locator) Fresh(node string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	r, ok := l.cache[node]
	return ok && r.Place != nil && l.now().Sub(r.At) <= maxAge
}

// Probe measures where node exits; node "DIRECT" measures the user's own
// location. It points the selector group MOHO-PROBE at node, then requests
// Cloudflare's trace through the probe listener 127.0.0.1:probePort and maps
// its loc and colo with geo.FromTrace. Probes run one at a time; results are
// cached by node and written to the cache file.
func (l *Locator) Probe(ctx context.Context, c *mihomo.Client, probePort int, node string) (Result, error) {
	select {
	case l.slot <- struct{}{}:
	case <-ctx.Done():
		return Result{Source: SourceUnknown, At: l.now()}, ctx.Err()
	}
	defer func() { <-l.slot }()

	if err := c.Select(ctx, ProbeGroup, node); err != nil {
		return Result{Source: SourceUnknown, At: l.now()}, fmt.Errorf("select %s: %w", node, err)
	}
	body, err := fetchTrace(ctx, probePort)
	if err != nil {
		return Result{Source: SourceUnknown, At: l.now()}, err
	}
	return l.record(node, body)
}

// record keeps what a trace answer says about node's exit.
func (l *Locator) record(node, body string) (Result, error) {
	ip, loc, colo := parseTrace(body)
	if ip == "" && loc == "" {
		return Result{Source: SourceUnknown, At: l.now()}, errors.New("trace: no ip or loc in the answer")
	}
	r := Result{Source: SourceProbe, IP: ip, At: l.now()}
	if p, ok := geo.FromTrace(loc, colo); ok {
		r.Place = &p
	}
	l.mu.Lock()
	l.cache[node] = r
	err := l.saveLocked()
	l.mu.Unlock()
	return r, err
}

// fetchTrace requests the trace through the probe listener, on a connection
// of its own: a kept-alive one would still go through the previous node.
func fetchTrace(ctx context.Context, probePort int) (string, error) {
	proxy := &url.URL{Scheme: "http", Host: "127.0.0.1:" + strconv.Itoa(probePort)}
	client := &http.Client{
		Timeout:   probeTimeout,
		Transport: &http.Transport{Proxy: http.ProxyURL(proxy), DisableKeepAlives: true},
	}
	return fetch(ctx, client, TraceURL)
}

// parseTrace reads the key=value lines of a /cdn-cgi/trace answer.
func parseTrace(body string) (ip, loc, colo string) {
	for _, line := range strings.Split(body, "\n") {
		k, v, ok := strings.Cut(strings.TrimSpace(line), "=")
		if !ok {
			continue
		}
		switch k {
		case "ip":
			ip = v
		case "loc":
			loc = v
		case "colo":
			colo = v
		}
	}
	return ip, loc, colo
}

// saveLocked writes the cache file atomically; l.mu is held.
func (l *Locator) saveLocked() error {
	if l.path == "" {
		return nil
	}
	b, err := json.MarshalIndent(l.cache, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(l.path), 0o755); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(l.path), ".locate-*")
	if err != nil {
		return err
	}
	_, err = tmp.Write(b)
	if cerr := tmp.Close(); err == nil {
		err = cerr
	}
	if err == nil {
		err = os.Rename(tmp.Name(), l.path)
	}
	if err != nil {
		_ = os.Remove(tmp.Name())
	}
	return err
}
