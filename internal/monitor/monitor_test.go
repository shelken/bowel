package monitor

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/oboo/moho/internal/mihomo"
)

// Lines as the real core (mihomo v1.19.33) logged them.
func TestParseDialFailure(t *testing.T) {
	cases := []struct {
		line string
		want dialFailure
	}{
		{
			"[TCP] dial Refused (match Domain/refused.example) 127.0.0.1:62508(curl) --> refused.example:80 error: 127.0.0.1:1 connect error: dial tcp 127.0.0.1:1: connect: connection refused",
			dialFailure{Network: "tcp", Group: "Refused", Rule: "Domain", Payload: "refused.example", Source: "127.0.0.1:62508", Process: "curl", Host: "refused.example", Port: 80, Error: "127.0.0.1:1 connect error: dial tcp 127.0.0.1:1: connect: connection refused"},
		},
		{
			"[TCP] dial Sock (match Domain/sock.example) 127.0.0.1:62519(curl) --> sock.example:80 error: context deadline exceeded",
			dialFailure{Network: "tcp", Group: "Sock", Rule: "Domain", Payload: "sock.example", Source: "127.0.0.1:62519", Process: "curl", Host: "sock.example", Port: 80, Error: "context deadline exceeded"},
		},
		{
			"[TCP] dial DIRECT (match Match/) 127.0.0.1:62463(curl) --> 127.0.0.1:1 error: dial tcp 127.0.0.1:1: connect: connection refused",
			dialFailure{Network: "tcp", Group: "DIRECT", Rule: "Match", Source: "127.0.0.1:62463", Process: "curl", Host: "127.0.0.1", Port: 1, Error: "dial tcp 127.0.0.1:1: connect: connection refused"},
		},
		{
			"[TCP] dial DIRECT (match Domain/deadhost.invalid) 127.0.0.1:62462(curl) --> deadhost.invalid:80 error: dns resolve failed: couldn't find ip",
			dialFailure{Network: "tcp", Group: "DIRECT", Rule: "Domain", Payload: "deadhost.invalid", Source: "127.0.0.1:62462", Process: "curl", Host: "deadhost.invalid", Port: 80, Error: "dns resolve failed: couldn't find ip"},
		},
		{
			"[TCP] dial DIRECT (match Match/) 127.0.0.1:62555(curl) --> [::1]:1 error: dns resolve failed: ip version error",
			dialFailure{Network: "tcp", Group: "DIRECT", Rule: "Match", Source: "127.0.0.1:62555", Process: "curl", Host: "::1", Port: 1, Error: "dns resolve failed: ip version error"},
		},
		{
			"[UDP] dial Refused (match IPCIDR/9.9.9.9/32) 127.0.0.1:51520(python3.11) --> 9.9.9.9:53 error: 127.0.0.1:1 connect error: dial tcp 127.0.0.1:1: connect: connection refused",
			dialFailure{Network: "udp", Group: "Refused", Rule: "IPCIDR", Payload: "9.9.9.9/32", Source: "127.0.0.1:51520", Process: "python3.11", Host: "9.9.9.9", Port: 53, Error: "127.0.0.1:1 connect error: dial tcp 127.0.0.1:1: connect: connection refused"},
		},
		{ // global mode: no rule
			"[TCP] dial GLOBAL 127.0.0.1:62616(curl) --> global.example:80 error: 127.0.0.1:1 connect error: dial tcp 127.0.0.1:1: connect: connection refused",
			dialFailure{Network: "tcp", Group: "GLOBAL", Source: "127.0.0.1:62616", Process: "curl", Host: "global.example", Port: 80, Error: "127.0.0.1:1 connect error: dial tcp 127.0.0.1:1: connect: connection refused"},
		},
		{ // direct mode, no process found
			"[TCP] dial DIRECT 127.0.0.1:62628 --> 127.0.0.1:1 error: dial tcp 127.0.0.1:1: connect: connection refused",
			dialFailure{Network: "tcp", Group: "DIRECT", Source: "127.0.0.1:62628", Host: "127.0.0.1", Port: 1, Error: "dial tcp 127.0.0.1:1: connect: connection refused"},
		},
		{ // group names with spaces, process names with parentheses, uid
			"[TCP] dial 🚀 节点 选择 (match RuleSet/proxy) 192.168.1.5:50000(Google Chrome Helper (Renderer), uid=501) --> www.google.com:443 error: i/o timeout",
			dialFailure{Network: "tcp", Group: "🚀 节点 选择", Rule: "RuleSet", Payload: "proxy", Source: "192.168.1.5:50000", Process: "Google Chrome Helper (Renderer)", Host: "www.google.com", Port: 443, Error: "i/o timeout"},
		},
		{
			"[TCP] dial Proxy [fe80::1]:50000(uid=501) --> [2001:db8::1]:443 error: x",
			dialFailure{Network: "tcp", Group: "Proxy", Source: "[fe80::1]:50000", Host: "2001:db8::1", Port: 443, Error: "x"},
		},
		// sing-box (route/conn.go) words a failed dial by what it opened and
		// through which outbound; the id ties it to the lines of its connection.
		{
			"[3066431264 5.0s] connection: open connection to example.org:443 using outbound/shadowsocks[IEPL | 日本 | WAP]: dial tcp 192.0.2.1:443: i/o timeout",
			dialFailure{ID: "3066431264", Network: "tcp", Group: "IEPL | 日本 | WAP", Host: "example.org", Port: 443, Error: "dial tcp 192.0.2.1:443: i/o timeout"},
		},
		{ // an address list, when the name was already resolved: no port
			"[1 0ms] connection: open connection to [192.0.2.1,2001:db8::1] using outbound/direct[direct]: dial tcp: connection refused",
			dialFailure{ID: "1", Network: "tcp", Group: "direct", Host: "192.0.2.1", Error: "dial tcp: connection refused"},
		},
		{ // tags may hold brackets; UDP
			"[2 1.0s] connection: open packet connection to [2001:db8::1]:53 using outbound/hysteria2[[HK] node]: timeout",
			dialFailure{ID: "2", Network: "udp", Group: "[HK] node", Host: "2001:db8::1", Port: 53, Error: "timeout"},
		},
		{ // no outbound named
			"connection: open connection to 192.0.2.1:80: refused",
			dialFailure{Network: "tcp", Host: "192.0.2.1", Port: 80, Error: "refused"},
		},
		{
			"[3 2s] connection: listen packet connection using outbound/vless[node]: closed",
			dialFailure{ID: "3", Network: "udp", Group: "node", Error: "closed"},
		},
	}
	for _, c := range cases {
		got, ok := parseDialFailure(c.line)
		if !ok {
			t.Errorf("not parsed: %s", c.line)
			continue
		}
		if got != c.want {
			t.Errorf("%s\n got %+v\nwant %+v", c.line, got, c.want)
		}
	}
	for _, line := range []string{
		"[TCP] 127.0.0.1:62458(curl) --> example.org:80 match Domain(example.org) using Proxy[dead]",
		"[UDP] 127.0.0.1:55904(python3.11) --> 9.9.9.9:53 match IPCIDR(9.9.9.9/32) using SS[ssnode]",
		"[UDP] Resolve Ip error: can't resolve ip: couldn't find ip",
		"Start initial provider p1",
	} {
		if f, ok := parseDialFailure(line); ok {
			t.Errorf("parsed %q as %+v", line, f)
		}
	}
}

