// Package routes is Moho's routing template: kinds of traffic and the rules
// that recognise them. Providers only supply nodes; every provider's nodes
// can serve every route (decision D1 of docs/product-ia.md).
package routes

import (
	"github.com/oboo/moho/internal/i18n"
	"github.com/oboo/moho/internal/mihomo"
)

// Route is a kind of traffic and where it may go.
type Route struct {
	ID      string `json:"id"`
	Name    string `json:"name"`
	Color   string `json:"color"`   // CSS custom property of its color
	Summary string `json:"summary"` // what it matches, for people
	// Rules are mihomo rules without their target, e.g. "DOMAIN-SUFFIX,openai.com".
	Rules []string `json:"-"`
	// Direct routes never use a node.
	Direct bool `json:"direct"`
	// Group is the core's group that serves the route; Auto its automatic
	// (fastest + failover) group, "" when it has none. Direct routes have
	// neither.
	Group string `json:"-"`
	Auto  string `json:"-"`
}

// Table is the routes in the order they are shown.
type Table []Route

// ByID is the route with that id, or the zero Route.
func (t Table) ByID(id string) Route {
	for _, r := range t {
		if r.ID == id {
			return r
		}
	}
	return Route{}
}

// OfChain is the id of the route that a connection's last step (a group,
// or DIRECT) belongs to, "" when none does. A connection that went direct
// belongs to the direct route only when a rule sent it there.
func (t Table) OfChain(group, rule string) string {
	if group == "" {
		return ""
	}
	for _, r := range t {
		if !r.Direct && (group == r.Group || group == r.Auto) {
			return r.ID
		}
	}
	if group == "DIRECT" && rule != "" && rule != "Match" {
		for _, r := range t {
			if r.Direct {
				return r.ID
			}
		}
	}
	return ""
}

// StepTitle is a chain's step as the interface shows it: a route's group by
// the route's title, anything else (a node, DIRECT) as it is.
func (t Table) StepTitle(name string) string {
	if name == "" {
		return name
	}
	for _, r := range t {
		switch name {
		case r.Group:
			return Title(r)
		case r.Auto:
			return Title(r) + i18n.T("·自动", " · Auto")
		}
	}
	return name
}

// Current is the node the route uses now, and whether its automatic group
// chose it. A group may pick another group, so it follows the choices to
// the node that carries the traffic; "" when the core has no such group.
func (r Route) Current(px map[string]mihomo.Proxy) (node string, auto bool) {
	g, ok := px[r.Group]
	if !ok {
		return "", false
	}
	node = g.Now
	for len(px[node].All) > 0 {
		node = px[node].Now
	}
	return node, r.Auto != "" && g.Now == r.Auto
}

// Default is the route that takes what no rule matched.
const Default = "default"

// Title is a route's name as the interface shows it. Name stays the
// mihomo group's, in Chinese, so that configs and choices keep across a
// change of language. Only the template's own names have an English one;
// a core's route keeps the name the core gave it.
func Title(r Route) string {
	if en, ok := titleEn[r.Name]; ok {
		return i18n.T(r.Name, en)
	}
	return r.Name
}

// Describe is a route's Summary as the interface shows it.
func Describe(r Route) string {
	if en, ok := summaryEn[r.Summary]; ok {
		return i18n.T(r.Summary, en)
	}
	return r.Summary
}

const defaultSummary = "没命中其他规则的流量"

// titleEn and summaryEn are keyed by the Chinese text they translate.
var titleEn = map[string]string{"默认": "Default", "AI 服务": "AI", "流媒体": "Streaming", "开发": "Dev", "直连": "Direct"}

var summaryEn = map[string]string{
	defaultSummary: "Traffic no other rule matched",
	"OpenAI · Claude · Gemini · Perplexity 等": "OpenAI · Claude · Gemini · Perplexity and more",
	"YouTube · Netflix · Spotify · Disney+ 等": "YouTube · Netflix · Spotify · Disney+ and more",
	"GitHub · npm · Docker · PyPI · Go 等":     "GitHub · npm · Docker · PyPI · Go and more",
	"国内网站 · 局域网":                              "Mainland China sites · LAN",
}

// Template returns the routes in the order their rules apply; the default
// route comes first for display, its rule (MATCH) last.
func Template() Table {
	t := Table{
		{ID: Default, Name: "默认", Color: "--r-default", Summary: defaultSummary},
		{ID: "ai", Name: "AI 服务", Color: "--r-ai", Summary: "OpenAI · Claude · Gemini · Perplexity 等", Rules: suffixes(
			"openai.com", "chatgpt.com", "oaistatic.com", "oaiusercontent.com", "sora.com",
			"anthropic.com", "claude.ai", "claude.com", "claudeusercontent.com",
			"gemini.google.com", "generativelanguage.googleapis.com", "aistudio.google.com", "notebooklm.google.com", "bard.google.com",
			"perplexity.ai", "x.ai", "grok.com", "poe.com", "cursor.com", "cursor.sh", "cursorapi.com",
			"mistral.ai", "huggingface.co", "openrouter.ai", "midjourney.com", "character.ai",
		)},
		{ID: "media", Name: "流媒体", Color: "--r-media", Summary: "YouTube · Netflix · Spotify · Disney+ 等", Rules: []string{
			"GEOSITE,youtube", "GEOSITE,netflix", "GEOSITE,spotify", "GEOSITE,disney", "GEOSITE,hbo",
			"GEOSITE,primevideo", "GEOSITE,twitch", "GEOSITE,tiktok", "GEOSITE,bahamut", "GEOSITE,abema",
		}},
		{ID: "dev", Name: "开发", Color: "--r-dev", Summary: "GitHub · npm · Docker · PyPI · Go 等", Rules: suffixes(
			"github.com", "githubusercontent.com", "githubassets.com", "github.io", "ghcr.io",
			"npmjs.org", "npmjs.com", "yarnpkg.com", "docker.io", "docker.com", "pypi.org", "pythonhosted.org",
			"golang.org", "go.dev", "proxy.golang.org", "crates.io", "rust-lang.org", "gitlab.com", "bitbucket.org",
			"stackoverflow.com", "jsdelivr.net", "unpkg.com", "vercel.com", "brew.sh",
		)},
		{ID: "direct", Name: "直连", Color: "--r-direct", Summary: "国内网站 · 局域网", Direct: true, Rules: []string{
			"GEOSITE,private", "GEOSITE,cn", "GEOIP,private,no-resolve", "GEOIP,CN,no-resolve",
		}},
	}
	for i, r := range t {
		if !r.Direct {
			t[i].Group, t[i].Auto = r.Name, r.Name+"·自动"
		}
	}
	return t
}

func suffixes(domains ...string) []string {
	out := make([]string, len(domains))
	for i, d := range domains {
		out[i] = "DOMAIN-SUFFIX," + d
	}
	return out
}

// Count is the number of rules, MATCH included.
func Count(rs []Route) int {
	n := 1
	for _, r := range rs {
		n += len(r.Rules)
	}
	return n
}
