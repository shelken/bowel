package main

import (
	"context"
	"encoding/json"
	"os"
	"slices"
	"strings"
	"testing"

	"github.com/egoist/mygo"

	"github.com/oboo/moho/internal/core"
	"github.com/oboo/moho/internal/i18n"
	"github.com/oboo/moho/internal/mcp"
	"github.com/oboo/moho/internal/settings"
	"github.com/oboo/moho/internal/sysproxy"
)

// mcpApp is an App on a data folder of its own, with the system proxy out
// of reach and no dialog: asks counts the questions, answer gives the
// user's answer.
func mcpApp(t *testing.T) (a *App, asks *int, answer *[2]bool) {
	t.Helper()
	saved := osProxy
	osProxy.enable = func(string, int, []string) error { t.Error("the test tried to enable the system proxy"); return nil }
	osProxy.disable = func() error { t.Error("the test tried to disable the system proxy"); return nil }
	osProxy.snapshot = func() (sysproxy.Saved, error) { return sysproxy.Saved{}, nil }
	osProxy.restore = func(sysproxy.Saved) error { t.Error("the test tried to restore the system proxy"); return nil }
	savedAsk := askAgent
	asks, answer = new(int), new([2]bool)
	askAgent = func(ctx context.Context, client, what, detail string) (bool, bool, error) {
		*asks++
		return answer[0], answer[1], nil
	}
	dir, err := os.MkdirTemp("", "mh") // short: unix socket paths are limited
	if err != nil {
		t.Fatal(err)
	}
	mygo.App.SetPath(mygo.PathUserData, dir)
	agentLog.list = nil
	t.Cleanup(func() {
		osProxy, askAgent = saved, savedAsk
		agentLog.list = nil
		mygo.App.SetPath(mygo.PathUserData, "")
		os.RemoveAll(dir)
		i18n.Set("zh")
	})
	a, err = newApp()
	if err != nil {
		t.Fatal(err)
	}
	return a, asks, answer
}

// callTool runs a tool as an agent's call would.
func callTool(t *testing.T, srv *mcp.Server, s *mcp.Session, name string, args any) (map[string]any, error) {
	t.Helper()
	for _, tool := range srv.Tools {
		if tool.Name != name {
			continue
		}
		raw, _ := json.Marshal(args)
		if args == nil {
			raw = []byte("{}")
		}
		out, err := tool.Run(context.Background(), s, raw)
		if err != nil {
			return nil, err
		}
		m := map[string]any{}
		if str, ok := out.(string); ok {
			m["text"] = str
		} else {
			b, _ := json.Marshal(out)
			_ = json.Unmarshal(b, &m)
		}
		return m, nil
	}
	t.Fatalf("no tool %s", name)
	return nil, nil
}

func setPolicy(t *testing.T, a *App, p string) {
	t.Helper()
	if _, err := a.settings.Update(func(s *settings.Settings) { s.MCPPolicy = p }); err != nil {
		t.Fatal(err)
	}
}