func TestFailuresFromLogs(t *testing.T) {
	m := New("")
	now := time.Now()
	refused := "[TCP] dial Refused (match Domain/refused.example) 127.0.0.1:62508(curl) --> refused.example:80 error: 127.0.0.1:1 connect error: dial tcp 127.0.0.1:1: connect: connection refused"
	// mihomo logs each retry of one dial.
	for i := range 10 {
		m.addLog(mihomo.Log{Type: "warning", Payload: refused}, now.Add(time.Duration(i)*100*time.Millisecond))
	}
	m.addLog(mihomo.Log{Type: "info", Payload: "[TCP] 127.0.0.1:62474(curl) --> example.com:443 match Match using DIRECT"}, now)
	m.addLog(mihomo.Log{Type: "warning", Payload: "[UDP] dial Refused (match IPCIDR/9.9.9.9/32) 127.0.0.1:51520 --> 9.9.9.9:53 error: boom"}, now.Add(time.Second))

	conns := m.Conns()
	if len(conns) != 2 {
		t.Fatalf("got %d conns, want 2: %+v", len(conns), conns)
	}
	u, c := conns[0], conns[1]
	if c.Status != "failed" || c.App != "curl" || c.Host != "refused.example" || c.Port != 80 || c.Network != "tcp" ||
		!slices.Equal(c.Chains, []string{"Refused"}) || c.Rule != "Domain" || !strings.Contains(c.Error, "connection refused") {
		t.Errorf("tcp failure: %+v", c)
	}
	if u.App != unknownApp() || u.IP != "9.9.9.9" || u.Network != "udp" || u.Error != "boom" || u.Payload != "9.9.9.9/32" {
		t.Errorf("udp failure: %+v", u)
	}
	ev := m.Events(0)
	if len(ev) != 2 || !ev[0].Failed || ev[0].Host != "9.9.9.9" || ev[1].App != "curl" {
		t.Errorf("events: %+v", ev)
	}
	logs := m.Logs(0)
	if len(logs) != 12 || logs[0].Seq != 1 || logs[11].Seq != 12 || logs[11].Level != "warning" {
		t.Errorf("logs: %d %+v", len(logs), logs)
	}
	if got := m.Logs(10); len(got) != 2 || got[0].Seq != 11 {
		t.Errorf("Logs(10): %+v", got)
	}
	if got := m.Logs(12); len(got) != 0 {
		t.Errorf("Logs(12): %+v", got)
	}
}

