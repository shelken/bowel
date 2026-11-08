package routes

import (
	"slices"
	"strconv"
	"strings"

	"github.com/oboo/moho/internal/mihomo"
)

// Discover is the routes of a core that Moho did not configure: each
// selector that rules send traffic to is a route, and so is the Direct
// outbound when a rule targets it. The default route is the group GLOBAL
// has chosen, which is where traffic no rule matched goes.
func Discover(px map[string]mihomo.Proxy, rules []mihomo.Rule) Table {
	var t Table
	def := px["GLOBAL"].Now
	if px[def].Type == "Selector" {
		t = append(t, Route{ID: Default, Name: "默认", Color: "--r-default", Summary: defaultSummary, Group: def})
	}
	at := map[string]int{} // target -> index in t
	if len(t) > 0 {
		at[def] = 0
	}
	used := map[string]bool{Default: true, "direct": true} // ids Moho gives its own routes
	direct := -1                                           // index of the direct route
	palette := []string{"--r-ai", "--r-media", "--r-dev"}
	for _, rl := range rules {
		name := target(rl.Proxy)
		i, seen := at[name]
		if !seen {
			switch px[name].Type {
			case "Selector":
				id := name
				for n := 2; used[id] || (id != name && px[id].Type == "Selector"); n++ {
					id = name + "-" + strconv.Itoa(n)
				}
				used[id] = true
				t = append(t, Route{ID: id, Name: name, Group: name, Color: palette[0]})
				palette = append(palette[1:], palette[0])
			case "Direct":
				if direct < 0 {
					t = append(t, Route{ID: "direct", Name: "直连", Color: "--r-direct", Direct: true})
					direct = len(t) - 1
				}
				at[name] = direct
				t[direct].Rules = append(t[direct].Rules, rl.Payload)
				continue
			default:
				continue
			}
			i = len(t) - 1
			at[name] = i
		}
		t[i].Rules = append(t[i].Rules, rl.Payload)
	}
	return t
}

// target is the name a rule sends traffic to. sing-box lists a route
// action's options after the outbound: route(name,option,...).
func target(proxy string) string {
	if n, ok := strings.CutPrefix(proxy, "route("); ok {
		n, _, _ = strings.Cut(strings.TrimSuffix(n, ")"), ",")
		return n
	}
	return proxy
}

// Nodes is the names of the nodes of a core that has no providers to say:
// every proxy that is neither a group nor a stand-in for no node at all.
func Nodes(px map[string]mihomo.Proxy) []string {
	var out []string
	for name, p := range px {
		switch {
		case len(p.All) > 0:
		case slices.Contains([]string{"Direct", "Reject", "RejectDrop", "Pass", "Compatible"}, p.Type):
		default:
			out = append(out, name)
		}
	}
	slices.Sort(out)
	return out
}
