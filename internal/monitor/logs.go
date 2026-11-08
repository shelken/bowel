package monitor

import (
	"net"
	"net/netip"
	"path"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/oboo/moho/internal/mihomo"
)

// dialFailure is what a failed-dial log line of mihomo says.
type dialFailure struct {
	ID      string // sing-box's id of the connection; mihomo's lines carry none
	Network string // "tcp" / "udp"
	Group   string // the proxy (usually the rule's target group) mihomo dialed through
	Rule    string // rule type, e.g. "Domain"; empty in global/direct mode
	Payload string
	Source  string // ip:port of the client
	Process string // process name, if mihomo found it
	Host    string // destination host or IP
	Port    int
	Error   string
}

// mihomo's tunnel logs a failed dial, at level warning, as
//
//	[TCP] dial Refused (match Domain/refused.example) 127.0.0.1:62508(curl) --> refused.example:80 error: 127.0.0.1:1 connect error: dial tcp 127.0.0.1:1: connect: connection refused
//	[UDP] dial Refused (match IPCIDR/9.9.9.9/32) 127.0.0.1:51520(python3.11) --> 9.9.9.9:53 error: ...
//	[TCP] dial GLOBAL 127.0.0.1:62616(curl) --> global.example:80 error: ...   (no rule: global/direct mode)
//
// The source is "ip:port", "ip:port(process)", "ip:port(process, uid=N)"
// or "ip:port(uid=N)". Group names may contain spaces, process names
// parentheses and rule payloads slashes, hence the lazy groups anchored on
// the fixed " --> " and " error: ".
var dialFailRe = regexp.MustCompile(
	`^\[(TCP|UDP)\] dial (.+?) (?:\(match ([^/()]*)/(.*?)\) )?(\[[^\]]*\]:\d+|[^\s()]+)(?:\((.*?)\))? --> (\S+) error: (.*)$`)

// sing-box logs a failed dial, at level error, as
//
//	[3066431264 5.0s] connection: open connection to example.org:443 using outbound/shadowsocks[node]: dial tcp 192.0.2.1:443: i/o timeout
//	[3066431265 1.0s] connection: open packet connection to 192.0.2.9:53 using outbound/direct[direct]: refused
//	[3066431266 0ms] connection: listen packet connection using  using outbound/vless[node]: closed
//
// (the listen line says "using" twice: sing-box's own wording)
//
// with a list of addresses in place of host:port once the name has been
// resolved. It names the outbound that was dialed, not the rule's group,
// and neither the client nor the process: those are in the other lines of
// the connection, which carry the same id.
var sbDialFailRe = regexp.MustCompile(
	`^(?:\[(\d+) [^\]]*\] )?(?:connection: )?(open connection to|open packet connection to|listen packet connection(?: using)?)(?: (\S+))?(?: +using outbound/[^\[\s]+\[(.*?)\])?: (.*)$`)

func parseSingBoxFailure(text string) (dialFailure, bool) {
	g := sbDialFailRe.FindStringSubmatch(text)
	if g == nil {
		return dialFailure{}, false
	}
	f := dialFailure{ID: g[1], Network: "tcp", Group: g[4], Error: g[5]}
	if g[2] != "open connection to" {
		f.Network = "udp"
	}
	if h, p, err := net.SplitHostPort(g[3]); err == nil {
		f.Host = h
		f.Port, _ = strconv.Atoi(p)
	} else {
		f.Host, _, _ = strings.Cut(strings.Trim(g[3], "[]"), ",")
	}
	return f, true
}

// sbConn is what the lines of one sing-box connection told, before it failed.
type sbConn struct {
	Source string // the client's ip:port
	Path   string // the client's process
}

var (
	sbIDRe     = regexp.MustCompile(`^\[(\d+) [^\]]*\] (.*)$`)
	sbSourceRe = regexp.MustCompile(`: inbound (?:packet )?connection from (\S+)$`)
	sbPathRe   = regexp.MustCompile(`^router: found process path: (.*), user: [^,]*$`)
)

