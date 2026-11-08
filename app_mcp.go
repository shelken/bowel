package main

// MCP: agents (Claude Code, Codex, Cursor, …) read bowel's state and, as
// the user's policy says (settings' mcpPolicy: ask, allow or deny), change
// it. The app serves the tools on a unix socket in its data folder, for
// this user only; `bowel mcp` (main.go) bridges an agent's stdio to it.
//
// Reading is always allowed. A change is shown to the user first, as the
// settings it would change, when the policy is ask; the dialog can let the
// rest of that connection through. Every change an agent asks for is kept
// in a short list the 设置 > MCP page shows.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"slices"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/egoist/mygo"

	"github.com/oboo/moho/internal/core"
	"github.com/oboo/moho/internal/i18n"
	"github.com/oboo/moho/internal/mcp"
	"github.com/oboo/moho/internal/routes"
	"github.com/oboo/moho/internal/settings"
)

// mcpSocket is where the app serves MCP: in its data folder, or, when that
// path is too long for a unix socket, in the temporary folder.
func mcpSocket(dataDir string) string {
	p := filepath.Join(dataDir, "mcp.sock")
	if len(p) < 100 {
		return p
	}
	return filepath.Join(os.TempDir(), fmt.Sprintf("bowel-%d-mcp.sock", os.Getuid()))
}

// mcpErr is why the server is not serving, "" while it is.
var mcpErr atomic.Pointer[string]

// serveMCP serves the tools until ctx is done.
func (a *App) serveMCP(ctx context.Context) {
	l, err := mcp.Listen(mcpSocket(a.dataDir))
	if err != nil {
		log.Println("mcp:", err)
		e := err.Error()
		mcpErr.Store(&e)
		return
	}
	mcpErr.Store(new(string))
	a.mcpServer().Accept(ctx, l)
}

// mcpCommand is how an agent starts bowel's MCP server: this executable,
// with the argument "mcp". Screenshots replace it with the installed path.
var mcpCommand = func() string {
	exe, err := os.Executable()
	if err != nil {
		return "bowel"
	}
	if p, err := filepath.EvalSymlinks(exe); err == nil {
		exe = p
	}
	return exe
}

// runMCPBridge is `bowel mcp`: it carries the agent's stdio to the running
// app, opening the app first when it is not running.
func runMCPBridge() error {
	data, err := mygo.App.Path(mygo.PathUserData)
	if err != nil {
		return err
	}
	return mcp.Bridge(mcpSocket(data), os.Stdin, os.Stdout, startOwnApp)
}

// startOwnApp opens the .app this executable belongs to, in the
// background. Outside a bundle (go run, mygo dev) there is nothing to open.
func startOwnApp() error {
	exe, err := os.Executable()
	if err != nil {
		return err
	}
	exe, _ = filepath.EvalSymlinks(exe)
	bundle := filepath.Dir(filepath.Dir(filepath.Dir(exe))) // X.app/Contents/MacOS/x
	if !strings.HasSuffix(bundle, ".app") {
		return errors.New("not inside an .app; start bowel first")
	}
	return exec.Command("open", "-g", bundle).Run()
}

// ---- Approval ----

// agentChange is one change an agent asked for, as the MCP page lists it.
type agentChange struct {
	At     time.Time
	Client string
	What   words
	// Outcome: "applied", "denied", "failed".
	Outcome string
	Error   string
}

var agentLog struct {
	mu   sync.Mutex
	list []agentChange // newest first
}

func (a *App) noteAgentChange(ch agentChange) {
	agentLog.mu.Lock()
	agentLog.list = append([]agentChange{ch}, agentLog.list...)
	if len(agentLog.list) > 30 {
		agentLog.list = agentLog.list[:30]
	}
	agentLog.mu.Unlock()
	if u := a.uiRef(); u != nil {
		u.settingsRedraw()
	}
}

func agentChanges() []agentChange {
	agentLog.mu.Lock()
	defer agentLog.mu.Unlock()
	return slices.Clone(agentLog.list)
}

// words are a few words in both languages, put in the interface's at the
// time they are shown.
type words struct{ zh, en string }

func (w words) String() string { return i18n.T(w.zh, w.en) }

// askAgent asks the user whether client may make the change what (detail
// says more). remember lets the rest of the connection through. Tests
// replace it.
var askAgent = func(ctx context.Context, client, what, detail string) (ok, remember bool, err error) {
	askMu.Lock() // one dialog at a time
	defer askMu.Unlock()
	if err := ctx.Err(); err != nil {
		return false, false, err
	}
	type answer struct {
		r   mygo.MessageResult
		err error
	}
	ch := make(chan answer, 1)
	go func() {
		mygo.App.Focus() // from the menu bar, the dialog must come to the front
		r, err := mygo.Dialog.Message(mygo.MessageOptions{
			Type:          mygo.MessageQuestion,
			Message:       fmt.Sprintf(i18n.T("%s 想修改 bowel：%s", "%s wants to change bowel: %s"), client, what),
			Detail:        detail,
			Buttons:       []string{i18n.T("允许", "Allow"), i18n.T("拒绝", "Deny")},
			DefaultButton: 0, CancelButton: 1,
			CheckboxLabel: i18n.T("这次连接里不再询问", "Don't ask again for this connection"),
		})
		ch <- answer{r, err}
	}()
	select {
	case ans := <-ch:
		if ans.err != nil {
			return false, false, ans.err
		}
		return ans.r.Button == 0, ans.r.Button == 0 && ans.r.CheckboxChecked, nil
	case <-ctx.Done(): // the agent gave up, or left
		return false, false, ctx.Err()
	}
}

var askMu sync.Mutex

var errDenied = errors.New("denied")

