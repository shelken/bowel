package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/egoist/mygo"

	"github.com/oboo/moho/internal/gateway"
	"github.com/oboo/moho/internal/mihomo"
	"github.com/oboo/moho/internal/routes"
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
	t.Setenv("MOHO_CORE", filepath.Join(t.TempDir(), "no-core")) // nothing to copy into a TUN install

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
	a.traceURL = "" // a test must not ask the internet where it is
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

// The external core is on bowel's mixed port too, and the system proxy
// that points there is the user's own: quitting must neither turn it off
// nor forget what it was.
func TestExternalCoreQuitLeavesSystemProxy(t *testing.T) {
	a, dir := externalApp(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/version":
			io.WriteString(w, `{"meta":true,"version":"sing-box 1.14.4"}`)
		case "/proxies":
			io.WriteString(w, `{"proxies":{"GLOBAL":{"name":"GLOBAL","type":"Fallback","now":"proxy","all":["proxy"]}}}`)
		default:
			io.WriteString(w, `{}`)
		}
	}))
	backup := a.proxyBackup()
	if err := os.WriteFile(backup, []byte(`{}`), 0o600); err != nil {
		t.Fatal(err)
	}
	a.shutdown()
	if _, err := os.Stat(backup); err != nil {
		t.Errorf("quitting dropped the proxy backup in %s: %v", dir, err)
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
				"lan":{"name":"lan","type":"Selector","now":"direct","all":["direct","n1"]},
				"fastest":{"name":"fastest","type":"URLTest","now":"n1","all":["n1","n2"]},
				"direct":{"name":"direct","type":"Direct"},
				"n2":{"name":"n2","type":"Hysteria2","udp":true},
				"n1":{"name":"n1","type":"Shadowsocks"}}}`)
		case "/rules":
			io.WriteString(w, `{"rules":[
				{"type":"default","payload":"domain=a.example","proxy":"route(video)"},
				{"type":"default","payload":"domain=b.example","proxy":"route(direct)"},
				{"type":"default","payload":"domain=c.example","proxy":"route(lan)"},
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
	if got, want := strings.Join(routes, " "), "default=n1 video=n2 direct= lan="; got != want {
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
	// Attached and answering is the one light's "ok": the core is not ours
	// to take over with, so there is no takeover to judge.
	if s.State != StateOK || s.Issue != nil || !s.Takeover.On || s.Takeover.System.Status != "off" || s.Takeover.Tun.Status != "off" || s.Takeover.Gateway.Status != "off" {
		t.Errorf("state %q, issue %+v, takeover %+v", s.State, s.Issue, s.Takeover)
	}
	var nodes []string
	for _, n := range s.Nodes {
		nodes = append(nodes, n.Name)
	}
	if got, want := strings.Join(nodes, " "), "n1 n2"; got != want {
		t.Errorf("nodes %q, want %q", got, want)
	}
}

// The mode and the user rules in the settings are the built-in core's. An
// external core has its own, and bowel's interface has no control to change
// them back, so a mode or a rule left from before must not show.
func TestExternalCoreIgnoresBuiltInModeAndRules(t *testing.T) {
	a, _ := externalApp(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/version":
			io.WriteString(w, `{"version":"sing-box 1.14.4"}`)
		case "/proxies":
			io.WriteString(w, `{"proxies":{
				"GLOBAL":{"name":"GLOBAL","type":"Fallback","now":"main","all":["main"]},
				"main":{"name":"main","type":"Selector","now":"n1","all":["n1"]},
				"video":{"name":"video","type":"Selector","now":"n1","all":["n1"]},
				"n1":{"name":"n1","type":"Shadowsocks"}}}`)
		case "/rules":
			io.WriteString(w, `{"rules":[{"type":"default","payload":"domain=a.example","proxy":"route(video)"}]}`)
		case "/providers/proxies":
			io.WriteString(w, `{"providers":{}}`)
		case "/connections":
			io.WriteString(w, `{"connections":[]}`)
		default:
			http.NotFound(w, r)
		}
	}))
	if _, err := a.settings.Update(func(s *settings.Settings) {
		s.Mode = "direct"
		s.Rules = []routes.UserRule{{Kind: routes.KindSuffix, Value: "x.example", Route: "video"}}
	}); err != nil {
		t.Fatal(err)
	}
	a.refresh()

	s := a.snapshot()
	if s.Mode != "rule" {
		t.Errorf("mode %q, want rule", s.Mode)
	}
	for _, r := range s.Routes {
		if r.Mine != 0 {
			t.Errorf("route %s counts %d of the built-in core's user rules", r.ID, r.Mine)
		}
	}
	if want := routes.Count(a.routeTable()); s.RuleCount != want {
		t.Errorf("rule count %d, want the core's %d", s.RuleCount, want)
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
		props, _ := tool.InputSchema()["properties"].(map[string]any)
		if route, ok := props["route"].(map[string]any); ok {
			if d, _ := route["description"].(string); strings.Contains(d, ": .") || !strings.Contains(d, "list_routes") {
				t.Errorf("%s: route is described as %q", tool.Name, d)
			}
		}
	}
}

