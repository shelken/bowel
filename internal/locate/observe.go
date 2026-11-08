package locate

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strconv"
	"sync/atomic"

	"github.com/oboo/moho/internal/mihomo"
)

// Observe measures the exit of whichever node the core sends bowel's own
// traffic through, for a core whose config bowel cannot extend with a probe
// listener. It requests the trace at target and, while that connection is
// still open, finds it in the core's connection list by the port it was made
// from. The node is the first proxy of the connection's chain that is not a
// group, and the trace's answer is kept under its name.
//
// A request the core never saw (nothing routes bowel's traffic into it) is
// not in the list, and a request that left the core direct was carried by no
// node; both are errors, and nothing is kept.
func (l *Locator) Observe(ctx context.Context, c *mihomo.Client, target string) (string, Result, error) {
	select {
	case l.slot <- struct{}{}:
	case <-ctx.Done():
		return "", Result{Source: SourceUnknown, At: l.now()}, ctx.Err()
	}
	defer func() { <-l.slot }()

	unknown := func(err error) (string, Result, error) {
		return "", Result{Source: SourceUnknown, At: l.now()}, err
	}
	var port atomic.Int64
	tr := &http.Transport{DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
		var d net.Dialer
		conn, err := d.DialContext(ctx, network, addr)
		if err != nil {
			return nil, err
		}
		if a, ok := conn.LocalAddr().(*net.TCPAddr); ok {
			port.Store(int64(a.Port))
		}
		return conn, nil
	}}
	// Closing the idle connection is what ends it: it must stay open while
	// the core's list is read.
	defer tr.CloseIdleConnections()
	body, err := fetch(ctx, &http.Client{Timeout: probeTimeout, Transport: tr}, target)
	if err != nil {
		return unknown(err)
	}
	snap, err := c.Connections(ctx)
	if err != nil {
		return unknown(err)
	}
	px, err := c.Proxies(ctx)
	if err != nil {
		return unknown(err)
	}
	var chain []string
	for _, cn := range snap.Connections {
		if cn.Metadata.SourcePort == strconv.FormatInt(port.Load(), 10) {
			chain = cn.Chains
			break
		}
	}
	if len(chain) == 0 {
		return unknown(errors.New("the core did not carry the trace request"))
	}
	node := ""
	for _, n := range chain {
		if p, ok := px[n]; ok && !isGroup(p.Type) {
			node = n
			break
		}
	}
	if node == "" || px[node].Type == "Direct" {
		return unknown(errors.New("the trace request left the core without a node"))
	}
	r, err := l.record(node, body)
	return node, r, err
}

func isGroup(t string) bool {
	switch t {
	case "Selector", "URLTest", "Fallback", "LoadBalance", "Relay":
		return true
	}
	return false
}

func fetch(ctx context.Context, client *http.Client, target string) (string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, target, nil)
	if err != nil {
		return "", err
	}
	resp, err := client.Do(req)
	if err != nil {
		return "", fmt.Errorf("trace: %w", err)
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	if err != nil {
		return "", fmt.Errorf("trace: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("trace: %s", resp.Status)
	}
	return string(b), nil
}