// approve lets a change through as the policy says, asking when it is ask.
func (a *App) approve(ctx context.Context, s *mcp.Session, what words, detail string) error {
	switch a.settings.Get().Policy() {
	case "allow":
		return nil
	case "deny":
		a.noteAgentChange(agentChange{At: time.Now(), Client: s.Name(), What: what, Outcome: "denied"})
		return errors.New(i18n.T("bowel 设置为不允许 agent 做变更（设置 > MCP）。请让用户自己修改，或请用户改这项设置。",
			"bowel is set not to let agents make changes (Settings > MCP). Ask the user to make the change, or to change that setting."))
	}
	if s.Get("allowed") == true {
		return nil
	}
	ok, remember, err := askAgent(ctx, s.Name(), what.String(), detail)
	if err == nil && !ok {
		err = errDenied
	}
	if err != nil {
		a.noteAgentChange(agentChange{At: time.Now(), Client: s.Name(), What: what, Outcome: "denied"})
		if errors.Is(err, errDenied) {
			return errors.New(i18n.T("用户拒绝了这次变更。", "The user denied this change."))
		}
		return err
	}
	if remember {
		s.Set("allowed", true)
	}
	return nil
}

// change is an approved action: it asks, runs do, and notes the outcome.
func (a *App) change(ctx context.Context, s *mcp.Session, what words, detail string, do func() error) error {
	if err := a.approve(ctx, s, what, detail); err != nil {
		return err
	}
	err := do()
	ch := agentChange{At: time.Now(), Client: s.Name(), What: what, Outcome: "applied"}
	if err != nil {
		ch.Outcome, ch.Error = "failed", err.Error()
	}
	a.noteAgentChange(ch)
	return err
}

// changeSettings is a change of the saved settings by edit: it is checked,
// shown as what it changes, approved, then saved and applied. edit runs
// again on the settings as they are then. It returns what changed.
func (a *App) changeSettings(ctx context.Context, s *mcp.Session, what words, edit func(*settings.Settings) error) ([]string, error) {
	old := a.settings.Get()
	next := old.Clone()
	if err := edit(&next); err != nil {
		return nil, err
	}
	if err := next.Validate(); err != nil {
		return nil, err
	}
	diff := a.settingsDiff(old, next)
	if len(diff) == 0 {
		return nil, nil
	}
	err := a.change(ctx, s, what, strings.Join(diff, "\n"), func() error {
		var editErr error
		err := a.updateSettings(func(st *settings.Settings) { editErr = edit(st) })
		return errors.Join(editErr, err)
	})
	return diff, err
}

// secretKeys are the settings an agent reads only as set or not.
var secretKeys = []string{"extSecret", "coreSecret", "authentication"}

// settingsMap is s as its JSON keys, with the secrets hidden.
func settingsMap(s settings.Settings) map[string]any {
	b, _ := json.Marshal(s)
	m := map[string]any{}
	_ = json.Unmarshal(b, &m)
	for _, k := range secretKeys {
		switch v := m[k].(type) {
		case string:
			if v != "" {
				m[k] = "••••"
			}
		case []any:
			for i, e := range v {
				if str, ok := e.(string); ok {
					user, _, _ := strings.Cut(str, ":")
					v[i] = user + ":••••"
				}
			}
		}
	}
	if m["mcpPolicy"] == "" {
		m["mcpPolicy"] = "ask"
	}
	return m
}