// Nothing a person does at the interface may change the core, or what is
// around it, when the core is someone else's: not its config, not the
// system proxy or TUN that bowel would otherwise set up for its own core.
// A refused action keeps the settings as they were.
func TestExternalCoreRefusesToBeConfigured(t *testing.T) {
	var mu sync.Mutex
	var writes []string
	a, _ := externalApp(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			mu.Lock()
			writes = append(writes, r.Method+" "+r.URL.Path)
			mu.Unlock()
		}
		switch r.URL.Path {
		case "/version":
			io.WriteString(w, `{"version":"sing-box 1.14.4"}`)
		case "/proxies":
			io.WriteString(w, `{"proxies":{"GLOBAL":{"name":"GLOBAL","type":"Fallback","now":"main","all":["main"]},
				"main":{"name":"main","type":"Selector","now":"n1","all":["n1"]},"n1":{"name":"n1","type":"Shadowsocks"}}}`)
		case "/rules":
			io.WriteString(w, `{"rules":[]}`)
		default:
			w.WriteHeader(http.StatusNoContent)
		}
	}))
	a.refresh()
	cwd := t.TempDir()
	t.Chdir(cwd)
	before := a.settings.Get()

	for name, act := range map[string]func() error{
		"takeover":     func() error { return a.setTakeover(true) },
		"system proxy": func() error { return a.setSystemProxy(true) },
		"TUN":          func() error { return a.setTun(true) },
		"LAN":          func() error { return a.setLan(true) },
		"mode":         func() error { return a.setMode("global") },
		"gateway":      func() error { return a.setGateway(true) },
		"port":         func() error { return a.updateSettings(func(s *settings.Settings) { s.MixedPort = 17999 }) },
		"provider": func() error {
			_, err := a.addProvider(context.Background(), "hysteria2://pw@127.0.0.1:443?sni=x#test")
			return err
		},
		"geo":       a.updateGeo,
		"flush DNS": a.flushDNS,
		"rule": func() error {
			_, err := a.addRule(routes.UserRule{Kind: routes.KindSuffix, Value: "example.com", Route: "default"})
			return err
		},
	} {
		// Refused for what it is, not for failing at a later step.
		if err := act(); err == nil || err.Error() != a.errExternal().Error() && !errors.Is(err, mihomo.ErrReadOnly) {
			t.Errorf("%s: %v", name, err)
		}
	}
	if ps := a.providers.List(); len(ps) > 0 {
		t.Errorf("a provider was added: %v", ps)
	}
	_ = a.resetSettings()
	if got := a.settings.Get(); !reflect.DeepEqual(got, before) {
		t.Errorf("settings changed:\n%+v\n%+v", before, got)
	}
	if ents, _ := os.ReadDir(cwd); len(ents) > 0 {
		t.Errorf("files were written: %v", ents)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(writes) > 0 {
		t.Errorf("the external core was changed: %v", writes)
	}
}

// Which core bowel drives is read at the next start: choosing another
// leaves the running core alone, whichever core that is.
func TestCoreSourceChangesWaitForARestart(t *testing.T) {
	var mu sync.Mutex
	var writes []string
	a, _ := externalApp(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			mu.Lock()
			writes = append(writes, r.Method+" "+r.URL.Path)
			mu.Unlock()
		}
		switch r.URL.Path {
		case "/version":
			io.WriteString(w, `{"version":"sing-box 1.14.4"}`)
		case "/proxies":
			io.WriteString(w, `{"proxies":{"GLOBAL":{"name":"GLOBAL","type":"Fallback","now":"main","all":["main"]}}}`)
		default:
			w.WriteHeader(http.StatusNoContent)
		}
	}))
	for name, fn := range map[string]func(*settings.Settings){
		"address": func(s *settings.Settings) { s.CoreAddr = "127.0.0.1:19090" },
		"secret":  func(s *settings.Settings) { s.CoreSecret = "x" },
		"source":  func(s *settings.Settings) { s.CoreSource = "" },
	} {
		if err := a.updateSettings(fn); err != nil {
			t.Errorf("%s: %v", name, err)
		}
	}
	if s := a.settings.Get(); s.CoreAddr != "127.0.0.1:19090" || s.CoreSecret != "x" || s.CoreSource != "" {
		t.Errorf("not kept: %+v", s)
	}
	if !a.core.External() {
		t.Error("the running core changed")
	}
	mu.Lock()
	defer mu.Unlock()
	if len(writes) > 0 {
		t.Errorf("the core was changed: %v", writes)
	}
}