// sing-box logs who asked and what was opened on separate lines of one
// connection, all tied by an id; the failure itself says neither.
func TestFailuresFromSingBoxLogs(t *testing.T) {
	m := New("")
	now := time.Now()
	for _, l := range []mihomo.Log{
		{Type: "info", Payload: "[111 0ms] inbound/tun[tun-in]: inbound connection from 198.51.100.1:59308"},
		{Type: "info", Payload: "[111 0ms] router: found process path: /Applications/Foo.app/Contents/MacOS/Foo, user: me"},
		{Type: "info", Payload: "[222 0ms] inbound/tun[tun-in]: inbound connection from 198.51.100.1:59309"},
		{Type: "error", Payload: "[111 5.0s] connection: open connection to example.org:443 using outbound/shadowsocks[node a]: dial tcp 192.0.2.1:443: i/o timeout"},
		{Type: "error", Payload: "[111 5.0s] connection: open connection to example.org:443 using outbound/shadowsocks[node a]: dial tcp 192.0.2.1:443: i/o timeout"},
		{Type: "error", Payload: "[222 5.0s] connection: open packet connection to 192.0.2.9:53 using outbound/direct[direct]: refused"},
	} {
		m.addLog(l, now)
		now = now.Add(time.Second)
	}
	conns := m.Conns()
	if len(conns) != 2 {
		t.Fatalf("got %d conns, want 2: %+v", len(conns), conns)
	}
	u, c := conns[0], conns[1]
	if c.Status != "failed" || c.App != "Foo" || c.Process != "/Applications/Foo.app/Contents/MacOS/Foo" || c.Host != "example.org" ||
		c.Port != 443 || c.Network != "tcp" || !slices.Equal(c.Chains, []string{"node a"}) || !strings.Contains(c.Error, "i/o timeout") {
		t.Errorf("tcp failure: %+v", c)
	}
	if u.App != unknownApp() || u.IP != "192.0.2.9" || u.Network != "udp" || !slices.Equal(u.Chains, []string{"direct"}) {
		t.Errorf("udp failure: %+v", u)
	}
}

