package core

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// A controller that is not ours (sing-box's Clash API) can restart at any
// time, so the manager has to follow it down and back up.
func TestExternalFollowsController(t *testing.T) {
	var alive atomic.Bool
	alive.Store(true)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !alive.Load() {
			http.Error(w, "down", http.StatusServiceUnavailable)
			return
		}
		switch r.URL.Path {
		case "/version":
			io.WriteString(w, `{"meta":true,"premium":true,"version":"sing-box 1.14.4"}`)
		case "/proxies":
			io.WriteString(w, `{"proxies":{"GLOBAL":{"name":"GLOBAL","type":"Fallback","now":"proxy","all":["proxy"]}}}`)
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	m := NewExternal(strings.TrimPrefix(srv.URL, "http://"), "")
	m.PollEvery = 10 * time.Millisecond
	if err := m.Connect(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(m.Stop)

	if s := m.Status(); s.State != Running || s.Version != "sing-box 1.14.4" {
		t.Fatalf("after Connect: %+v", s)
	}
	if _, err := m.Client(); err != nil {
		t.Fatal(err)
	}

	alive.Store(false)
	waitCoreState(t, m, Failed)
	if _, err := m.Client(); err == nil {
		t.Fatal("Client answered while the controller was down")
	}

	alive.Store(true)
	waitCoreState(t, m, Running)
}

func waitCoreState(t *testing.T, m *Manager, want State) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if m.Status().State == want {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("state is %s, want %s", m.Status().State, want)
}

// The core is someone else's: the one thing bowel may ask of it is to
// pick a node for a group. Everything else a client can do changes the
// core, and must not reach it.
func TestExternalClientOnlyPicksNodes(t *testing.T) {
	var mu sync.Mutex
	var got []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		got = append(got, r.Method+" "+r.URL.Path)
		mu.Unlock()
		switch r.URL.Path {
		case "/version":
			io.WriteString(w, `{"version":"sing-box 1.14.4"}`)
		case "/proxies":
			io.WriteString(w, `{"proxies":{"GLOBAL":{"name":"GLOBAL","type":"Fallback","now":"proxy","all":["proxy"]}}}`)
		default:
			w.WriteHeader(http.StatusNoContent)
		}
	}))
	defer srv.Close()
	m := NewExternal(strings.TrimPrefix(srv.URL, "http://"), "")
	if err := m.Connect(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(m.Stop)
	c, err := m.Client()
	if err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	got = nil
	mu.Unlock()

	ctx := context.Background()
	for name, err := range map[string]error{
		"patch":        c.Patch(ctx, map[string]any{"mode": "global"}),
		"reload":       c.Reload(ctx, "/tmp/x.yaml"),
		"close one":    c.CloseConnection(ctx, "id"),
		"close all":    c.CloseAllConnections(ctx),
		"geo":          c.UpdateGeo(ctx),
		"flush dns":    c.FlushDNS(ctx),
		"flush fakeip": c.FlushFakeIP(ctx),
	} {
		if err == nil {
			t.Errorf("%s was let through", name)
		}
	}
	if err := c.Select(ctx, "main", "n1"); err != nil {
		t.Errorf("select: %v", err)
	}
	if _, err := c.Proxies(ctx); err != nil {
		t.Errorf("proxies: %v", err)
	}
	mu.Lock()
	defer mu.Unlock()
	if want := "[PUT /proxies/main GET /proxies]"; fmt.Sprint(got) != want {
		t.Errorf("the controller saw %v, want %s", got, want)
	}
}
