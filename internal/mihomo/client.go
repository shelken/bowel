// Package mihomo is a small client of mihomo's external controller (the
// RESTful API that Clash dashboards use).
package mihomo

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// Client talks to one mihomo instance, over a unix socket or TCP.
type Client struct {
	base   string
	secret string
	http   *http.Client
	stream *http.Client // without a timeout, for streaming endpoints
}

// NewUnix returns a client of a controller listening on a unix socket
// (external-controller-unix), which needs no secret.
func NewUnix(socket string) *Client {
	dial := func(ctx context.Context, _, _ string) (net.Conn, error) {
		var d net.Dialer
		return d.DialContext(ctx, "unix", socket)
	}
	return newClient("http://mihomo", "", &http.Transport{DialContext: dial, DisableCompression: true})
}

// NewTCP returns a client of a controller on addr, e.g. "127.0.0.1:9097".
func NewTCP(addr, secret string) *Client {
	return newClient("http://"+addr, secret, &http.Transport{Proxy: nil, DisableCompression: true})
}

func newClient(base, secret string, t *http.Transport) *Client {
	return &Client{
		base:   base,
		secret: secret,
		http:   &http.Client{Transport: t, Timeout: 15 * time.Second},
		stream: &http.Client{Transport: t},
	}
}

func (c *Client) do(ctx context.Context, method, path string, body, out any) error {
	var r io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return err
		}
		r = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.base+path, r)
	if err != nil {
		return err
	}
	if c.secret != "" {
		req.Header.Set("Authorization", "Bearer "+c.secret)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		var e struct {
			Message string `json:"message"`
		}
		b, _ := io.ReadAll(resp.Body)
		if json.Unmarshal(b, &e) == nil && e.Message != "" {
			return fmt.Errorf("mihomo: %s", e.Message)
		}
		return fmt.Errorf("mihomo: %s %s: %s", method, path, resp.Status)
	}
	if out == nil {
		_, _ = io.Copy(io.Discard, resp.Body)
		return nil
	}
	return json.NewDecoder(resp.Body).Decode(out)
}

// Version is the answer of GET /version.
type Version struct {
	Meta    bool   `json:"meta"`
	Version string `json:"version"`
}

func (c *Client) Version(ctx context.Context) (Version, error) {
	var v Version
	err := c.do(ctx, http.MethodGet, "/version", nil, &v)
	return v, err
}

// Config is the part of GET /configs the client uses.
type Config struct {
	Mode      string `json:"mode"`
	MixedPort int    `json:"mixed-port"`
	AllowLan  bool   `json:"allow-lan"`
	LogLevel  string `json:"log-level"`
}

func (c *Client) Config(ctx context.Context) (Config, error) {
	var v Config
	err := c.do(ctx, http.MethodGet, "/configs", nil, &v)
	return v, err
}

// Patch changes settings of the running config, e.g. {"mode": "global"}.
func (c *Client) Patch(ctx context.Context, fields map[string]any) error {
	return c.do(ctx, http.MethodPatch, "/configs", fields, nil)
}

// Reload makes mihomo load the config file at path again.
func (c *Client) Reload(ctx context.Context, path string) error {
	return c.do(ctx, http.MethodPut, "/configs?force=true", map[string]string{"path": path}, nil)
}

// Rule is one entry of GET /rules. Proxy is its target: a group's name, or
// in sing-box "route(<group>)" among its other actions.
type Rule struct {
	Type    string `json:"type"`
	Payload string `json:"payload"`
	Proxy   string `json:"proxy"`
}

// Rules is the core's rules, in the order they apply.
func (c *Client) Rules(ctx context.Context) ([]Rule, error) {
	var v struct {
		Rules []Rule `json:"rules"`
	}
	err := c.do(ctx, http.MethodGet, "/rules", nil, &v)
	return v.Rules, err
}

// Proxy is one entry of GET /proxies: a node or a group.
type Proxy struct {
	Name    string    `json:"name"`
	Type    string    `json:"type"`
	Now     string    `json:"now,omitempty"`
	All     []string  `json:"all,omitempty"`
	UDP     bool      `json:"udp"`
	Alive   bool      `json:"alive"`
	Hidden  bool      `json:"hidden,omitempty"`
	Icon    string    `json:"icon,omitempty"`
	History []History `json:"history"`
}

type History struct {
	Time  time.Time `json:"time"`
	Delay int       `json:"delay"`
}

func (c *Client) Proxies(ctx context.Context) (map[string]Proxy, error) {
	var v struct {
		Proxies map[string]Proxy `json:"proxies"`
	}
	err := c.do(ctx, http.MethodGet, "/proxies", nil, &v)
	return v.Proxies, err
}

// Select makes the selector group use the proxy name.
func (c *Client) Select(ctx context.Context, group, name string) error {
	return c.do(ctx, http.MethodPut, "/proxies/"+url.PathEscape(group), map[string]string{"name": name}, nil)
}

// Delay tests one proxy and returns its delay in milliseconds.
func (c *Client) Delay(ctx context.Context, name, testURL string, timeout time.Duration) (int, error) {
	q := url.Values{"url": {testURL}, "timeout": {fmt.Sprint(timeout.Milliseconds())}}
	var v struct {
		Delay int `json:"delay"`
	}
	err := c.do(ctx, http.MethodGet, "/proxies/"+url.PathEscape(name)+"/delay?"+q.Encode(), nil, &v)
	return v.Delay, err
}

