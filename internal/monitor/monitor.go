// Package monitor watches what the running mihomo core does: live traffic
// per connection and per proxy group, recent connection events, failed
// dials, the core's log and today's/this month's traffic totals.
package monitor

import (
	"context"
	"maps"
	"net/netip"
	"slices"
	"sync"
	"time"

	"github.com/oboo/moho/internal/mihomo"
)

const (
	maxClosed = 300  // closed and failed connections kept
	maxEvents = 200  // events kept
	maxLogs   = 2000 // log lines kept

	pollEvery = time.Second
	saveEvery = 10 * time.Second
)

// Conn is one connection of the core: active, closed, or a dial that failed.
type Conn struct {
	ID, App, Process, Host, IP string // App: process name, else basename of processPath, else "未知"; Process: processPath; Host: host, else sniffHost, else destinationIP
	Port                       int
	Network                    string   // "tcp" / "udp", lower-case
	Rule                       string   // rule type, e.g. "Domain", "Match"
	Payload                    string   // rule payload
	Chains                     []string // as mihomo reports them: [node, …, group]
	Down, Up                   int64    // totals
	DownRate, UpRate           int64    // bytes/s over the last poll
	Start, End                 time.Time
	Status                     string // "active", "closed", "failed"
	Error                      string
	// Source is the client's address: this Mac's own, or another
	// device's on the LAN.
	Source string
}

// Event is a newly seen connection or a failed dial.
type Event struct {
	Time   time.Time
	App    string
	Host   string
	Chains []string
	Failed bool
	Error  string
}

// Log is one line of the core's log.
type Log struct {
	Seq   int64
	Time  time.Time
	Level string // debug, info, warning, error
	Text  string
}

// Snapshot is the state of the core's traffic at the last poll.
type Snapshot struct {
	Down, Up int64            // total rates, bytes/s
	ByGroup  map[string]int64 // download rate by chains[len-1] (the rule's target group, or "DIRECT")
	// GroupBytes is the bytes downloaded by group since the monitor was
	// made, as the polls saw them: what a share of the traffic adds up,
	// however far apart the polls are.
	GroupBytes                    map[string]int64
	Active                        int
	TodayDown, TodayUp, MonthDown int64
}

// Monitor keeps what it has seen of the core. Its methods are safe for
// concurrent use.
type Monitor struct {
	// SkipGroup names a proxy group whose connections and failed dials are
	// the app's own (Moho's location probe), not the user's: they are not
	// tracked. Set it before Run.
	SkipGroup string
	// FindProcess, if set, names the process (its executable's path)
	// behind a client socket the core reported without one. Set it before
	// Run.
	FindProcess func(network string, src netip.AddrPort) string
	// Every, if set, is how long to wait before the next poll, asked
	// after each: longer while no one looks. Wake polls at once. Set it
	// before Run.
	Every func() time.Duration

	statsPath string
	runMu     sync.Mutex // one Run at a time
	saveMu    sync.Mutex // one stats write at a time

	mu       sync.Mutex
	active   map[string]*tracked
	closed   []Conn // oldest first; only the last maxClosed count
	events   []Event
	logs     []Log
	seq      int64
	lastPoll time.Time // zero before the first poll of a Run
	down, up int64     // total rates at the last poll
	byGroup  map[string]int64
	// groupBytes adds up the download by group, across Runs.
	groupBytes map[string]int64
	wake       chan struct{}

	totals     bool  // lastDown/lastUp are known
	lastDown   int64 // the controller's downloadTotal at the last poll
	lastUp     int64
	stats      stats
	dirty      bool
	lastSave   time.Time
	recentFail map[string]time.Time // dedupes mihomo's retries of one dial
	failSeq    int64
	paths      map[string]string // process name -> path, from polls
	sbConns    map[string]sbConn // sing-box's connection id -> what its other lines said
	ticks      []func()
}

type tracked struct {
	Conn
	pollDown, pollUp int64 // totals at the previous poll
}

// New returns a monitor that keeps its traffic totals in the JSON file at
// statsPath, loading them now if the file exists.
func New(statsPath string) *Monitor {
	m := &Monitor{
		statsPath:  statsPath,
		active:     map[string]*tracked{},
		byGroup:    map[string]int64{},
		groupBytes: map[string]int64{},
		wake:       make(chan struct{}, 1),
		recentFail: map[string]time.Time{},
		paths:      map[string]string{},
		sbConns:    map[string]sbConn{},
	}
	m.stats = loadStats(statsPath)
	m.stats.roll(time.Now())
	return m
}

// Run blocks until ctx is done: it polls the core's connections every
// second (or Every) and streams its log at level "info". Call it again with a new
// client when the core restarts; a second Run waits for the previous one
// to return.
func (m *Monitor) Run(ctx context.Context, c *mihomo.Client) {
	m.runMu.Lock()
	defer m.runMu.Unlock()
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	m.mu.Lock()
	m.lastPoll = time.Time{}
	m.mu.Unlock()

	var wg sync.WaitGroup
	wg.Go(func() { m.streamLogs(ctx, c) })

	t := time.NewTimer(pollEvery)
	defer t.Stop()
loop:
	for {
		pctx, pcancel := context.WithTimeout(ctx, 3*time.Second)
		s, err := c.Connections(pctx)
		pcancel()
		if ctx.Err() != nil {
			break
		}
		if err != nil {
			m.pollFailed()
		} else {
			m.apply(s, time.Now())
		}
		m.saveIfDue(time.Now(), false)
		m.tick()
		every := pollEvery
		if m.Every != nil {
			every = max(m.Every(), pollEvery)
		}
		t.Reset(every)
		select {
		case <-ctx.Done():
			break loop
		case <-t.C:
		case <-m.wake:
		}
	}
	cancel()
	wg.Wait()
	m.stop(time.Now())
	m.saveIfDue(time.Now(), true)
	m.tick()
}