func TestLogRing(t *testing.T) {
	m := New("")
	now := time.Now()
	for i := range 5000 {
		m.addLog(mihomo.Log{Type: "info", Payload: fmt.Sprint(i)}, now)
	}
	logs := m.Logs(0)
	if len(logs) != maxLogs || logs[0].Seq != 5000-maxLogs+1 || logs[len(logs)-1].Seq != 5000 {
		t.Fatalf("kept %d, %d..%d", len(logs), logs[0].Seq, logs[len(logs)-1].Seq)
	}
	if got := m.Logs(4990); len(got) != 10 || got[0].Text != "4990" {
		t.Errorf("Logs(4990): %d %+v", len(got), got[0])
	}
}

func fakeConn(id string, down, up int64, start time.Time, chains ...string) mihomo.Connection {
	var c mihomo.Connection
	c.ID, c.Download, c.Upload, c.Start, c.Chains = id, down, up, start, chains
	c.Rule, c.RulePayload = "Domain", "example.com"
	c.Metadata.Network = "TCP"
	c.Metadata.Host = "example.com"
	c.Metadata.DestinationIP = "93.184.216.34"
	c.Metadata.DestinationPort = "443"
	c.Metadata.ProcessPath = "/Applications/Safari.app/Contents/MacOS/Safari"
	return c
}

func TestPollRates(t *testing.T) {
	m := New("")
	// Noon today: Now() rolls the totals by the real clock, so polls on any
	// other day would read as yesterday's and be zeroed.
	y, mo, d := time.Now().Date()
	t0 := time.Date(y, mo, d, 12, 0, 0, 0, time.Local)
	start := t0.Add(-time.Minute)

	// First poll: existing bytes are history, not a rate.
	m.apply(mihomo.Snapshot{DownloadTotal: 1000, UploadTotal: 100, Connections: []mihomo.Connection{
		fakeConn("a", 1000, 100, start, "node1", "Proxy"),
	}}, t0)
	if s := m.Now(); s.Down != 0 || s.Active != 1 || s.TodayDown != 1000 || s.TodayUp != 100 {
		t.Fatalf("first poll: %+v", s)
	}

	// Second poll, 2 s later: a grew by 4000/400, b is new with 500 down.
	m.apply(mihomo.Snapshot{DownloadTotal: 5500, UploadTotal: 500, Connections: []mihomo.Connection{
		fakeConn("a", 5000, 500, start, "node1", "Proxy"),
		fakeConn("b", 500, 0, t0.Add(time.Second)),
	}}, t0.Add(2*time.Second))
	s := m.Now()
	if s.Down != 2250 || s.Up != 200 || s.Active != 2 {
		t.Errorf("second poll: %+v", s)
	}
	if s.ByGroup["Proxy"] != 2000 || s.ByGroup["DIRECT"] != 250 {
		t.Errorf("by group: %+v", s.ByGroup)
	}
	// The bytes, not the rates: what a share adds up whatever the polls' pace.
	if s.GroupBytes["Proxy"] != 4000 || s.GroupBytes["DIRECT"] != 500 {
		t.Errorf("group bytes: %+v", s.GroupBytes)
	}
	if s.TodayDown != 5500 || s.TodayUp != 500 || s.MonthDown != 5500 {
		t.Errorf("totals: %+v", s)
	}
	conns := m.Conns()
	if len(conns) != 2 || conns[0].ID != "b" || conns[1].ID != "a" {
		t.Fatalf("conns: %+v", conns)
	}
	a := conns[1]
	if a.DownRate != 2000 || a.UpRate != 200 || a.Down != 5000 || a.App != "Safari" || a.Port != 443 ||
		a.Network != "tcp" || a.Host != "example.com" || a.IP != "93.184.216.34" || a.Status != "active" {
		t.Errorf("a: %+v", a)
	}

	// Third poll: a went away.
	t3 := t0.Add(3 * time.Second)
	m.apply(mihomo.Snapshot{DownloadTotal: 5600, UploadTotal: 500, Connections: []mihomo.Connection{
		fakeConn("b", 600, 0, t0.Add(time.Second)),
	}}, t3)
	conns = m.Conns()
	if len(conns) != 2 {
		t.Fatalf("conns: %+v", conns)
	}
	a = conns[1]
	if a.ID != "a" || a.Status != "closed" || !a.End.Equal(t3) || a.DownRate != 0 || a.Down != 5000 {
		t.Errorf("closed a: %+v", a)
	}
	if s := m.Now(); s.Down != 100 || s.Active != 1 || s.ByGroup["Proxy"] != 0 || s.ByGroup["DIRECT"] != 100 {
		t.Errorf("third poll: %+v", s)
	}
	ev := m.Events(0)
	if len(ev) != 2 || ev[0].Host != "example.com" || !slices.Equal(ev[1].Chains, []string{"node1", "Proxy"}) {
		t.Errorf("events: %+v", ev)
	}
	if got := m.Events(1); len(got) != 1 {
		t.Errorf("Events(1): %+v", got)
	}

	// The core restarted: totals went down, the new value is the delta.
	m.apply(mihomo.Snapshot{DownloadTotal: 300, UploadTotal: 30}, t0.Add(4*time.Second))
	if s := m.Now(); s.TodayDown != 5900 || s.TodayUp != 530 || s.Active != 0 {
		t.Errorf("after restart: %+v", s)
	}

	// The closed ring keeps 300.
	for i := range 400 {
		m.apply(mihomo.Snapshot{Connections: []mihomo.Connection{fakeConn(fmt.Sprint("c", i), 0, 0, t0)}}, t0.Add(time.Duration(5+i)*time.Second))
	}
	m.stop(t0.Add(time.Hour))
	if n := len(m.Conns()); n != maxClosed {
		t.Errorf("kept %d closed, want %d", n, maxClosed)
	}
	if n := len(m.Events(0)); n != maxEvents {
		t.Errorf("kept %d events, want %d", n, maxEvents)
	}
}