// settingsDiff is what changes from old to next, a line each, for people.
func (a *App) settingsDiff(old, next settings.Settings) []string {
	om, nm := settingsMap(old), settingsMap(next)
	keys := make([]string, 0, len(nm))
	for k := range nm {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var out []string
	for _, k := range keys {
		if reflect.DeepEqual(om[k], nm[k]) {
			continue
		}
		if k == "rules" {
			out = append(out, a.rulesDiff(old.Rules, next.Rules)...)
			continue
		}
		ol, ok1 := om[k].([]any)
		nl, ok2 := nm[k].([]any)
		if ok1 || ok2 {
			before := len(out)
			for _, e := range nl {
				if !slices.ContainsFunc(ol, func(o any) bool { return reflect.DeepEqual(o, e) }) {
					out = append(out, fmt.Sprintf("%s: + %v", k, e))
				}
			}
			for _, e := range ol {
				if !slices.ContainsFunc(nl, func(n any) bool { return reflect.DeepEqual(n, e) }) {
					out = append(out, fmt.Sprintf("%s: − %v", k, e))
				}
			}
			if len(out) == before {
				out = append(out, k+": "+i18n.T("顺序调整", "reordered"))
			}
			continue
		}
		out = append(out, fmt.Sprintf("%s: %v → %v", k, show(om[k]), show(nm[k])))
	}
	return out
}

// show is a setting's value in a diff: a string as it is, "" as "".
func show(v any) string {
	if s, ok := v.(string); ok && s != "" {
		return s
	}
	b, _ := json.Marshal(v)
	return string(b)
}

func (a *App) rulesDiff(old, next []routes.UserRule) []string {
	var out []string
	for _, r := range next {
		i := slices.IndexFunc(old, r.Same)
		switch {
		case i < 0:
			out = append(out, fmt.Sprintf("rules: + %s → %s", r.Mihomo(), routes.Title(a.route(r.Route))))
		case old[i].Route != r.Route:
			out = append(out, fmt.Sprintf("rules: %s %s → %s", r.Mihomo(), routes.Title(a.route(old[i].Route)), routes.Title(a.route(r.Route))))
		}
	}
	for _, r := range old {
		if !slices.ContainsFunc(next, r.Same) {
			out = append(out, fmt.Sprintf("rules: − %s → %s", r.Mihomo(), routes.Title(a.route(r.Route))))
		}
	}
	return out
}

// ---- Lookups ----

// findRoute is the route s names: its id, its Chinese name or its title.
func (a *App) findRoute(s string) (routes.Route, error) {
	s = strings.TrimSpace(s)
	table := a.routeTable()
	for _, r := range table {
		if strings.EqualFold(s, r.ID) || s == r.Name || strings.EqualFold(s, routes.Title(r)) {
			return r, nil
		}
	}
	ids := make([]string, len(table))
	for i, r := range table {
		ids[i] = r.ID
	}
	return routes.Route{}, fmt.Errorf("%s: %q (%s)", i18n.T("没有这条路由", "No such route"), s, strings.Join(ids, ", "))
}

// findNode is the node s names among the choices of a route: its name in
// the core, or its label when one node has it. An external core's route
// offers its group's members, which may be a Direct outbound or a group;
// bowel's own offers every node.
func (a *App) findNode(routeID, s string) (string, error) {
	snap := a.snapshot()
	nodes := snap.Nodes
	if snap.External {
		for _, r := range snap.Routes {
			if r.ID == routeID {
				nodes = r.Members
			}
		}
	}
	for _, n := range nodes {
		if n.Name == s {
			return n.Name, nil
		}
	}
	var hits []string
	for _, n := range nodes {
		if strings.EqualFold(n.Display, s) || strings.EqualFold(display(n.Name), s) {
			hits = append(hits, n.Name)
		}
	}
	switch len(hits) {
	case 1:
		return hits[0], nil
	case 0:
		return "", fmt.Errorf(i18n.T("没有这个节点: %s", "No such node: %s"), s)
	}
	return "", fmt.Errorf(i18n.T("有多个节点叫 %s，请用节点的 name", "Several nodes are called %s; use the node's name"), s)
}

// ---- Views for agents ----

type mcpRoute struct {
	ID      string `json:"id"`
	Name    string `json:"name"`
	Summary string `json:"summary"`
	Direct  bool   `json:"direct"`
	// Auto: the route picks its fastest node by itself.
	Auto bool `json:"auto,omitempty"`
	// Node is the node it uses now (a node's name), "" for direct.
	Node      string `json:"node,omitempty"`
	NodeLabel string `json:"nodeLabel,omitempty"`
	UserRules int    `json:"userRules"`
	// Members are what an external core's route can use: the names of its
	// group's nodes, groups and Direct outbounds.
	Members []string `json:"members,omitempty"`
}

type mcpNode struct {
	Name     string `json:"name"`
	Label    string `json:"label"`
	Provider string `json:"provider"`
	Type     string `json:"type"`
	// DelayMs is the last test's; 0 failed. Absent when untested.
	DelayMs *int   `json:"delayMs,omitempty"`
	Where   string `json:"where,omitempty"`
}

func (a *App) mcpRoutes(snap Snapshot) []mcpRoute {
	out := make([]mcpRoute, 0, len(snap.Routes))
	for _, r := range snap.Routes {
		v := mcpRoute{ID: r.ID, Name: r.Name, Summary: r.Summary, Direct: r.Direct, Auto: r.Auto, Node: r.Node, UserRules: r.Mine}
		if r.Node != "" {
			v.NodeLabel = a.display(r.Node)
		}
		for _, m := range r.Members {
			v.Members = append(v.Members, m.Name)
		}
		out = append(out, v)
	}
	return out
}

func mcpNodes(snap Snapshot) []mcpNode {
	out := make([]mcpNode, 0, len(snap.Nodes))
	for _, n := range snap.Nodes {
		v := mcpNode{Name: n.Name, Label: n.Display, Provider: n.Provider, Type: n.Type}
		if n.Tested {
			d := n.Delay
			v.DelayMs = &d
		}
		if p := n.Where.Place; p != nil {
			v.Where = strings.TrimSpace(p.Country + " " + p.City + " (" + n.Where.Source + ")")
		}
		out = append(out, v)
	}
	return out
}

type mcpRule struct {
	Kind    string `json:"kind"`
	Value   string `json:"value"`
	Route   string `json:"route"`
	Mihomo  string `json:"mihomo"`
	Matches string `json:"matches"`
}

func mcpRules(rs []routes.UserRule) []mcpRule {
	out := make([]mcpRule, 0, len(rs))
	for _, r := range rs {
		out = append(out, mcpRule{Kind: r.Kind, Value: r.Value, Route: r.Route, Mihomo: r.Mihomo(), Matches: r.Describe()})
	}
	return out
}

// editable are the keys update_settings takes: all but what has a tool of
// its own, and the policy, which only the user sets.
func editable(s settings.Settings) []string {
	var keys []string
	for k := range settingsMap(s) {
		if !slices.Contains(notEditable, k) {
			keys = append(keys, k)
		}
	}
	sort.Strings(keys)
	return keys
}

var notEditable = []string{"mcpPolicy", "rules", "mode", "systemProxy", "tun", "paused"}

// parseTarget reads a host, URL, domain pattern or IP into a rule of kind
// ("" guesses) for route.
func parseTarget(kind, target, route string) (routes.UserRule, error) {
	return routes.ParseRule(kind, target, route)
}

// whitelists are the lists the whitelist tools edit.
var whitelists = []string{"direct", "bypass", "lan"}

func (a *App) whitelistView(s settings.Settings) map[string]any {
	var direct []mcpRule
	for _, r := range mcpRules(s.Rules) {
		if r.Route == "direct" {
			direct = append(direct, r)
		}
	}
	if direct == nil {
		direct = []mcpRule{}
	}
	return map[string]any{
		"direct": direct,
		"bypass": s.Bypass,
		"lan":    s.LanAllowedIPs,
		"about": map[string]string{
			"direct": "Your rules sending traffic to the direct route: it goes out without a proxy, through the core.",
			"bypass": "The system proxy's exceptions: apps that follow the system proxy don't send these to bowel at all.",
			"lan":    "Addresses on the LAN allowed to use bowel's proxy port when allowLan is on.",
		},
	}
}

// ---- The server ----

func obj(props map[string]any, required ...string) map[string]any {
	m := map[string]any{"type": "object", "properties": props, "additionalProperties": false}
	if len(required) > 0 {
		m["required"] = required
	}
	return m
}

func str(desc string, enum ...string) map[string]any {
	m := map[string]any{"type": "string", "description": desc}
	if len(enum) > 0 {
		m["enum"] = enum
	}
	return m
}

// routeArg is the schema of a tool's route argument, with the routes as
// they are now: an external core's are found after the server is built.
func (a *App) routeArg() map[string]any {
	table := a.routeTable()
	routeIDs := make([]string, len(table))
	for i, r := range table {
		routeIDs[i] = r.ID
	}
	if len(routeIDs) == 0 {
		return str("A route's id, as list_routes gives it.")
	}
	return str("A route's id: "+strings.Join(routeIDs, ", ")+".", routeIDs...)
}

func (a *App) mcpServer() *mcp.Server {
	return &mcp.Server{
		Name: "bowel", Title: "bowel", Version: mygo.App.Version(),
		Instructions: mcpInstructions,
		Tools: []mcp.Tool{
			{
				Name: "get_status", Title: "Status", ReadOnly: true,
				Description: "bowel's state now: the one light (ok, degraded, error, off) and its issue, the core, the mode, the takeover (system proxy, TUN, port, LAN), each route's node, and today's traffic.",
				Run: func(ctx context.Context, s *mcp.Session, _ json.RawMessage) (any, error) {
					snap, t := a.snapshot(), a.tick()
					return map[string]any{
						"state": snap.State, "issue": snap.Issue, "core": snap.Core, "mode": snap.Mode,
						"takeover": snap.Takeover, "routes": a.mcpRoutes(snap), "nodes": len(snap.Nodes),
						"providers": len(snap.Providers), "activeConnections": t.Active,
						"traffic":     map[string]int64{"downBps": t.Down, "upBps": t.Up, "todayDown": t.TodayDown, "todayUp": t.TodayUp, "monthDown": t.MonthDown},
						"agentPolicy": a.settings.Get().Policy(), "version": snap.Version,
					}, nil
				},
			},
			{
				Name: "get_settings", Title: "Settings", ReadOnly: true,
				Description: "All saved settings as settings.json has them (secrets hidden), the keys update_settings can change, and the choices of the keys that take one of a few values.",
				Run: func(ctx context.Context, s *mcp.Session, _ json.RawMessage) (any, error) {
					st := a.settings.Get()
					return map[string]any{"settings": settingsMap(st), "editable": editable(st), "choices": settingChoices()}, nil
				},
			},
			{
				Name: "list_routes", Title: "Routes and nodes", ReadOnly: true,
				Description: "The routes (kinds of traffic: default, ai, media, dev, direct) with the node each uses, and every node with its last delay and location. Every node can serve every proxied route.",
				Run: func(ctx context.Context, s *mcp.Session, _ json.RawMessage) (any, error) {
					snap := a.snapshot()
					return map[string]any{"routes": a.mcpRoutes(snap), "nodes": mcpNodes(snap)}, nil
				},
			},
			{
				Name: "list_rules", Title: "Your rules", ReadOnly: true,
				Description: "The user's own rules, in the order they apply (before the built-in ones): what each matches and its route.",
				Run: func(ctx context.Context, s *mcp.Session, _ json.RawMessage) (any, error) {
					if a.core.External() {
						return map[string]any{"rules": []mcpRule{}}, nil // its rules are its own
					}
					return map[string]any{"rules": mcpRules(a.settings.Get().Rules)}, nil
				},
			},
			{
				Name: "explain", Title: "Where would it go", ReadOnly: true,
				Description: "Where a connection to a host, URL or IP would go, and which rule decides. When GeoSite/GeoIP data or the default route decides, sure is false.",
				Input:       obj(map[string]any{"target": str("A host, URL or IP, as api.openai.com.")}, "target"),
				Run: func(ctx context.Context, s *mcp.Session, raw json.RawMessage) (any, error) {
					var p struct {
						Target string `json:"target"`
					}
					if err := mcp.Args(raw, &p); err != nil {
						return nil, err
					}
					host := p.Target
					if r, err := routes.ParseRule(routes.KindDomain, host, ""); err == nil {
						host = r.Value
					} else if r, err := routes.ParseRule(routes.KindIP, host, ""); err == nil {
						host, _, _ = strings.Cut(r.Value, "/")
					} else {
						return nil, err
					}
					if a.core.External() {
						return map[string]any{"host": host, "site": routes.Site(host), "sure": false, "userRule": false,
							"note": "The external core decides by its own rules; bowel can't tell which one applies. list_connections shows the rule that matched a real connection."}, nil
					}
					id, rule, mine, sure := routes.Explain(host, a.settings.Get().Rules, a.routeTable())
					out := map[string]any{"host": host, "site": routes.Site(host), "sure": sure, "userRule": mine}
					if sure {
						out["route"], out["rule"] = id, rule
					} else {
						out["note"] = "No user rule or built-in domain rule matches; GeoSite/GeoIP data decides, else the default route."
					}
					return out, nil
				},
			},
			{
				Name: "list_connections", Title: "Connections", ReadOnly: true,
				Description: "Recent connections, newest first: app, host, route, node, rule and traffic. Filter by a part of the host or app.",
				Input: obj(map[string]any{
					"filter": str("Only connections whose host or app contains this."),
					"status": str("Only these: active, closed or failed.", "active", "closed", "failed"),
					"limit":  map[string]any{"type": "integer", "description": "At most this many (default 50, at most 500).", "minimum": 1, "maximum": 500},
				}),
				Run: func(ctx context.Context, s *mcp.Session, raw json.RawMessage) (any, error) {
					var p struct {
						Filter, Status string
						Limit          int
					}
					if err := mcp.Args(raw, &p); err != nil {
						return nil, err
					}
					if p.Limit <= 0 || p.Limit > 500 {
						p.Limit = 50
					}
					all := a.connections()
					sort.SliceStable(all, func(i, j int) bool { return all[i].Start.After(all[j].Start) })
					f := strings.ToLower(p.Filter)
					out := []Conn{}
					for _, c := range all {
						if p.Status != "" && c.Status != p.Status {
							continue
						}
						if f != "" && !strings.Contains(strings.ToLower(c.Host), f) && !strings.Contains(strings.ToLower(c.App), f) && !strings.Contains(c.IP, f) {
							continue
						}
						out = append(out, c)
						if len(out) == p.Limit {
							break
						}
					}
					return map[string]any{"connections": out, "total": len(all)}, nil
				},
			},
			{
				Name: "list_providers", Title: "Providers", ReadOnly: true,
				Description: "The providers that supply nodes (subscriptions, links, files): their nodes, data used and expiry. Subscription URLs are masked.",
				Run: func(ctx context.Context, s *mcp.Session, _ json.RawMessage) (any, error) {
					return map[string]any{"providers": a.snapshot().Providers}, nil
				},
			},
			{
				Name: "get_logs", Title: "Core logs", ReadOnly: true,
				Description: "The core's recent log lines, newest last.",
				Input: obj(map[string]any{
					"level": str("Only this level and worse.", "debug", "info", "warning", "error"),
					"limit": map[string]any{"type": "integer", "description": "At most this many (default 100, at most 1000).", "minimum": 1, "maximum": 1000},
				}),
				Run: func(ctx context.Context, s *mcp.Session, raw json.RawMessage) (any, error) {
					var p struct {
						Level string
						Limit int
					}
					if err := mcp.Args(raw, &p); err != nil {
						return nil, err
					}
					if p.Limit <= 0 || p.Limit > 1000 {
						p.Limit = 100
					}
					rank := map[string]int{"debug": 0, "info": 1, "warning": 2, "error": 3}
					var out []LogLine
					for _, l := range a.logs(0) {
						if p.Level == "" || rank[l.Level] >= rank[p.Level] {
							out = append(out, l)
						}
					}
					if len(out) > p.Limit {
						out = out[len(out)-p.Limit:]
					}
					if out == nil {
						out = []LogLine{}
					}
					return map[string]any{"logs": out}, nil
				},
			},
			{
				Name: "get_whitelists", Title: "Whitelists", ReadOnly: true,
				Description: "The three whitelists: direct (sites sent straight out, without a proxy), bypass (the system proxy's exceptions) and lan (LAN addresses allowed to use the proxy).",
				Run: func(ctx context.Context, s *mcp.Session, _ json.RawMessage) (any, error) {
					return a.whitelistView(a.settings.Get()), nil
				},
			},
			{
				Name: "test_route", Title: "Test a route's nodes",
				Description: "Measures the delay of a route's nodes now, and returns them. Changes nothing.",
				Schema:      func() map[string]any { return obj(map[string]any{"route": a.routeArg()}, "route") },
				Run: func(ctx context.Context, s *mcp.Session, raw json.RawMessage) (any, error) {
					var p struct{ Route string }
					if err := mcp.Args(raw, &p); err != nil {
						return nil, err
					}
					r, err := a.findRoute(p.Route)
					if err != nil {
						return nil, err
					}
					if r.Direct {
						return nil, errors.New("the direct route uses no node")
					}
					err = a.testRoute(ctx, r.ID)
					snap := a.snapshot()
					out := map[string]any{"nodes": mcpNodes(snap)}
					if err != nil {
						out["error"] = err.Error()
					}
					return out, nil
				},
			},

			// ---- Changes: as the policy says ----
			{
				Name: "set_mode", Title: "Set the mode",
				Description: "Changes the proxy mode: rule (routes by rules), global (everything through the default route's node) or direct (no proxy). Open connections are closed so they reconnect the new way.",
				Input:       obj(map[string]any{"mode": str("The mode.", "rule", "global", "direct")}, "mode"),
				Run: func(ctx context.Context, s *mcp.Session, raw json.RawMessage) (any, error) {
					var p struct{ Mode string }
					if err := mcp.Args(raw, &p); err != nil {
						return nil, err
					}
					old := a.settings.Get().Mode
					if p.Mode == old {
						return "The mode is " + old + " already.", nil
					}
					err := a.change(ctx, s, words{"切换代理模式", "Switch the proxy mode"}, "mode: "+old+" → "+p.Mode, func() error { return a.setMode(p.Mode) })
					if err != nil {
						return nil, err
					}
					return "The mode is now " + p.Mode + ".", nil
				},
			},
			{
				Name: "set_takeover", Title: "Set the takeover", Destructive: true,
				Description: "Turns bowel's takeover of this Mac's traffic on or off, and chooses its entries: the system proxy and TUN (all traffic; the first time macOS asks the user for an administrator's password). Give only what should change.",
				Input: obj(map[string]any{
					"enabled":     map[string]any{"type": "boolean", "description": "Take over traffic at all (false pauses it, keeping the entries)."},
					"systemProxy": map[string]any{"type": "boolean", "description": "Use the system proxy."},
					"tun":         map[string]any{"type": "boolean", "description": "Use TUN."},
				}),
				Run: func(ctx context.Context, s *mcp.Session, raw json.RawMessage) (any, error) {
					var p struct {
						Enabled, SystemProxy, Tun *bool
					}
					if err := mcp.Args(raw, &p); err != nil {
						return nil, err
					}
					st := a.settings.Get()
					var lines []string
					note := func(k string, from, to bool) {
						if from != to {
							lines = append(lines, fmt.Sprintf("%s: %v → %v", k, from, to))
						}
					}
					if p.SystemProxy != nil {
						note("systemProxy", st.SystemProxy, *p.SystemProxy)
					}
					if p.Tun != nil {
						note("tun", st.Tun, *p.Tun)
					}
					if p.Enabled != nil {
						note("enabled", a.takeoverOn(st), *p.Enabled)
					}
					if len(lines) == 0 {
						return "Nothing to change.", nil
					}
					err := a.change(ctx, s, words{"更改接管", "Change the takeover"}, strings.Join(lines, "\n"), func() error {
						if p.SystemProxy != nil && *p.SystemProxy != st.SystemProxy {
							if err := a.setSystemProxy(*p.SystemProxy); err != nil {
								return err
							}
						}
						if p.Tun != nil && *p.Tun != st.Tun {
							if err := a.setTun(*p.Tun); err != nil {
								return err
							}
						}
						if p.Enabled != nil && *p.Enabled != a.takeoverOn(a.settings.Get()) {
							return a.setTakeover(*p.Enabled)
						}
						return nil
					})
					if err != nil {
						return nil, err
					}
					return map[string]any{"changed": lines, "takeover": a.snapshot().Takeover}, nil
				},
			},
			{
				Name: "select_node", Title: "Choose a route's node",
				Description: "Makes a proxied route use a node, or pick the fastest by itself (node \"auto\"). Nodes are named as list_routes gives them.",
				Schema: func() map[string]any {
					return obj(map[string]any{"route": a.routeArg(), "node": str("A node's name or label, or \"auto\".")}, "route", "node")
				},
				Run: func(ctx context.Context, s *mcp.Session, raw json.RawMessage) (any, error) {
					var p struct{ Route, Node string }
					if err := mcp.Args(raw, &p); err != nil {
						return nil, err
					}
					r, err := a.findRoute(p.Route)
					if err != nil {
						return nil, err
					}
					if r.Direct {
						return nil, errors.New("the direct route uses no node")
					}
					node, label := "", i18n.T("自动", "Auto")
					if a.core.External() && (p.Node == "" || strings.EqualFold(p.Node, "auto")) && r.Auto == "" {
						return nil, errors.New(i18n.T("这条路由没有自动选择的分组，请指定一个节点", "This route has no automatic group; name one of its members"))
					}
					if !strings.EqualFold(p.Node, "auto") && p.Node != "" {
						if node, err = a.findNode(r.ID, p.Node); err != nil {
							return nil, err
						}
						label = a.display(node)
					}
					cur := i18n.T("自动", "Auto")
					for _, rv := range a.snapshot().Routes {
						if rv.ID == r.ID && !rv.Auto && rv.Node != "" {
							cur = a.display(rv.Node)
						}
					}
					err = a.change(ctx, s, words{"为路由选择节点", "Choose a route's node"},
						fmt.Sprintf("%s: %s → %s", routes.Title(r), cur, label), func() error { return a.selectNode(r.ID, node) })
					if err != nil {
						return nil, err
					}
					return fmt.Sprintf("%s now uses %s.", r.ID, label), nil
				},
			},
			{
				Name: "add_rule", Title: "Add a rule",
				Description: "Sends traffic to a route with a rule of the user's, which wins over the built-in ones. It replaces a rule for the same traffic. Open connections it now sends elsewhere are closed, so the change shows at once. kind: suffix (the domain and its subdomains; the default for names), domain (that name only), keyword (names containing it), ip (an address or CIDR; the default for addresses).",
				Schema: func() map[string]any {
					return obj(map[string]any{
						"target": str("A domain, *.domain, URL, keyword or IP/CIDR."),
						"route":  a.routeArg(),
						"kind":   str("The kind of rule; guessed when left out.", routes.Kinds...),
					}, "target", "route")
				},
				Run: func(ctx context.Context, s *mcp.Session, raw json.RawMessage) (any, error) {
					var p struct{ Target, Route, Kind string }
					if err := mcp.Args(raw, &p); err != nil {
						return nil, err
					}
					rt, err := a.findRoute(p.Route)
					if err != nil {
						return nil, err
					}
					r, err := parseTarget(p.Kind, p.Target, rt.ID)
					if err != nil {
						return nil, err
					}
					diff, err := a.changeSettings(ctx, s, words{"添加规则", "Add a rule"}, func(st *settings.Settings) error {
						st.Rules = routes.Upsert(st.Rules, r)
						return nil
					})
					if err != nil {
						return nil, err
					}
					if diff == nil {
						return "That rule is there already.", nil
					}
					return map[string]any{"rule": mcpRules([]routes.UserRule{r})[0], "closedConnections": a.reroute(r, r.Route)}, nil
				},
			},
			{
				Name: "remove_rule", Title: "Remove a rule", Destructive: true,
				Description: "Takes out the user's rule for some traffic, as list_rules shows it (kind and value, or the target as add_rule took it).",
				Input: obj(map[string]any{
					"target": str("The rule's value, or what was given to add_rule."),
					"kind":   str("The rule's kind; guessed when left out.", routes.Kinds...),
				}, "target"),
				Run: func(ctx context.Context, s *mcp.Session, raw json.RawMessage) (any, error) {
					var p struct{ Target, Kind string }
					if err := mcp.Args(raw, &p); err != nil {
						return nil, err
					}
					r, err := a.ruleFor(p.Kind, p.Target)
					if err != nil {
						return nil, err
					}
					_, err = a.changeSettings(ctx, s, words{"删除规则", "Remove a rule"}, func(st *settings.Settings) error {
						st.Rules = routes.Remove(st.Rules, r)
						return nil
					})
					if err != nil {
						return nil, err
					}
					return map[string]any{"removed": mcpRules([]routes.UserRule{r})[0], "closedConnections": a.reroute(r, "")}, nil
				},
			},
			{
				Name: "whitelist_add", Title: "Add to a whitelist",
				Description: "Adds entries to a whitelist. direct: sites (domain, *.domain, URL or IP/CIDR) that go out without a proxy, as rules to the direct route. bypass: the system proxy's exceptions (as *.example.com, 192.168.0.0/16). lan: LAN addresses or CIDRs allowed to use the proxy port.",
				Input: obj(map[string]any{
					"list":    str("Which whitelist.", whitelists...),
					"entries": map[string]any{"type": "array", "items": map[string]any{"type": "string"}, "minItems": 1, "description": "What to add."},
				}, "list", "entries"),
				Run: func(ctx context.Context, s *mcp.Session, raw json.RawMessage) (any, error) {
					return a.whitelistEdit(ctx, s, raw, true)
				},
			},
			{
				Name: "whitelist_remove", Title: "Remove from a whitelist", Destructive: true,
				Description: "Takes entries out of a whitelist (direct, bypass or lan), as get_whitelists shows them.",
				Input: obj(map[string]any{
					"list":    str("Which whitelist.", whitelists...),
					"entries": map[string]any{"type": "array", "items": map[string]any{"type": "string"}, "minItems": 1, "description": "What to remove."},
				}, "list", "entries"),
				Run: func(ctx context.Context, s *mcp.Session, raw json.RawMessage) (any, error) {
					return a.whitelistEdit(ctx, s, raw, false)
				},
			},
			{
				Name: "update_settings", Title: "Change settings", Destructive: true,
				Description: "Changes saved settings, by their keys as get_settings gives them (as {\"logLevel\": \"debug\", \"ipv6\": false}). A list replaces the whole list. The change is checked, then the core reloads; when the core refuses it, nothing is kept. Not here: mode (set_mode), takeover (set_takeover), rules (add_rule, remove_rule) and the agent policy (only the user sets it).",
				Input: obj(map[string]any{
					"changes": map[string]any{"type": "object", "description": "Keys and their new values.", "minProperties": 1},
				}, "changes"),
				Run: func(ctx context.Context, s *mcp.Session, raw json.RawMessage) (any, error) {
					var p struct {
						Changes map[string]json.RawMessage `json:"changes"`
					}
					if err := mcp.Args(raw, &p); err != nil {
						return nil, err
					}
					patch, err := a.settingsPatch(p.Changes)
					if err != nil {
						return nil, err
					}
					diff, err := a.changeSettings(ctx, s, words{"修改设置", "Change settings"}, func(st *settings.Settings) error {
						return json.Unmarshal(patch, st)
					})
					if err != nil {
						return nil, err
					}
					if diff == nil {
						return "Nothing to change: the settings have these values already.", nil
					}
					return map[string]any{"changed": diff}, nil
				},
			},
			{
				Name: "update_providers", Title: "Update providers",
				Description: "Downloads subscriptions again: one provider by id, or every enabled subscription.",
				Input:       obj(map[string]any{"id": str("A provider's id from list_providers; all when left out.")}),
				Run: func(ctx context.Context, s *mcp.Session, raw json.RawMessage) (any, error) {
					var p struct{ ID string }
					if err := mcp.Args(raw, &p); err != nil {
						return nil, err
					}
					what := "all"
					if p.ID != "" {
						pv, ok := a.providers.Get(p.ID)
						if !ok {
							return nil, fmt.Errorf("no provider %q", p.ID)
						}
						what = pv.Name
					}
					err := a.change(ctx, s, words{"更新 Provider", "Update Providers"}, what, func() error {
						if p.ID != "" {
							return a.updateProvider(ctx, p.ID)
						}
						return a.updateAll(ctx)
					})
					if err != nil {
						return nil, err
					}
					return map[string]any{"providers": a.snapshot().Providers}, nil
				},
			},
			{
				Name: "flush_dns", Title: "Flush DNS",
				Description: "Clears the core's DNS cache and its fake-ip mappings.",
				Run: func(ctx context.Context, s *mcp.Session, _ json.RawMessage) (any, error) {
					if err := a.change(ctx, s, words{"清空 DNS 缓存", "Flush the DNS cache"}, "DNS", a.flushDNS); err != nil {
						return nil, err
					}
					return "Flushed.", nil
				},
			},
		},
	}
}

// ruleFor is the user's rule a target names: one with that value (and
// kind, when given), else the rule add_rule would have made of it.
func (a *App) ruleFor(kind, target string) (routes.UserRule, error) {
	rules := a.settings.Get().Rules
	for _, r := range rules {
		if r.Value == strings.TrimSpace(strings.ToLower(target)) && (kind == "" || kind == r.Kind) {
			return r, nil
		}
	}
	r, err := parseTarget(kind, target, "")
	if err != nil {
		return r, err
	}
	if i := slices.IndexFunc(rules, r.Same); i >= 0 {
		return rules[i], nil
	}
	return r, fmt.Errorf("no rule for %s (%s)", r.Value, r.Kind)
}

// settingsPatch checks an agent's changes and gives them as JSON to decode
// onto the settings.
func (a *App) settingsPatch(changes map[string]json.RawMessage) ([]byte, error) {
	if len(changes) == 0 {
		return nil, errors.New("no changes given")
	}
	st := a.settings.Get()
	ok := editable(st)
	for k, v := range changes {
		switch {
		case slices.Contains(notEditable, k):
			return nil, fmt.Errorf("%s can't be changed here (%s)", k, map[string]string{
				"mcpPolicy": "only the user sets it, in Settings > MCP", "rules": "use add_rule and remove_rule",
				"mode": "use set_mode", "systemProxy": "use set_takeover", "tun": "use set_takeover", "paused": "use set_takeover",
			}[k])
		case !slices.Contains(ok, k):
			return nil, fmt.Errorf("unknown setting %q; get_settings lists them", k)
		case slices.Contains(secretKeys, k) && strings.Contains(string(v), "••••"):
			return nil, fmt.Errorf("%s: give the real value, not the hidden one", k)
		}
	}
	b, err := json.Marshal(changes)
	if err != nil {
		return nil, err
	}
	// The types must fit before anything is shown to the user.
	if err := json.Unmarshal(b, &st); err != nil {
		return nil, fmt.Errorf("bad value: %w", err)
	}
	return b, nil
}

// settingChoices are the values of the settings that take one of a few.
func settingChoices() map[string][]string {
	return map[string][]string{
		"language": i18n.Languages, "appearance": settings.Appearances, "borders": settings.BorderStyles, "skin": settings.Skins,
		"logLevel": settings.LogLevels, "mode": {"rule", "global", "direct"}, "mcpPolicy": settings.MCPPolicies,
		"dnsMode": core.DNSModes, "tunStack": core.TunStacks, "findProcess": core.FindProcess, "fingerprint": core.Fingerprints,
	}
}

func (a *App) whitelistEdit(ctx context.Context, s *mcp.Session, raw json.RawMessage, add bool) (any, error) {
	var p struct {
		List    string   `json:"list"`
		Entries []string `json:"entries"`
	}
	if err := mcp.Args(raw, &p); err != nil {
		return nil, err
	}
	if len(p.Entries) == 0 {
		return nil, errors.New("no entries given")
	}
	what := words{"加入白名单", "Add to a whitelist"}
	if !add {
		what = words{"移出白名单", "Remove from a whitelist"}
	}
	var edit func(*settings.Settings) error
	var rules []routes.UserRule
	switch p.List {
	case "direct":
		for _, e := range p.Entries {
			var r routes.UserRule
			var err error
			if add {
				r, err = parseTarget("", e, "direct")
			} else {
				r, err = a.ruleFor("", e)
				if err == nil && r.Route != "direct" {
					err = fmt.Errorf("%s goes to %s, not direct; remove_rule takes it out", r.Value, r.Route)
				}
			}
			if err != nil {
				return nil, err
			}
			rules = append(rules, r)
		}
		edit = func(st *settings.Settings) error {
			for _, r := range rules {
				if add {
					st.Rules = routes.Upsert(st.Rules, r)
				} else {
					st.Rules = routes.Remove(st.Rules, r)
				}
			}
			return nil
		}
	case "bypass", "lan":
		var entries []string
		for _, e := range p.Entries {
			if e = strings.TrimSpace(e); e != "" {
				entries = append(entries, e)
			}
		}
		list := func(st *settings.Settings) *[]string {
			if p.List == "bypass" {
				return &st.Bypass
			}
			return &st.LanAllowedIPs
		}
		if !add {
			st := a.settings.Get()
			cur := *list(&st)
			for _, e := range entries {
				if !slices.Contains(cur, e) {
					return nil, fmt.Errorf("%s is not in %s", e, p.List)
				}
			}
		}
		edit = func(st *settings.Settings) error {
			l := list(st)
			for _, e := range entries {
				if add && !slices.Contains(*l, e) {
					*l = append(*l, e)
				} else if !add {
					*l = slices.DeleteFunc(*l, func(x string) bool { return x == e })
				}
			}
			return nil
		}
	default:
		return nil, fmt.Errorf("unknown whitelist %q (%s)", p.List, strings.Join(whitelists, ", "))
	}
	diff, err := a.changeSettings(ctx, s, what, edit)
	if err != nil {
		return nil, err
	}
	out := a.whitelistView(a.settings.Get())
	if diff == nil {
		out["note"] = "Nothing to change."
	} else {
		out["changed"] = diff
		closed := 0
		for _, r := range rules {
			keep := ""
			if add {
				keep = r.Route
			}
			closed += a.reroute(r, keep)
		}
		if p.List == "direct" {
			out["closedConnections"] = closed
		}
	}
	return out, nil
}

const mcpInstructions = `bowel is the user's macOS client for the mihomo proxy core. Providers supply nodes; routes are kinds of traffic (default, ai, media, dev, direct), each proxied route using one node or picking the fastest itself ("auto"). The user's own rules come before the built-in ones.

Read freely: get_status first, then get_settings, list_routes, list_rules, explain, list_connections, get_logs, get_whitelists.

Changes (set_mode, set_takeover, select_node, add_rule, remove_rule, whitelist_add/remove, update_settings, update_providers, flush_dns) follow the user's policy: by default bowel asks the user in a dialog, which may take a while; they may also be always allowed or always refused. A refusal is the user's decision: don't retry it, tell the user. Prefer the narrowest change: to send a site somewhere, add_rule; to let a site skip the proxy, whitelist_add to direct.`

var mcpStop context.CancelFunc

// startMCP serves the tools for the app's life; stopMCP ends that, taking
// the socket away.
func (a *App) startMCP() {
	ctx, cancel := context.WithCancel(context.Background())
	mcpStop = cancel
	go a.serveMCP(ctx)
}

func stopMCP() {
	if mcpStop != nil {
		mcpStop()
	}
}