// GroupDelay tests every proxy of a group; proxies that failed are absent.
func (c *Client) GroupDelay(ctx context.Context, group, testURL string, timeout time.Duration) (map[string]int, error) {
	q := url.Values{"url": {testURL}, "timeout": {fmt.Sprint(timeout.Milliseconds())}}
	v := map[string]int{}
	err := c.do(ctx, http.MethodGet, "/group/"+url.PathEscape(group)+"/delay?"+q.Encode(), nil, &v)
	return v, err
}

// Snapshot is the answer of GET /connections.
type Snapshot struct {
	DownloadTotal int64        `json:"downloadTotal"`
	UploadTotal   int64        `json:"uploadTotal"`
	Memory        int64        `json:"memory"`
	Connections   []Connection `json:"connections"`
}

type Connection struct {
	ID          string    `json:"id"`
	Upload      int64     `json:"upload"`
	Download    int64     `json:"download"`
	Start       time.Time `json:"start"`
	Chains      []string  `json:"chains"`
	Rule        string    `json:"rule"`
	RulePayload string    `json:"rulePayload"`
	Metadata    struct {
		Network         string `json:"network"`
		Type            string `json:"type"`
		Host            string `json:"host"`
		SniffHost       string `json:"sniffHost"`
		SourceIP        string `json:"sourceIP"`
		SourcePort      string `json:"sourcePort"`
		DestinationIP   string `json:"destinationIP"`
		DestinationPort string `json:"destinationPort"`
		Process         string `json:"process"`
		ProcessPath     string `json:"processPath"`
		InboundName     string `json:"inboundName"`
	} `json:"metadata"`
}

func (c *Client) Connections(ctx context.Context) (Snapshot, error) {
	var v Snapshot
	err := c.do(ctx, http.MethodGet, "/connections", nil, &v)
	return v, err
}

func (c *Client) CloseConnection(ctx context.Context, id string) error {
	return c.do(ctx, http.MethodDelete, "/connections/"+url.PathEscape(id), nil, nil)
}

func (c *Client) CloseAllConnections(ctx context.Context) error {
	return c.do(ctx, http.MethodDelete, "/connections", nil, nil)
}

// Traffic is one sample of GET /traffic: bytes per second.
type Traffic struct {
	Up   int64 `json:"up"`
	Down int64 `json:"down"`
}

// StreamTraffic calls fn with a sample every second until ctx ends or the
// stream breaks.
func (c *Client) StreamTraffic(ctx context.Context, fn func(Traffic)) error {
	return streamJSON(ctx, c, "/traffic", fn)
}

// Log is one line of GET /logs.
type Log struct {
	Type    string `json:"type"`
	Payload string `json:"payload"`
}

func (c *Client) StreamLogs(ctx context.Context, level string, fn func(Log)) error {
	return streamJSON(ctx, c, "/logs?level="+url.QueryEscape(level), fn)
}

func streamJSON[T any](ctx context.Context, c *Client, path string, fn func(T)) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.base+path, nil)
	if err != nil {
		return err
	}
	if c.secret != "" {
		req.Header.Set("Authorization", "Bearer "+c.secret)
	}
	resp, err := c.stream.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("mihomo: GET %s: %s", path, resp.Status)
	}
	sc := bufio.NewScanner(resp.Body)
	sc.Buffer(make([]byte, 64<<10), 1<<20)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" {
			continue
		}
		var v T
		if json.Unmarshal([]byte(line), &v) == nil {
			fn(v)
		}
	}
	if err := sc.Err(); err != nil && ctx.Err() == nil {
		return err
	}
	return ctx.Err()
}

// ProxyProvider is one entry of GET /providers/proxies.
type ProxyProvider struct {
	Name        string    `json:"name"`
	Type        string    `json:"type"`
	VehicleType string    `json:"vehicleType"`
	Proxies     []Proxy   `json:"proxies"`
	UpdatedAt   time.Time `json:"updatedAt"`
}

func (c *Client) ProxyProviders(ctx context.Context) (map[string]ProxyProvider, error) {
	var v struct {
		Providers map[string]ProxyProvider `json:"providers"`
	}
	err := c.do(ctx, http.MethodGet, "/providers/proxies", nil, &v)
	return v.Providers, err
}

// HealthCheck tests every node of a provider.
func (c *Client) HealthCheck(ctx context.Context, provider string) error {
	return c.do(ctx, http.MethodGet, "/providers/proxies/"+url.PathEscape(provider)+"/healthcheck", nil, nil)
}

// UpdateGeo makes the core download the geo databases again, from the
// config's geox-url. It answers before the download ends.
func (c *Client) UpdateGeo(ctx context.Context) error {
	return c.do(ctx, http.MethodPost, "/configs/geo", map[string]any{}, nil)
}

// FlushFakeIP forgets the fake-ip mappings.
func (c *Client) FlushFakeIP(ctx context.Context) error {
	return c.do(ctx, http.MethodPost, "/cache/fakeip/flush", nil, nil)
}

// FlushDNS empties the resolver's cache.
func (c *Client) FlushDNS(ctx context.Context) error {
	return c.do(ctx, http.MethodPost, "/cache/dns/flush", nil, nil)
}
