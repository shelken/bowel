package core

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
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