func TestStatsRollover(t *testing.T) {
	path := filepath.Join(t.TempDir(), "sub", "stats.json")
	m := New(path)
	day1 := time.Date(2026, 1, 30, 23, 59, 50, 0, time.Local)
	m.mu.Lock()
	m.addTotalsLocked(100, 10, day1)
	m.mu.Unlock()
	m.saveIfDue(day1, false)

	var st stats
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(b, &st); err != nil {
		t.Fatal(err)
	}
	if st != (stats{Date: "2026-01-30", TodayDown: 100, TodayUp: 10, Month: "2026-01", MonthDown: 100}) {
		t.Errorf("saved %+v", st)
	}
	var raw map[string]any
	_ = json.Unmarshal(b, &raw)
	for _, k := range []string{"date", "todayDown", "todayUp", "month", "monthDown"} {
		if _, ok := raw[k]; !ok {
			t.Errorf("no %q in %s", k, b)
		}
	}

	// Not saved again within 10 s.
	m.mu.Lock()
	m.addTotalsLocked(1, 1, day1.Add(5*time.Second))
	m.mu.Unlock()
	m.saveIfDue(day1.Add(5*time.Second), false)
	if loadStats(path).TodayDown != 100 {
		t.Errorf("saved within 10 s")
	}

	// Past local midnight: a new day, same month.
	day2 := time.Date(2026, 1, 31, 0, 0, 5, 0, time.Local)
	m.mu.Lock()
	m.addTotalsLocked(50, 5, day2)
	got := m.stats
	m.mu.Unlock()
	if got != (stats{Date: "2026-01-31", TodayDown: 50, TodayUp: 5, Month: "2026-01", MonthDown: 151}) {
		t.Errorf("new day: %+v", got)
	}
	m.saveIfDue(day2, false)

	// What New loads.
	if st := loadStats(path); st != got {
		t.Errorf("loaded %+v, want %+v", st, got)
	}
	// A new month.
	feb := time.Date(2026, 2, 1, 8, 0, 0, 0, time.Local)
	m.mu.Lock()
	m.addTotalsLocked(7, 0, feb)
	if m.stats != (stats{Date: "2026-02-01", TodayDown: 7, Month: "2026-02", MonthDown: 7}) {
		t.Errorf("new month: %+v", m.stats)
	}
	m.mu.Unlock()
	m.saveIfDue(feb, false)

	// A stale file is rolled over on load.
	m3 := New(path)
	if s := m3.Now(); s.TodayDown != 0 || s.MonthDown != 0 {
		t.Errorf("stale stats not rolled: %+v", s)
	}
}

