package routes

import (
	"testing"

	"github.com/oboo/moho/internal/i18n"
	"github.com/oboo/moho/internal/mihomo"
)

func TestTemplateRoutesNameTheirGroups(t *testing.T) {
	for _, r := range Template() {
		if r.Direct {
			if r.Group != "" || r.Auto != "" {
				t.Errorf("%s is direct but has groups %q, %q", r.ID, r.Group, r.Auto)
			}
			continue
		}
		if r.Group != r.Name || r.Auto != r.Name+"·自动" {
			t.Errorf("%s: groups %q, %q", r.ID, r.Group, r.Auto)
		}
	}
}

func group(now string, all ...string) mihomo.Proxy {
	return mihomo.Proxy{Type: "Selector", Now: now, All: all}
}

// Current is the node at the end of the groups' choices: a route may pick
// its automatic group, or a group of another route, and what people want
// to know is the node that carries the traffic.
func TestCurrentFollowsGroupsToTheNode(t *testing.T) {
	px := map[string]mihomo.Proxy{
		"ai":      group("ai-auto", "ai-auto", "n1"),
		"ai-auto": group("n1", "n1", "n2"),
		"dev":     group("n2", "dev-auto", "n2"),
		"chain":   group("region", "region"),
		"region":  group("n3", "n3"),
		"direct":  group("DIRECT", "DIRECT"),
		"n1":      {Type: "Shadowsocks"}, "n2": {Type: "Shadowsocks"}, "n3": {Type: "Hysteria2"},
		"DIRECT": {Type: "Direct"},
	}
	for _, c := range []struct {
		name string
		r    Route
		node string
		auto bool
	}{
		{"auto group chosen", Route{Group: "ai", Auto: "ai-auto"}, "n1", true},
		{"node chosen", Route{Group: "dev", Auto: "dev-auto"}, "n2", false},
		{"no automatic group, through another group", Route{Group: "chain"}, "n3", false},
		{"DIRECT is a node", Route{Group: "direct"}, "DIRECT", false},
		{"group the core does not have", Route{Group: "gone", Auto: "gone-auto"}, "", false},
	} {
		node, auto := c.r.Current(px)
		if node != c.node || auto != c.auto {
			t.Errorf("%s: got %q, %v; want %q, %v", c.name, node, auto, c.node, c.auto)
		}
	}
}

func TestTableOfChain(t *testing.T) {
	tb := Template()
	ai := tb.ByID("ai")
	for _, c := range []struct{ group, rule, want string }{
		{ai.Group, "DomainSuffix", "ai"},
		{ai.Auto, "DomainSuffix", "ai"},
		{"DIRECT", "GeoSite", "direct"},
		{"DIRECT", "Match", ""}, // no rule matched: not the direct route's traffic
		{"DIRECT", "", ""},
		{"somewhere else", "Match", ""},
	} {
		if got := tb.OfChain(c.group, c.rule); got != c.want {
			t.Errorf("OfChain(%q, %q) = %q, want %q", c.group, c.rule, got, c.want)
		}
	}
}

func TestTableStepTitle(t *testing.T) {
	tb := Template()
	ai := tb.ByID("ai")
	for _, c := range []struct{ step, want string }{
		{ai.Group, Title(ai)},
		{ai.Auto, Title(ai) + i18n.T("·自动", " · Auto")},
		{"HK-01", "HK-01"},
		{"DIRECT", "DIRECT"},
	} {
		if got := tb.StepTitle(c.step); got != c.want {
			t.Errorf("StepTitle(%q) = %q, want %q", c.step, got, c.want)
		}
	}
}