// noteSingBoxConn keeps what a line says of the client of its connection.
func (m *Monitor) noteSingBoxConn(text string) {
	g := sbIDRe.FindStringSubmatch(text)
	if g == nil {
		return
	}
	var c sbConn
	if s := sbSourceRe.FindStringSubmatch(g[2]); s != nil {
		c.Source = s[1]
	} else if p := sbPathRe.FindStringSubmatch(g[2]); p != nil {
		c.Path = p[1]
	} else {
		return
	}
	if len(m.sbConns) >= 4096 { // a connection that failed did so within seconds
		clear(m.sbConns)
	}
	old := m.sbConns[g[1]]
	if c.Source == "" {
		c.Source = old.Source
	}
	if c.Path == "" {
		c.Path = old.Path
	}
	m.sbConns[g[1]] = c
}

func parseDialFailure(text string) (dialFailure, bool) {
	text = strings.TrimSpace(text)
	if f, ok := parseSingBoxFailure(text); ok {
		return f, true
	}
	g := dialFailRe.FindStringSubmatch(text)
	if g == nil {
		return dialFailure{}, false
	}
	f := dialFailure{
		Network: strings.ToLower(g[1]),
		Group:   g[2],
		Rule:    g[3],
		Payload: g[4],
		Source:  g[5],
		Process: g[6],
		Host:    g[7],
		Error:   g[8],
	}
	if i := strings.Index(f.Process, ", uid="); i >= 0 {
		f.Process = f.Process[:i]
	} else if strings.HasPrefix(f.Process, "uid=") {
		f.Process = ""
	}
	if h, p, err := net.SplitHostPort(g[7]); err == nil {
		f.Host = h
		f.Port, _ = strconv.Atoi(p)
	}
	return f, true
}

// addLog keeps one line of the core's log and turns failed dials into
// failed connections and events.
func (m *Monitor) addLog(l mihomo.Log, now time.Time) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.seq++
	m.logs = push(m.logs, Log{Seq: m.seq, Time: now, Level: l.Type, Text: l.Payload}, maxLogs)

	if l.Type == "info" {
		m.noteSingBoxConn(l.Payload)
	}
	if l.Type != "warning" && l.Type != "error" {
		return
	}
	f, ok := parseDialFailure(l.Payload)
	if !ok || (m.SkipGroup != "" && f.Group == m.SkipGroup) {
		return
	}
	if sb, known := m.sbConns[f.ID]; f.ID != "" && known {
		f.Source = sb.Source
		if sb.Path != "" {
			f.Process = path.Base(sb.Path)
			m.paths[f.Process] = sb.Path
		}
	}
	// mihomo retries a TCP dial several times within a few seconds, each
	// logged alike; the client's source port tells one dial from another.
	for k, t := range m.recentFail {
		if now.Sub(t) > time.Minute {
			delete(m.recentFail, k)
		}
	}
	key := f.ID + "|" + f.Network + "|" + f.Source + "|" + f.Host + ":" + strconv.Itoa(f.Port) + "|" + f.Group
	if _, dup := m.recentFail[key]; dup {
		m.recentFail[key] = now
		return
	}
	m.recentFail[key] = now

	m.failSeq++
	proc := m.paths[f.Process]
	if f.Process == "" && m.FindProcess != nil {
		// the client may still be waiting on its socket
		if src, err := netip.ParseAddrPort(f.Source); err == nil {
			proc = m.FindProcess(f.Network, src)
		}
	}
	app := appName(f.Process, proc)
	c := Conn{
		ID:      "failed-" + strconv.FormatInt(m.failSeq, 10),
		App:     app,
		Process: proc,
		Host:    f.Host,
		Port:    f.Port,
		Network: f.Network,
		Rule:    f.Rule,
		Payload: f.Payload,
		Start:   now,
		End:     now,
		Status:  "failed",
		Error:   f.Error,
	}
	if net.ParseIP(f.Host) != nil {
		c.IP = f.Host
	}
	if f.Group != "" {
		c.Chains = []string{f.Group}
	}
	m.closed = push(m.closed, c, maxClosed)
	m.events = push(m.events, Event{Time: now, App: app, Host: f.Host, Chains: c.Chains, Failed: true, Error: f.Error}, maxEvents)
}