// TestMCPPolicy: reading is always allowed; a change asks, goes through or
// is refused as the policy says, and the policy itself is the user's.
func TestMCPPolicy(t *testing.T) {
	a, asks, answer := mcpApp(t)
	srv := a.mcpServer()
	s := &mcp.Session{}
	lang := map[string]any{"changes": map[string]any{"language": "en"}}
	language := func() string { return a.settings.Get().Language }

	if a.settings.Get().Policy() != "ask" {
		t.Fatalf("the default policy is %q, want ask", a.settings.Get().Policy())
	}

	// Refused.
	setPolicy(t, a, "deny")
	for _, name := range []string{"get_status", "get_settings", "list_routes", "list_rules", "get_whitelists", "list_providers", "list_connections", "get_logs"} {
		if _, err := callTool(t, srv, s, name, nil); err != nil {
			t.Errorf("%s under deny: %v", name, err)
		}
	}
	if out, err := callTool(t, srv, s, "explain", map[string]any{"target": "https://api.openai.com/v1"}); err != nil || out["route"] != "ai" {
		t.Errorf("explain: %v %v", out, err)
	}
	if _, err := callTool(t, srv, s, "update_settings", lang); err == nil || language() != "zh" {
		t.Errorf("deny let a change through: %v, language %s", err, language())
	}
	if *asks != 0 {
		t.Errorf("deny asked the user")
	}

	// Asked, and refused by the user.
	setPolicy(t, a, "ask")
	if _, err := callTool(t, srv, s, "update_settings", lang); err == nil || language() != "zh" || *asks != 1 {
		t.Errorf("a refused change: %v, language %s, asks %d", err, language(), *asks)
	}
	// Allowed once: the next change asks again.
	*answer = [2]bool{true, false}
	if out, err := callTool(t, srv, s, "update_settings", lang); err != nil || language() != "en" {
		t.Fatalf("an allowed change: %v %v", out, err)
	} else if ch := out["changed"].([]any); len(ch) != 1 || ch[0] != "language: zh → en" {
		t.Errorf("changed: %v", ch)
	}
	if out, _ := callTool(t, srv, s, "update_settings", lang); !strings.Contains(out["text"].(string), "Nothing to change") || *asks != 2 {
		t.Errorf("no change should not ask: %v, asks %d", out, *asks)
	}
	if _, err := callTool(t, srv, s, "update_settings", map[string]any{"changes": map[string]any{"language": "ja"}}); err != nil || *asks != 3 {
		t.Errorf("allowed once should ask again: %v, asks %d", err, *asks)
	}
	// Allowed for the connection.
	*answer = [2]bool{true, true}
	callTool(t, srv, s, "update_settings", map[string]any{"changes": map[string]any{"language": "zh-Hant"}})
	callTool(t, srv, s, "update_settings", map[string]any{"changes": map[string]any{"language": "en"}})
	if *asks != 4 || language() != "en" {
		t.Errorf("remembered: asks %d, language %s", *asks, language())
	}
	if _, err := callTool(t, srv, &mcp.Session{}, "update_settings", map[string]any{"changes": map[string]any{"language": "zh"}}); err != nil || *asks != 5 {
		t.Errorf("another connection should be asked: %v, asks %d", err, *asks)
	}

	// Always allowed.
	setPolicy(t, a, "allow")
	*answer = [2]bool{false, false}
	if _, err := callTool(t, srv, &mcp.Session{}, "update_settings", lang); err != nil || language() != "en" || *asks != 5 {
		t.Errorf("allow: %v, language %s, asks %d", err, language(), *asks)
	}

	// The policy and what has tools of its own can't be changed here, and
	// what is wrong fails before anyone is asked.
	setPolicy(t, a, "ask")
	for _, bad := range []map[string]any{
		{"mcpPolicy": "allow"}, {"mode": "global"}, {"tun": true}, {"rules": []any{}},
		{"nope": 1}, {"language": 3}, {"language": "xx"}, {"mixedPort": 0},
	} {
		if _, err := callTool(t, srv, s, "update_settings", map[string]any{"changes": bad}); err == nil {
			t.Errorf("%v should fail", bad)
		}
	}
	if a.settings.Get().MCPPolicy != "ask" || *asks != 5 {
		t.Errorf("policy %q, asks %d", a.settings.Get().MCPPolicy, *asks)
	}

	// Secrets are hidden, and the hidden value can't be written back.
	if _, err := a.settings.Update(func(s *settings.Settings) { s.ExtSecret, s.Authentication, s.CoreSecret = "hunter2", []string{"me:pw"}, "sbsecret" }); err != nil {
		t.Fatal(err)
	}
	out, _ := callTool(t, srv, s, "get_settings", nil)
	b, _ := json.Marshal(out)
	if strings.Contains(string(b), "hunter2") || strings.Contains(string(b), "sbsecret") || strings.Contains(string(b), "pw\"") || !strings.Contains(string(b), "me:••••") {
		t.Errorf("secrets shown: %s", b)
	}
	if ed := out["editable"].([]any); slices.Contains(ed, any("mcpPolicy")) || !slices.Contains(ed, any("logLevel")) {
		t.Errorf("editable: %v", ed)
	}
	if _, err := callTool(t, srv, s, "update_settings", map[string]any{"changes": map[string]any{"extSecret": "••••"}}); err == nil {
		t.Error("writing back the hidden secret should fail")
	}
	if _, err := callTool(t, srv, s, "update_settings", map[string]any{"changes": map[string]any{"coreSecret": "••••"}}); err == nil {
		t.Error("writing back the hidden core secret should fail")
	}

	// The page's list has what happened, newest first.
	list := agentChanges()
	if len(list) == 0 || list[0].Outcome != "applied" || !slices.ContainsFunc(list, func(c agentChange) bool { return c.Outcome == "denied" }) {
		t.Errorf("agent changes: %+v", list)
	}
}