// TestRealCore runs the bundled mihomo, downloads through its mixed port
// and makes a dial fail, and checks what the monitor saw.
func TestRealCore(t *testing.T) {
	bin, _ := filepath.Abs("../../resources/darwin-arm64/bin/mihomo")
	if _, err := os.Stat(bin); err != nil {
		t.Skip("no mihomo binary:", err)
	}
	if testing.Short() {
		t.Skip("short")
	}
	dir := t.TempDir()
	for _, f := range []string{"GeoSite.dat", "GeoIP.dat"} {
		if b, err := os.ReadFile(filepath.Join("../../resources/geo", f)); err == nil {
			_ = os.WriteFile(filepath.Join(dir, f), b, 0o600)
		}
	}
	mixed, ctl, closedPort := freePort(t), freePort(t), freePort(t)
	cfg := fmt.Sprintf(`mixed-port: %d
external-controller: 127.0.0.1:%d
log-level: info
mode: rule
ipv6: false
find-process-mode: always
rules:
  - MATCH,DIRECT
`, mixed, ctl)
	if err := os.WriteFile(filepath.Join(dir, "config.yaml"), []byte(cfg), 0o600); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(bin, "-d", dir)
	cmd.Dir = dir
	out, _ := os.Create(filepath.Join(dir, "core.log"))
	cmd.Stdout, cmd.Stderr = out, out
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
		out.Close()
	})

	c := mihomo.NewTCP(fmt.Sprintf("127.0.0.1:%d", ctl), "")
	deadline := time.Now().Add(10 * time.Second)
	for {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		_, err := c.Version(ctx)
		cancel()
		if err == nil {
			break
		}
		if time.Now().After(deadline) {
			b, _ := os.ReadFile(filepath.Join(dir, "core.log"))
			t.Fatalf("core did not start: %v\n%s", err, b)
		}
		time.Sleep(100 * time.Millisecond)
	}

	// A local server that sends 64 KiB every 50 ms for 5 s.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	srv := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/ready" {
			return
		}
		chunk := make([]byte, 64<<10)
		for range 100 {
			if _, err := w.Write(chunk); err != nil {
				return
			}
			w.(http.Flusher).Flush()
			select {
			case <-r.Context().Done():
				return
			case <-time.After(50 * time.Millisecond):
			}
		}
	})}
	go srv.Serve(ln)
	t.Cleanup(func() { srv.Close() })

	// The controller answers before the core routes anything: until then
	// the mixed port refuses connections or answers 502 itself, with
	// nothing in /connections or the log. Wait until a request gets through.
	proxy, _ := url.Parse(fmt.Sprintf("http://127.0.0.1:%d", mixed))
	ready := &http.Client{Transport: &http.Transport{Proxy: http.ProxyURL(proxy), DisableKeepAlives: true}, Timeout: time.Second}
	for {
		resp, err := ready.Get("http://" + ln.Addr().String() + "/ready")
		if err == nil {
			resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				break
			}
		}
		if time.Now().After(deadline) {
			b, _ := os.ReadFile(filepath.Join(dir, "core.log"))
			t.Fatalf("core does not route: %v %v\n%s", resp, err, b)
		}
		time.Sleep(20 * time.Millisecond)
	}

	m := New(filepath.Join(dir, "stats.json"))
	ticks := make(chan struct{}, 100)
	m.OnTick(func() {
		select {
		case ticks <- struct{}{}:
		default:
		}
	})
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { m.Run(ctx, c); close(done) }()
	defer func() {
		cancel()
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Error("Run did not return")
		}
	}()

	hc := &http.Client{Transport: &http.Transport{Proxy: http.ProxyURL(proxy)}, Timeout: 20 * time.Second}
	dl := make(chan error, 1)
	go func() {
		resp, err := hc.Get("http://" + ln.Addr().String() + "/big")
		if err == nil {
			_, err = io.Copy(io.Discard, resp.Body)
			resp.Body.Close()
			if err == nil && resp.StatusCode != http.StatusOK {
				err = fmt.Errorf("status %s", resp.Status)
			}
		}
		dl <- err
	}()
	// A dial the core cannot make.
	go func() {
		resp, err := hc.Get(fmt.Sprintf("http://127.0.0.1:%d/", closedPort))
		if err == nil {
			resp.Body.Close()
		}
	}()

	var sawRate, sawGroup, sawFail bool
	var last Conn
	timeout := time.After(15 * time.Second)
	for !(sawRate && sawGroup && sawFail) {
		select {
		case <-ticks:
		case <-timeout:
			b, _ := os.ReadFile(filepath.Join(dir, "core.log"))
			dlErr := "still running"
			select {
			case err := <-dl:
				dlErr = fmt.Sprint(err)
			default:
			}
			t.Fatalf("rate %v group %v failure %v; last %+v; conns %+v; logs %+v; download: %s\ncore log:\n%s", sawRate, sawGroup, sawFail, last, m.Conns(), m.Logs(0), dlErr, b)
		}
		for _, cn := range m.Conns() {
			if cn.Status == "active" && cn.Port == ln.Addr().(*net.TCPAddr).Port && cn.DownRate > 0 {
				sawRate, last = true, cn
			}
			if cn.Status == "failed" && cn.Port == closedPort && slices.Equal(cn.Chains, []string{"DIRECT"}) &&
				strings.Contains(cn.Error, "refused") {
				sawFail = true
			}
		}
		if m.Now().ByGroup["DIRECT"] > 0 {
			sawGroup = true
		}
	}
	if last.IP != "127.0.0.1" || last.Network != "tcp" || last.App == "" || !slices.Equal(last.Chains, []string{"DIRECT"}) || last.Rule != "Match" {
		t.Errorf("download conn: %+v", last)
	}
	t.Logf("download: app %q process %q down %d at %d B/s", last.App, last.Process, last.Down, last.DownRate)
	if err := <-dl; err != nil {
		t.Errorf("download: %v", err)
	}
	if s := m.Now(); s.TodayDown < 3<<20 {
		t.Errorf("today down %d, want >= 3 MiB", s.TodayDown)
	}
	var failed bool
	for _, e := range m.Events(0) {
		failed = failed || e.Failed
	}
	if !failed {
		t.Errorf("no failure event: %+v", m.Events(0))
	}

	// Run again after cancel, as the integrator does on a core restart.
	cancel()
	<-done
	if s := m.Now(); s.Active != 0 || s.Down != 0 {
		t.Errorf("after Run: %+v", s)
	}
	if _, err := os.Stat(filepath.Join(dir, "stats.json")); err != nil {
		t.Errorf("stats not saved: %v", err)
	}
	for len(ticks) > 0 {
		<-ticks
	}
	ctx2, cancel2 := context.WithCancel(context.Background())
	done2 := make(chan struct{})
	go func() { m.Run(ctx2, c); close(done2) }()
	select {
	case <-ticks:
	case <-time.After(5 * time.Second):
		t.Error("no tick from the second Run")
	}
	cancel2()
	select {
	case <-done2:
	case <-time.After(5 * time.Second):
		t.Error("second Run did not return")
	}
}