// The tick counts today's bytes per route while it holds a.mu, and finds
// the route through the table, which takes a.mu too. Once it did both at
// the same time, and the whole app hung at the first byte of traffic.
func TestTickWithTrafficDoesNotHang(t *testing.T) {
	var polls atomic.Int64
	a, _ := externalApp(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/version":
			io.WriteString(w, `{"version":"sing-box 1.14.4"}`)
		case "/proxies":
			io.WriteString(w, `{"proxies":{"GLOBAL":{"name":"GLOBAL","type":"Fallback","now":"main","all":["main"]},
				"main":{"name":"main","type":"Selector","now":"n1","all":["n1"]},"n1":{"name":"n1","type":"Shadowsocks"}}}`)
		case "/rules":
			io.WriteString(w, `{"rules":[]}`)
		case "/connections":
			fmt.Fprintf(w, `{"connections":[{"id":"c1","download":%d,"upload":0,"start":%q,"chains":["n1","main"],"rule":"default","metadata":{"host":"a.example","destinationPort":"443","network":"tcp"}}]}`,
				1000*polls.Add(1), time.Now().Format(time.RFC3339))
		default:
			http.NotFound(w, r)
		}
	}))
	a.refresh()
	deadline := time.Now().Add(10 * time.Second)
	for a.mon.Now().GroupBytes["main"] == 0 {
		if time.Now().After(deadline) {
			t.Fatal("the monitor counted no bytes")
		}
		a.mon.Wake()
		time.Sleep(100 * time.Millisecond)
	}
	done := make(chan Tick, 1)
	go func() { done <- a.buildTick() }()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("buildTick hangs")
	}
}

// The core to drive is read at the start, so a saved choice that is not
// the running one waits for a restart, and the window offers it.
func TestChoosingAnotherCoreNeedsARestart(t *testing.T) {
	a, _ := externalApp(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/version":
			io.WriteString(w, `{"version":"sing-box 1.14.4"}`)
		case "/proxies":
			io.WriteString(w, `{"proxies":{"GLOBAL":{"name":"GLOBAL","type":"Fallback","now":"main","all":["main"]},
				"main":{"name":"main","type":"Selector","now":"n1","all":["n1"]},"n1":{"name":"n1","type":"Shadowsocks"}}}`)
		case "/rules":
			io.WriteString(w, `{"rules":[]}`)
		default:
			w.WriteHeader(http.StatusNoContent)
		}
	}))
	if a.snapshot().RestartNeeded {
		t.Fatal("a restart is asked for before anything changed")
	}
	run := a.settings.Get()
	for name, change := range map[string]func(*settings.Settings){
		"built-in":  func(s *settings.Settings) { s.CoreSource, s.CoreAddr = "", "" },
		"address":   func(s *settings.Settings) { s.CoreAddr = "127.0.0.1:19090" },
		"secret":    func(s *settings.Settings) { s.CoreSecret = "s" },
		"same core": func(s *settings.Settings) { s.CoreAddr = run.CoreAddr },
	} {
		if err := a.updateSettings(change); err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if got, want := a.snapshot().RestartNeeded, name != "same core"; got != want {
			t.Errorf("%s: RestartNeeded %v, want %v", name, got, want)
		}
		if err := a.updateSettings(func(s *settings.Settings) { *s = run }); err != nil {
			t.Fatal(err)
		}
		if a.snapshot().RestartNeeded {
			t.Errorf("%s: a restart is asked for after the choice went back", name)
		}
	}
}

// A probe that fails leaves the node's place a guess; the log says why, for
// a core that can be probed. An external one cannot, so nothing is tried.
func TestProbeFailuresAreLogged(t *testing.T) {
	var out strings.Builder
	log.SetOutput(&out)
	t.Cleanup(func() { log.SetOutput(os.Stderr) })

	built, _, _ := mcpApp(t) // its core is not running
	built.probe("n1")
	if !strings.Contains(out.String(), "locate:") {
		t.Errorf("the built-in core's failed probe left no trace: %q", out.String())
	}

	out.Reset()
	ext, _ := externalApp(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/version":
			io.WriteString(w, `{"version":"sing-box 1.14.4"}`)
		case "/proxies":
			io.WriteString(w, `{"proxies":{"GLOBAL":{"name":"GLOBAL","type":"Fallback","now":"main","all":["main"]},
				"main":{"name":"main","type":"Selector","now":"n1","all":["n1"]},"n1":{"name":"n1","type":"Shadowsocks"}}}`)
		case "/rules":
			io.WriteString(w, `{"rules":[]}`)
		default:
			w.WriteHeader(http.StatusNoContent)
		}
	}))
	out.Reset()
	ext.probe("n1")
	if out.Len() != 0 {
		t.Errorf("an external core was probed: %q", out.String())
	}
}

