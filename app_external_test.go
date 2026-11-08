package main

import (
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/egoist/mygo"

	"github.com/oboo/moho/internal/gateway"
	"github.com/oboo/moho/internal/settings"
	"github.com/oboo/moho/internal/sysproxy"
)

// Attaching to someone else's core must not change anything: not the
// core (it may be the user's own sing-box), not the system proxy, and not
// bowel's own core directory. It needs no bundled core either.
func TestExternalCoreAttachIsReadOnly(t *testing.T) {
	saved := osProxy
	osProxy.enable = func(string, int, []string) error { t.Error("the test tried to enable the system proxy"); return nil }
	osProxy.disable = func() error { t.Error("the test tried to disable the system proxy"); return nil }
	osProxy.snapshot = func() (sysproxy.Saved, error) { return sysproxy.Saved{}, nil }
	osProxy.restore = func(sysproxy.Saved) error { t.Error("the test tried to restore the system proxy"); return nil }
	defer func() { osProxy = saved }()
	savedGw := osGateway
	osGateway = func() gateway.System { return &gwFake{t: t} }
	defer func() { osGateway = savedGw }()
	t.Setenv("MOHO_CORE", "")

	dir := t.TempDir()
	mygo.App.SetPath(mygo.PathUserData, dir)
	defer mygo.App.SetPath(mygo.PathUserData, "")

	var mu sync.Mutex
	var writes []string
	ctl := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			mu.Lock()
			writes = append(writes, r.Method+" "+r.URL.Path)
			mu.Unlock()
		}
		switch r.URL.Path {
		case "/version":
			io.WriteString(w, `{"meta":true,"premium":true,"version":"sing-box 1.14.4"}`)
		case "/proxies":
			io.WriteString(w, `{"proxies":{"GLOBAL":{"name":"GLOBAL","type":"Fallback","now":"proxy","all":["proxy"]}}}`)
		case "/providers/proxies":
			io.WriteString(w, `{"providers":{}}`)
		case "/connections":
			io.WriteString(w, `{"connections":[]}`)
		default:
			http.NotFound(w, r)
		}
	}))
	defer ctl.Close()

	st, err := settings.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.Update(func(s *settings.Settings) {
		s.CoreSource, s.CoreAddr = "external", strings.TrimPrefix(ctl.URL, "http://")
	}); err != nil {
		t.Fatal(err)
	}

	a, err := newApp()
	if err != nil {
		t.Fatal(err)
	}
	if err := a.startCore(); err != nil {
		t.Fatalf("startCore: %v", err)
	}
	defer a.shutdown()

	if s := a.snapshot(); s.Core.State != "running" {
		t.Fatalf("core %+v", s.Core)
	}
	// What onCoreStatus and the loop do once the core runs.
	a.applySystemProxy()
	a.locateExits(true)
	a.refresh()

	mu.Lock()
	defer mu.Unlock()
	if len(writes) > 0 {
		t.Errorf("the external core was changed: %v", writes)
	}
	if _, err := os.Stat(filepath.Join(dir, "core")); !os.IsNotExist(err) {
		t.Errorf("bowel made a core directory: %v", err)
	}
}
