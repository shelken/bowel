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

// externalApp is an app attached to a controller that h answers, with
// the OS faked so that a touch of the system proxy or the gateway fails
// the test. It returns the app and bowel's data directory.
func externalApp(t *testing.T, h http.Handler) (*App, string) {
	t.Helper()
	saved := osProxy
	osProxy.enable = func(string, int, []string) error { t.Error("the test tried to enable the system proxy"); return nil }
	osProxy.disable = func() error { t.Error("the test tried to disable the system proxy"); return nil }
	osProxy.snapshot = func() (sysproxy.Saved, error) { return sysproxy.Saved{}, nil }
	osProxy.restore = func(sysproxy.Saved) error { t.Error("the test tried to restore the system proxy"); return nil }
	t.Cleanup(func() { osProxy = saved })
	savedGw := osGateway
	osGateway = func() gateway.System { return &gwFake{t: t} }
	t.Cleanup(func() { osGateway = savedGw })
	t.Setenv("MOHO_CORE", "")

	dir := t.TempDir()
	mygo.App.SetPath(mygo.PathUserData, dir)
	t.Cleanup(func() { mygo.App.SetPath(mygo.PathUserData, "") })

	ctl := httptest.NewServer(h)
	t.Cleanup(ctl.Close)

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
	t.Cleanup(a.shutdown)
	return a, dir
}

// Attaching to someone else's core must not change anything: not the
// core (it may be the user's own sing-box), not the system proxy, and not
// bowel's own core directory. It needs no bundled core either.
func TestExternalCoreAttachIsReadOnly(t *testing.T) {
	var mu sync.Mutex
	var writes []string
	a, dir := externalApp(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
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

// The routes and nodes of an external core are the ones it reports, not
// bowel's template and not a provider's: a core that has no providers
// still has nodes.
func TestExternalCoreRoutesAndNodesAreItsOwn(t *testing.T) {
	a, _ := externalApp(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			t.Errorf("the external core was changed: %s %s", r.Method, r.URL.Path)
		}
		switch r.URL.Path {
		case "/version":
			io.WriteString(w, `{"meta":true,"version":"sing-box 1.14.4"}`)
		case "/proxies":
			io.WriteString(w, `{"proxies":{
				"GLOBAL":{"name":"GLOBAL","type":"Fallback","now":"main","all":["main"]},
				"main":{"name":"main","type":"Selector","now":"n1","all":["n1","n2","video"]},
				"video":{"name":"video","type":"Selector","now":"n2","all":["n1","n2"]},
				"fastest":{"name":"fastest","type":"URLTest","now":"n1","all":["n1","n2"]},
				"direct":{"name":"direct","type":"Direct"},
				"n2":{"name":"n2","type":"Hysteria2","udp":true},
				"n1":{"name":"n1","type":"Shadowsocks"}}}`)
		case "/rules":
			io.WriteString(w, `{"rules":[
				{"type":"default","payload":"domain=a.example","proxy":"route(video)"},
				{"type":"default","payload":"domain=b.example","proxy":"route(direct)"},
				{"type":"default","payload":"port=53","proxy":"hijack-dns"}]}`)
		case "/providers/proxies":
			io.WriteString(w, `{"providers":{}}`)
		case "/connections":
			io.WriteString(w, `{"connections":[]}`)
		default:
			http.NotFound(w, r)
		}
	}))
	a.refresh()

	s := a.snapshot()
	var routes []string
	for _, r := range s.Routes {
		routes = append(routes, r.ID+"="+r.Node)
	}
	if got, want := strings.Join(routes, " "), "default=n1 video=n2 direct="; got != want {
		t.Errorf("routes %q, want %q", got, want)
	}
	// What a route can be pointed at is its group's own members, groups
	// among them; the one in use is the group's choice, not the node at
	// the end of the chain.
	if !s.External {
		t.Error("the snapshot does not say the core is external")
	}
	for _, c := range []struct{ route, members, picked string }{
		{"default", "n1 n2 video", "n1"},
		{"video", "n1 n2", "n2"},
	} {
		var ms []string
		r := RouteView{}
		for _, v := range s.Routes {
			if v.ID == c.route {
				r = v
			}
		}
		for _, m := range r.Members {
			ms = append(ms, m.Name)
		}
		if strings.Join(ms, " ") != c.members || r.Picked != c.picked {
			t.Errorf("%s: members %q picked %q, want %q %q", c.route, ms, r.Picked, c.members, c.picked)
		}
	}
	var nodes []string
	for _, n := range s.Nodes {
		nodes = append(nodes, n.Name)
	}
	if got, want := strings.Join(nodes, " "), "n1 n2"; got != want {
		t.Errorf("nodes %q, want %q", got, want)
	}
}

// An external core's routes are found after the server is built, so the
// tools cannot list them: they must say where to look instead of listing
// nothing.
func TestExternalCoreMCPToolsDoNotListNoRoutes(t *testing.T) {
	a, _ := externalApp(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/version":
			io.WriteString(w, `{"version":"sing-box 1.14.4"}`)
		case "/proxies":
			io.WriteString(w, `{"proxies":{"GLOBAL":{"name":"GLOBAL","type":"Fallback","now":"proxy","all":["proxy"]}}}`)
		default:
			http.NotFound(w, r)
		}
	}))
	for _, tool := range a.mcpServer().Tools {
		props, _ := tool.Input["properties"].(map[string]any)
		if route, ok := props["route"].(map[string]any); ok {
			if d, _ := route["description"].(string); strings.Contains(d, ": .") || !strings.Contains(d, "list_routes") {
				t.Errorf("%s: route is described as %q", tool.Name, d)
			}
		}
	}
}