func (m *Monitor) streamLogs(ctx context.Context, c *mihomo.Client) {
	for {
		_ = c.StreamLogs(ctx, "info", func(l mihomo.Log) { m.addLog(l, time.Now()) })
		select {
		case <-ctx.Done():
			return
		case <-time.After(time.Second):
		}
	}
}

// Wake has Run poll now rather than at its next turn, as when someone
// starts to look after polls had slowed down. Safe from any goroutine.
func (m *Monitor) Wake() {
	select {
	case m.wake <- struct{}{}:
	default:
	}
}

// OnTick registers fn to be called after each poll, from the monitor's
// goroutine.
func (m *Monitor) OnTick(fn func()) {
	m.mu.Lock()
	m.ticks = append(m.ticks, fn)
	m.mu.Unlock()
}

func (m *Monitor) tick() {
	m.mu.Lock()
	fns := slices.Clone(m.ticks)
	m.mu.Unlock()
	for _, fn := range fns {
		fn()
	}
}

// Now returns the traffic at the last poll and the totals.
func (m *Monitor) Now() Snapshot {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.stats.roll(time.Now()) {
		m.dirty = true
	}
	g := make(map[string]int64, len(m.byGroup))
	for k, v := range m.byGroup {
		g[k] = v
	}
	return Snapshot{
		Down: m.down, Up: m.up, ByGroup: g, GroupBytes: maps.Clone(m.groupBytes), Active: len(m.active),
		TodayDown: m.stats.TodayDown, TodayUp: m.stats.TodayUp, MonthDown: m.stats.MonthDown,
	}
}

// Conns returns the active connections and the last 300 closed or failed
// ones, newest Start first.
func (m *Monitor) Conns() []Conn {
	m.mu.Lock()
	defer m.mu.Unlock()
	closed := last(m.closed, maxClosed)
	out := make([]Conn, 0, len(m.active)+len(closed))
	for _, t := range m.active {
		out = append(out, cloneConn(t.Conn))
	}
	for _, c := range closed {
		out = append(out, cloneConn(c))
	}
	slices.SortStableFunc(out, func(a, b Conn) int {
		if c := b.Start.Compare(a.Start); c != 0 {
			return c
		}
		if a.ID < b.ID {
			return -1
		}
		if a.ID > b.ID {
			return 1
		}
		return 0
	})
	return out
}

// Sources are the client addresses of the active connections, one for
// each connection.
func (m *Monitor) Sources() []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]string, 0, len(m.active))
	for _, t := range m.active {
		out = append(out, t.Source)
	}
	return out
}

// Rate is what an active connection carries now, for totals by app.
type Rate struct {
	ID, App, Host, Rule string
	Group               string // the last of its chains: the group it went through
	Start               time.Time
	DownRate, UpRate    int64
}

// Rates returns the active connections' rates, in no order: Conns without
// the closed ones, the copies of the chains and the sort, for what is
// read every second.
func (m *Monitor) Rates() []Rate {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]Rate, 0, len(m.active))
	for _, t := range m.active {
		c := &t.Conn
		r := Rate{ID: c.ID, App: c.App, Host: c.Host, Rule: c.Rule, Start: c.Start, DownRate: c.DownRate, UpRate: c.UpRate}
		if len(c.Chains) > 0 {
			r.Group = c.Chains[len(c.Chains)-1]
		}
		out = append(out, r)
	}
	return out
}

// Events returns up to n events, newest first; n <= 0 means all kept.
func (m *Monitor) Events(n int) []Event {
	m.mu.Lock()
	defer m.mu.Unlock()
	ev := last(m.events, maxEvents)
	if n <= 0 || n > len(ev) {
		n = len(ev)
	}
	out := make([]Event, 0, n)
	for i := len(ev) - 1; i >= 0 && len(out) < n; i-- {
		e := ev[i]
		e.Chains = slices.Clone(e.Chains)
		out = append(out, e)
	}
	return out
}

// Logs returns the kept log lines with Seq > after, oldest first.
func (m *Monitor) Logs(after int64) []Log {
	m.mu.Lock()
	defer m.mu.Unlock()
	logs := last(m.logs, maxLogs)
	i, _ := slices.BinarySearchFunc(logs, after+1, func(l Log, seq int64) int {
		switch {
		case l.Seq < seq:
			return -1
		case l.Seq > seq:
			return 1
		}
		return 0
	})
	return slices.Clone(logs[i:])
}

func cloneConn(c Conn) Conn {
	c.Chains = slices.Clone(c.Chains)
	return c
}

// push appends v to s and drops old entries once s holds twice max, so
// that appends stay cheap; readers use last(s, max).
func push[T any](s []T, v T, max int) []T {
	s = append(s, v)
	if len(s) >= 2*max {
		n := copy(s, s[len(s)-max:])
		clear(s[n:])
		s = s[:n]
	}
	return s
}

func last[T any](s []T, max int) []T {
	if len(s) > max {
		return s[len(s)-max:]
	}
	return s
}