func freePort(t *testing.T) int {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	return ln.Addr().(*net.TCPAddr).Port
}

func TestAppName(t *testing.T) {
	for _, c := range [][3]string{
		{"Claude Helper", "/Applications/Claude.app/Contents/Frameworks/Claude Helper.app/Contents/MacOS/Claude Helper", "Claude"},
		{"", "/Applications/Arc.app/Contents/MacOS/Arc", "Arc"},
		{"curl", "/usr/bin/curl", "curl"},
		{"", "/usr/bin/curl", "curl"},
		{"", `C:\Program Files\App\app.exe`, "app.exe"},
		{"", "", unknownApp()},
	} {
		if got := appName(c[0], c[1]); got != c[2] {
			t.Errorf("appName(%q, %q) = %q, want %q", c[0], c[1], got, c[2])
		}
	}
}

// Connections and failed dials the core could not name are named by
// FindProcess, from the client's address; the probe group is skipped.
func TestFindProcessAndSkip(t *testing.T) {
	m := New("")
	m.SkipGroup = "MOHO-PROBE"
	var asked []string
	m.FindProcess = func(network string, src netip.AddrPort) string {
		asked = append(asked, network+" "+src.String())
		if src.Port() == 50001 || src.Port() == 50002 {
			return "/Applications/Arc.app/Contents/Frameworks/Arc Helper.app/Contents/MacOS/Arc Helper"
		}
		return ""
	}
	now := time.Now()
	named := fakeConn("a", 0, 0, now, "DIRECT") // the core named it
	blind := fakeConn("b", 0, 0, now, "DIRECT")
	blind.Metadata.ProcessPath = ""
	blind.Metadata.SourceIP, blind.Metadata.SourcePort = "127.0.0.1", "50001"
	probe := fakeConn("p", 0, 0, now, "香港 01", "MOHO-PROBE")
	probe.Metadata.ProcessPath = ""
	m.apply(mihomo.Snapshot{Connections: []mihomo.Connection{named, blind, probe}}, now)
	m.addLog(mihomo.Log{Type: "warning", Payload: "[TCP] dial 默认 (match Match/) 127.0.0.1:50002 --> x.com:443 error: i/o timeout"}, now)
	m.addLog(mihomo.Log{Type: "warning", Payload: "[TCP] dial MOHO-PROBE 127.0.0.1:50003 --> www.cloudflare.com:443 error: i/o timeout"}, now)

	apps := map[string]string{}
	for _, c := range m.Conns() {
		apps[c.ID] = c.App
	}
	if apps["a"] != "Safari" || apps["b"] != "Arc" || apps["failed-1"] != "Arc" || len(apps) != 3 {
		t.Errorf("apps %v", apps)
	}
	if !slices.Equal(asked, []string{"tcp 127.0.0.1:50001", "tcp 127.0.0.1:50002"}) {
		t.Errorf("asked %v", asked)
	}
}

// TestPollEvery: Run waits Every between polls, and Wake cuts it short.
func TestPollEvery(t *testing.T) {
	polls := make(chan struct{}, 10)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/connections" {
			polls <- struct{}{}
			io.WriteString(w, `{"downloadTotal":0,"uploadTotal":0,"connections":[]}`)
			return
		}
		<-r.Context().Done() // the log stream
	}))
	defer srv.Close()

	m := New("")
	m.Every = func() time.Duration { return time.Hour }
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { m.Run(ctx, mihomo.NewTCP(strings.TrimPrefix(srv.URL, "http://"), "")); close(done) }()
	defer func() { cancel(); <-done }()

	wait := func(what string) {
		select {
		case <-polls:
		case <-time.After(3 * time.Second):
			t.Fatalf("no poll %s", what)
		}
	}
	wait("at the start")
	select {
	case <-polls:
		t.Fatal("polled again before Every")
	case <-time.After(1500 * time.Millisecond): // past the 1 s it used to wait
	}
	m.Wake()
	wait("after Wake")
}