// The routes of an external core are found after the MCP server is built, so
// the tools say them when they are listed.
func TestExternalRoutesAreChoicesOfTheTools(t *testing.T) {
	a, _ := externalApp(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/version":
			io.WriteString(w, `{"version":"sing-box 1.14.4"}`)
		case "/proxies":
			io.WriteString(w, `{"proxies":{"GLOBAL":{"name":"GLOBAL","type":"Fallback","now":"main","all":["main"]},
				"main":{"name":"main","type":"Selector","now":"n1","all":["n1"]},
				"video":{"name":"video","type":"Selector","now":"n1","all":["n1"]},"n1":{"name":"n1","type":"Shadowsocks"}}}`)
		case "/rules":
			io.WriteString(w, `{"rules":[{"type":"default","payload":"a","proxy":"route(video)"}]}`)
		default:
			w.WriteHeader(http.StatusNoContent)
		}
	}))
	srv := a.mcpServer() // before the routes are known
	a.refresh()
	for _, tool := range srv.Tools {
		if tool.Name != "select_node" {
			continue
		}
		route, _ := tool.InputSchema()["properties"].(map[string]any)["route"].(map[string]any)
		if got := fmt.Sprint(route["enum"]); got != "[default video]" {
			t.Errorf("route may be %s", got)
		}
		return
	}
	t.Fatal("no select_node")
}

// An external core has no probe group to point at a node, so where its
// nodes exit is learned from bowel's own request: the node that carried it
// gets the place the trace names. A node measured lately is not measured
// again, and nothing of the core is changed.
func TestExternalCoreLocatesTheDefaultRoutesExit(t *testing.T) {
	var mu sync.Mutex
	var port string
	var open bool
	var traces int
	trace := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		traces++
		mu.Unlock()
		io.WriteString(w, "ip=203.0.113.7\ncolo=NRT\nloc=JP\n")
	}))
	trace.Config.ConnState = func(c net.Conn, s http.ConnState) {
		mu.Lock()
		defer mu.Unlock()
		switch s {
		case http.StateNew:
			_, port, _ = net.SplitHostPort(c.RemoteAddr().String())
			open = true
		case http.StateClosed, http.StateHijacked:
			open = false
		}
	}
	trace.Start()
	defer trace.Close()

	a, _ := externalApp(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			t.Errorf("the external core was changed: %s %s", r.Method, r.URL.Path)
		}
		switch r.URL.Path {
		case "/version":
			io.WriteString(w, `{"version":"sing-box 1.14.4"}`)
		case "/proxies":
			io.WriteString(w, `{"proxies":{"GLOBAL":{"name":"GLOBAL","type":"Fallback","now":"main","all":["main"]},
				"main":{"name":"main","type":"Selector","now":"relay-7","all":["relay-7"]},"relay-7":{"name":"relay-7","type":"Shadowsocks"}}}`)
		case "/rules":
			io.WriteString(w, `{"rules":[]}`)
		case "/providers/proxies":
			io.WriteString(w, `{"providers":{}}`)
		case "/connections":
			mu.Lock()
			defer mu.Unlock()
			if !open {
				io.WriteString(w, `{"connections":[]}`)
				return
			}
			fmt.Fprintf(w, `{"connections":[{"id":"c1","chains":["relay-7","main"],"rule":"final","metadata":{"sourcePort":%q}}]}`, port)
		default:
			http.NotFound(w, r)
		}
	}))
	a.refresh()
	where := func() Located {
		for _, n := range a.snapshot().Nodes {
			if n.Name == "relay-7" {
				return n.Where
			}
		}
		t.Fatal("no node relay-7")
		return Located{}
	}

	// Without a trace page there is nothing to ask.
	a.mu.Lock()
	a.traceURL = ""
	a.mu.Unlock()
	a.locateExits(false)
	if w := where(); w.Source != "unknown" {
		t.Fatalf("before: %+v", w)
	}

	a.mu.Lock()
	a.traceURL = trace.URL
	a.mu.Unlock()
	a.locateExits(false)
	if w := where(); w.Source != "probe" || w.Place == nil || w.Place.Country != "JP" {
		t.Fatalf("after: %+v", w)
	}
	a.locateExits(false)
	mu.Lock()
	defer mu.Unlock()
	if traces != 1 {
		t.Errorf("%d traces, want 1: a recent measurement is enough", traces)
	}
}
