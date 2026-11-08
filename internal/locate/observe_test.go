package locate

import (
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/oboo/moho/internal/mihomo"
)

// observedCore is a trace endpoint and a core's controller. The controller
// lists the trace's connection only while the trace holds it open, as a real
// core does, under the chain it is given.
type observedCore struct {
	trace  *httptest.Server
	client *mihomo.Client

	mu   sync.Mutex
	port string
	open bool
}

func newObservedCore(t *testing.T, chain, proxies string) *observedCore {
	t.Helper()
	o := &observedCore{}
	o.trace = httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, "ip=203.0.113.7\ncolo=NRT\nloc=JP\n")
	}))
	o.trace.Config.ConnState = func(c net.Conn, s http.ConnState) {
		o.mu.Lock()
		defer o.mu.Unlock()
		switch s {
		case http.StateNew:
			_, o.port, _ = net.SplitHostPort(c.RemoteAddr().String())
			o.open = true
		case http.StateClosed, http.StateHijacked:
			o.open = false
		}
	}
	o.trace.Start()
	t.Cleanup(o.trace.Close)

	ctl := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/connections":
			o.mu.Lock()
			defer o.mu.Unlock()
			if !o.open || chain == "" {
				io.WriteString(w, `{"connections":[]}`)
				return
			}
			fmt.Fprintf(w, `{"connections":[{"id":"x","chains":%s,"rule":"final","metadata":{"sourcePort":%q}}]}`, chain, o.port)
		case "/proxies":
			io.WriteString(w, proxies)
		default:
			t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
		}
	}))
	t.Cleanup(ctl.Close)
	o.client = mihomo.NewTCP(strings.TrimPrefix(ctl.URL, "http://"), "")
	return o
}

const observedProxies = `{"proxies":{
	"main":{"name":"main","type":"Selector","now":"pool","all":["pool","direct"]},
	"pool":{"name":"pool","type":"URLTest","now":"n1","all":["n1"]},
	"direct":{"name":"direct","type":"Direct"},
	"n1":{"name":"n1","type":"Shadowsocks"}}}`

// The node a request went through is the first non-group of its chain; its
// exit is what the trace says, kept under that node's name.
func TestObserveCreditsTheNodeThatCarriedTheRequest(t *testing.T) {
	o := newObservedCore(t, `["n1","pool","main"]`, observedProxies)
	l := New("")
	node, r, err := l.Observe(t.Context(), o.client, o.trace.URL)
	if err != nil {
		t.Fatal(err)
	}
	if node != "n1" || r.Source != SourceProbe || r.Place == nil || r.Place.Country != "JP" || r.IP != "203.0.113.7" {
		t.Fatalf("observed %q %+v", node, r)
	}
	if g := l.Guess("n1", "Relay-7 专线"); g.Source != SourceProbe || g.Place == nil || g.Place.Country != "JP" {
		t.Errorf("guess after observing: %+v", g)
	}
	if !l.Fresh("n1") || l.Fresh("other") {
		t.Errorf("fresh: n1 %v, other %v", l.Fresh("n1"), l.Fresh("other"))
	}
}

// A request the core never saw (no TUN, no system proxy: it went out direct)
// says nothing about any node.
func TestObserveWithoutTheRequestInTheConnectionsSaysNothing(t *testing.T) {
	o := newObservedCore(t, ``, observedProxies)
	l := New("")
	if node, r, err := l.Observe(t.Context(), o.client, o.trace.URL); err == nil {
		t.Fatalf("observed %q %+v", node, r)
	}
	if l.Fresh("n1") || l.Guess("n1", "Relay-7 专线").Source != SourceUnknown {
		t.Error("something was kept")
	}
}

// A request that left the core direct was not carried by a node.
func TestObserveDoesNotCreditADirectExit(t *testing.T) {
	o := newObservedCore(t, `["direct","main"]`, observedProxies)
	l := New("")
	if node, r, err := l.Observe(t.Context(), o.client, o.trace.URL); err == nil {
		t.Fatalf("observed %q %+v", node, r)
	}
	if l.Fresh("direct") {
		t.Error("direct was kept")
	}
}