// TestMCPChanges drives the change tools against a real core, on a port of
// its own; the system proxy is out of reach.
func TestMCPChanges(t *testing.T) {
	if testing.Short() {
		t.Skip("starts the core")
	}
	a, asks, _ := mcpApp(t)
	if !exists(bundledCore()) {
		t.Skip("no core; run scripts/fetch-core.sh")
	}
	port, err := core.FreePort()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := a.settings.Update(func(s *settings.Settings) { s.MixedPort, s.MCPPolicy = port, "allow" }); err != nil {
		t.Fatal(err)
	}
	a.copyGeo()
	if err := a.applyConfig(); err != nil {
		t.Fatalf("start: %v", err)
	}
	defer a.shutdown()
	srv := a.mcpServer()
	s := &mcp.Session{}
	must := func(name string, args any) map[string]any {
		t.Helper()
		out, err := callTool(t, srv, s, name, args)
		if err != nil {
			t.Fatalf("%s %v: %v", name, args, err)
		}
		return out
	}

	// Rules.
	must("add_rule", map[string]any{"target": "https://api.example.com/x", "route": "dev"})
	if r := a.settings.Get().Rules; len(r) != 1 || r[0].Value != "api.example.com" || r[0].Kind != "suffix" || r[0].Route != "dev" {
		t.Fatalf("rules: %+v", r)
	}
	if out := must("explain", map[string]any{"target": "v2.api.example.com"}); out["route"] != "dev" || out["userRule"] != true {
		t.Errorf("explain: %v", out)
	}
	must("add_rule", map[string]any{"target": "api.example.com", "route": "AI"}) // by title, replacing
	if r := a.settings.Get().Rules; len(r) != 1 || r[0].Route != "ai" {
		t.Errorf("replaced: %+v", r)
	}
	if _, err := callTool(t, srv, s, "add_rule", map[string]any{"target": "x.com", "route": "nowhere"}); err == nil {
		t.Error("a rule to no route should fail")
	}
	must("remove_rule", map[string]any{"target": "api.example.com"})
	if r := a.settings.Get().Rules; len(r) != 0 {
		t.Errorf("removed: %+v", r)
	}

	// Whitelists.
	must("whitelist_add", map[string]any{"list": "direct", "entries": []string{"example.cn", "10.1.0.0/16"}})
	w := must("get_whitelists", nil)
	if d := w["direct"].([]any); len(d) != 2 {
		t.Errorf("direct: %v", d)
	}
	must("whitelist_add", map[string]any{"list": "bypass", "entries": []string{"*.corp.example"}})
	if !slices.Contains(a.settings.Get().Bypass, "*.corp.example") {
		t.Errorf("bypass: %v", a.settings.Get().Bypass)
	}
	if _, err := callTool(t, srv, s, "whitelist_add", map[string]any{"list": "bypass", "entries": []string{"a b"}}); err == nil {
		t.Error("a malformed bypass entry should fail")
	}
	must("whitelist_remove", map[string]any{"list": "bypass", "entries": []string{"*.corp.example"}})
	must("whitelist_remove", map[string]any{"list": "direct", "entries": []string{"example.cn"}})
	if r := a.settings.Get().Rules; len(r) != 1 || r[0].Value != "10.1.0.0/16" || slices.Contains(a.settings.Get().Bypass, "*.corp.example") {
		t.Errorf("after removing: rules %+v bypass %v", r, a.settings.Get().Bypass)
	}
	if _, err := callTool(t, srv, s, "whitelist_remove", map[string]any{"list": "lan", "entries": []string{"1.2.3.4/32"}}); err == nil {
		t.Error("removing what isn't there should fail")
	}

	// Settings that reload the core, and the mode.
	must("update_settings", map[string]any{"changes": map[string]any{"logLevel": "debug", "ipv6": false}})
	if st := a.settings.Get(); st.LogLevel != "debug" || st.IPv6 {
		t.Errorf("settings: %s %v", st.LogLevel, st.IPv6)
	}
	must("set_mode", map[string]any{"mode": "global"})
	if a.settings.Get().Mode != "global" {
		t.Error("mode")
	}
	must("set_mode", map[string]any{"mode": "rule"})
	if _, err := callTool(t, srv, s, "select_node", map[string]any{"route": "ai", "node": "nope"}); err == nil {
		t.Error("an unknown node should fail")
	}
	must("select_node", map[string]any{"route": "ai", "node": "auto"})
	if *asks != 0 {
		t.Errorf("allow asked %d times", *asks)
	}
}

func TestMCPCommand(t *testing.T) {
	if got := mcpClaudeCommand("/Applications/bowel.app/Contents/MacOS/bowel"); got != "claude mcp add --scope user bowel -- /Applications/bowel.app/Contents/MacOS/bowel mcp" {
		t.Errorf("command: %s", got)
	}
	if got := shellQuote("/Users/me/My Apps/bowel.app/x"); got != "'/Users/me/My Apps/bowel.app/x'" {
		t.Errorf("quoted: %s", got)
	}
	var cfg map[string]map[string]struct {
		Command string
		Args    []string
	}
	if err := json.Unmarshal([]byte(mcpJSON("/x/bowel")), &cfg); err != nil || cfg["mcpServers"]["bowel"].Command != "/x/bowel" || cfg["mcpServers"]["bowel"].Args[0] != "mcp" {
		t.Errorf("json: %v %v", cfg, err)
	}
	if p := mcpSocket("/short"); p != "/short/mcp.sock" {
		t.Errorf("socket: %s", p)
	}
	if p := mcpSocket("/" + strings.Repeat("x", 120)); len(p) > 100 {
		t.Errorf("a long folder should fall back: %s", p)
	}
}
