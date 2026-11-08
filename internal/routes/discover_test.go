package routes

import (
	"fmt"
	"testing"

	"github.com/oboo/moho/internal/mihomo"
)

// summary is a route reduced to what Discover decides.
func summary(r Route) string {
	return fmt.Sprintf("%s group=%q auto=%q direct=%v rules=%d %s", r.ID, r.Group, r.Auto, r.Direct, len(r.Rules), r.Color)
}

func rule(target, payload string) mihomo.Rule {
	return mihomo.Rule{Type: "default", Payload: payload, Proxy: target}
}

func TestDiscover(t *testing.T) {
	px := map[string]mihomo.Proxy{
		"GLOBAL":  {Type: "Fallback", Now: "main", All: []string{"main", "dns"}},
		"main":    group("work", "work", "n1", "n2"),
		"work":    group("n1", "n1"),
		"video":   group("n2", "n1", "n2"),
		"chat":    group("n1", "n1"),
		"games":   group("n1", "n1"),
		"fastest": {Type: "URLTest", Now: "n1", All: []string{"n1", "n2"}},
		"direct":  {Type: "Direct"},
		"n1":      {Type: "Shadowsocks"},
		"n2":      {Type: "Hysteria2"},
	}
	for _, c := range []struct {
		name  string
		px    map[string]mihomo.Proxy
		rules []mihomo.Rule
		want  []string
	}{
		{
			name: "selectors the rules route to, the default first, then in order of first rule",
			px:   px,
			rules: []mihomo.Rule{
				rule("sniff(inbound)", ""), rule("hijack-dns", "port=53"),
				rule("route(video)", "domain=a"), rule("route(chat)", "domain=b"),
				rule("route(video)", "domain=c"), rule("reject", "port=80"),
			},
			want: []string{
				`default group="main" auto="" direct=false rules=0 --r-default`,
				`video group="video" auto="" direct=false rules=2 --r-ai`,
				`chat group="chat" auto="" direct=false rules=1 --r-media`,
			},
		},
		{
			name:  "a rule that targets the Direct outbound makes the direct route",
			px:    px,
			rules: []mihomo.Rule{rule("route(video)", "a"), rule("route(direct)", "b"), rule("route(direct)", "c")},
			want: []string{
				`default group="main" auto="" direct=false rules=0 --r-default`,
				`video group="video" auto="" direct=false rules=1 --r-ai`,
				`direct group="" auto="" direct=true rules=2 --r-direct`,
			},
		},
		{
			name:  "no rule sends traffic direct: no direct route",
			px:    px,
			rules: []mihomo.Rule{rule("route(video)", "a")},
			want: []string{
				`default group="main" auto="" direct=false rules=0 --r-default`,
				`video group="video" auto="" direct=false rules=1 --r-ai`,
			},
		},
		{
			name:  "the default group is not listed twice when rules also target it",
			px:    px,
			rules: []mihomo.Rule{rule("route(main)", "a"), rule("route(video)", "b")},
			want: []string{
				`default group="main" auto="" direct=false rules=1 --r-default`,
				`video group="video" auto="" direct=false rules=1 --r-ai`,
			},
		},
		{
			name:  "URLTest, nodes and names the core does not have are no routes",
			px:    px,
			rules: []mihomo.Rule{rule("route(fastest)", "a"), rule("route(n1)", "b"), rule("route(gone)", "c")},
			want:  []string{`default group="main" auto="" direct=false rules=0 --r-default`},
		},
		{
			name:  "colors cycle through the route palette",
			px:    px,
			rules: []mihomo.Rule{rule("route(video)", "a"), rule("route(chat)", "b"), rule("route(games)", "c"), rule("route(work)", "d")},
			want: []string{
				`default group="main" auto="" direct=false rules=0 --r-default`,
				`video group="video" auto="" direct=false rules=1 --r-ai`,
				`chat group="chat" auto="" direct=false rules=1 --r-media`,
				`games group="games" auto="" direct=false rules=1 --r-dev`,
				`work group="work" auto="" direct=false rules=1 --r-ai`,
			},
		},
		{
			name:  "a core without GLOBAL has no default route",
			px:    map[string]mihomo.Proxy{"video": px["video"], "n1": px["n1"]},
			rules: []mihomo.Rule{rule("route(video)", "a")},
			want:  []string{`video group="video" auto="" direct=false rules=1 --r-ai`},
		},
		{
			name: "a target named without route() is a group's name, as mihomo reports it",
			px:   px,
			rules: []mihomo.Rule{
				{Type: "DomainSuffix", Payload: "a.example", Proxy: "video"},
			},
			want: []string{
				`default group="main" auto="" direct=false rules=0 --r-default`,
				`video group="video" auto="" direct=false rules=1 --r-ai`,
			},
		},
	} {
		t.Run(c.name, func(t *testing.T) {
			var got []string
			for _, r := range Discover(c.px, c.rules) {
				got = append(got, summary(r))
			}
			if fmt.Sprint(got) != fmt.Sprint(c.want) {
				t.Errorf("got\n  %s\nwant\n  %s", join(got), join(c.want))
			}
		})
	}
}

func join(ss []string) string {
	out := ""
	for i, s := range ss {
		if i > 0 {
			out += "\n  "
		}
		out += s
	}
	return out
}
