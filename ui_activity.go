package main

// 活动: every connection Moho carries, and the core's log. A query bar
// (tokens app: route: node: host:, status, route, app and network
// filters), a virtualized table whose rows open into their detail, and the
// log. While the page shows and is not paused, a poller fetches both once
// a second and applies them on the main thread.
//
// Design: prototype/index.html (activity, connTable, detail, logView) and
// docs/product-ia.md §5.1.

import (
	"fmt"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/egoist/mygo/ui"

	"github.com/oboo/moho/internal/i18n"
)

const (
	actMaxLogs = 2000
	actArmFor  = 3 * time.Second
	actMono    = "SF Mono, Menlo, monospace"
	actAllNet  = "TCP + UDP"
)

// actAllApps is the app choice that filters no app, in the interface's
// language; actIsAllApps knows it in every language.
func actAllApps() string { return i18n.T("所有应用", "All apps") }

func actIsAllApps(s string) bool {
	return s == "" || slices.Contains(i18n.Every("所有应用", "All apps"), s)
}

// Statuses in the order of the status control; the zero value is active.
var actStatuses = []string{"active", "closed", "failed", "all"}

// Log levels in the order of the level control; the zero value is all.
var actLevels = []struct{ id, zh, en, tag string }{
	{"", "全部", "All", ""}, {"debug", "调试", "Debug", "DBG"}, {"info", "信息", "Info", "INFO"}, {"warning", "警告", "Warning", "WARN"}, {"error", "错误", "Error", "ERR"},
}

type activityUI struct {
	logTab bool // the logs tab, else connections

	// Connections: the query and the filters.
	q      string
	status int    // index in actStatuses
	route  string // route id, "" for all
	app    string // app name, "" or actAllApps() for all
	net    string // actAllNet, "TCP" or "UDP"
	sort   string // rate, down, up, dur ("" is rate)
	asc    bool
	open   string // the connection whose detail shows

	// 断开筛选结果 waits for a second click: when it was armed, and for
	// which filter.
	armedAt  time.Time
	armedSig string

	// Logs.
	level int // index in actLevels
	logQ  string

	paused bool

	// Data, written only on the main thread.
	conns   []Conn
	logs    []LogLine
	lastSeq int64
	polling bool

	list, logList ui.ListState

	// rows is actFiltered's last answer, and what it was for: a frame
	// asks for it more than once, and each frame after it until the
	// connections or the filters change.
	rows    []Conn
	rowsOf  []Conn
	rowsKey string
}

// filterApp opens the page filtered to one app.
func (a *activityUI) filterApp(app string) {
	a.logTab, a.app, a.q, a.route, a.net, a.open = false, app, "", "", actAllNet, ""
	a.status = slices.Index(actStatuses, "all")
}

// showLogs opens the page on its logs.
func (a *activityUI) showLogs() { a.logTab = true }

// ---- Data ----

// apply takes a fetch: the connections as they are, and the log lines
// after the last one seen.
func (a *activityUI) apply(conns []Conn, logs []LogLine) {
	a.conns = conns
	for _, l := range logs {
		if l.Seq > a.lastSeq {
			a.logs = append(a.logs, l)
			a.lastSeq = l.Seq
		}
	}
	if n := len(a.logs) - actMaxLogs; n > 0 {
		a.logs = slices.Delete(a.logs, 0, n)
	}
}

// poll starts the poller unless one runs: while the page shows, the
// window is visible and the page is not paused, it fetches the
// connections and the new log lines once a second.
func (a *activityUI) poll(u *UI) {
	if a.polling || a.paused || u.app == nil || u.win == nil {
		return
	}
	a.polling = true
	win := u.win
	go a.pollWith(func(after int64) ([]Conn, []LogLine) { return u.app.connections(), u.app.logs(after) },
		func(fn func()) { win.Update(fn) },
		func() bool { return u.page == "activity" && !a.paused && win.IsVisible() },
		time.Second)
}

// pollWith is the poller's loop: fetch off the main thread, then on it
// (through update) apply what came if the page still wants it, or stop.
func (a *activityUI) pollWith(fetch func(after int64) ([]Conn, []LogLine), update func(func()), wanted func() bool, every time.Duration) {
	after := int64(-1) // read on the main thread before the first fetch
	for {
		if after < 0 {
			ch := make(chan int64, 1)
			update(func() { ch <- a.lastSeq })
			after = <-ch
		}
		conns, logs := fetch(after)
		if len(logs) > 0 {
			after = max(after, logs[len(logs)-1].Seq)
		}
		more := make(chan bool, 1)
		update(func() {
			if !wanted() {
				a.polling = false
				more <- false
				return
			}
			a.apply(conns, logs)
			more <- true
		})
		if !<-more {
			return
		}
		time.Sleep(every)
	}
}

// close marks connections closed at once and asks the core to close them.
func (a *activityUI) close(u *UI, ids []string) {
	if len(ids) == 0 {
		return
	}
	now := time.Now()
	set := make(map[string]bool, len(ids))
	for _, id := range ids {
		set[id] = true
	}
	for i := range a.conns {
		if set[a.conns[i].ID] && a.conns[i].Status == "active" {
			a.conns[i].Status, a.conns[i].End, a.conns[i].DownRate, a.conns[i].UpRate = "closed", now, 0, 0
		}
	}
	a.rowsKey = "" // changed in place
	if u.app != nil {
		u.do(func() error { return u.app.closeConns(ids) })
	}
}

// ---- Querying ----

func (a *activityUI) statusID() string { return actStatuses[min(max(a.status, 0), len(actStatuses)-1)] }

func (a *activityUI) count(status string) int {
	n := 0
	for _, c := range a.conns {
		if status == "all" || c.Status == status {
			n++
		}
	}
	return n
}

// actNode is the name a connection's exit goes by, 直连 when direct.
func actNode(c Conn) string {
	if c.Node == "" {
		return i18n.T("直连", "Direct")
	}
	return c.Node
}

// actMatch says whether a connection matches the query: words of the form
// app:, route: (id or name), node: or host: match that field, others any.
func (u *UI) actMatch(c Conn, q string) bool {
	if strings.TrimSpace(q) == "" {
		return true
	}
	rname := strings.ToLower(u.route(c.Route).Name)
	node := strings.ToLower(actNode(c))
	host := strings.ToLower(c.Host)
	for _, t := range strings.Fields(strings.ToLower(q)) {
		k, v, ok := strings.Cut(t, ":")
		if !ok {
			k, v = "", t
		}
		var hit bool
		switch k {
		case "app":
			hit = strings.Contains(strings.ToLower(c.App), v)
		case "route":
			hit = strings.Contains(rname, v) || strings.Contains(strings.ToLower(c.Route), v)
		case "node":
			hit = strings.Contains(node, v)
		case "host":
			hit = strings.Contains(host, v)
		default:
			hay := strings.ToLower(strings.Join([]string{c.App, c.Host, c.IP, c.Rule, c.Payload, rname, node}, " "))
			hit = strings.Contains(hay, t)
		}
		if !hit {
			return false
		}
	}
	return true
}

// actFiltered is the connections the query and the filters keep, sorted.
func (u *UI) actFiltered() []Conn {
	a := &u.act
	now := time.Now()
	key := a.sig() + "\x00" + a.sort + "\x00" + strconv.FormatBool(a.asc)
	if a.sort == "dur" { // the order of the live ones changes with time
		key += "\x00" + strconv.FormatInt(now.Unix(), 10)
	}
	if key == a.rowsKey && sameSlice(a.conns, a.rowsOf) {
		return a.rows
	}
	a.rows, a.rowsOf, a.rowsKey = u.actFilter(now), a.conns, key
	return a.rows
}

// sameSlice says whether a and b are the same slice, not equal ones.
func sameSlice[T any](a, b []T) bool {
	return len(a) == len(b) && (len(a) == 0 || &a[0] == &b[0])
}

func (u *UI) actFilter(now time.Time) []Conn {
	a := &u.act
	st := a.statusID()
	allApps := actIsAllApps(a.app)
	var out []Conn
	for _, c := range a.conns {
		switch {
		case st != "all" && c.Status != st:
		case a.route != "" && c.Route != a.route:
		case !allApps && c.App != a.app:
		case a.net != "" && a.net != actAllNet && !strings.EqualFold(c.Network, a.net):
		case !u.actMatch(c, a.q):
		default:
			out = append(out, c)
		}
	}
	key := func(c Conn) float64 {
		switch a.sort {
		case "down":
			return float64(c.Down)
		case "up":
			return float64(c.Up)
		case "dur":
			return float64(actDuration(c, now))
		}
		return float64(c.DownRate + c.UpRate)
	}
	sort.SliceStable(out, func(i, j int) bool {
		ki, kj := key(out[i]), key(out[j])
		if ki != kj {
			return ki < kj == a.asc
		}
		return out[i].Start.After(out[j].Start) // newest first among equals
	})
	return out
}

// sig names the filter, so an armed 断开筛选结果 disarms when it changes.
func (a *activityUI) sig() string {
	return strings.Join([]string{a.q, a.statusID(), a.route, a.app, a.net}, "\x00")
}

func actDuration(c Conn, now time.Time) time.Duration {
	if c.Start.IsZero() {
		return 0
	}
	end := now
	if !c.End.IsZero() {
		end = c.End
	}
	return max(0, end.Sub(c.Start))
}

func actFmtDur(d time.Duration) string {
	s := int(d.Seconds())
	switch {
	case s < 60:
		return fmt.Sprintf("%ds", s)
	case s < 3600:
		return fmt.Sprintf("%dm %02ds", s/60, s%60)
	case s < 86400:
		return fmt.Sprintf("%dh %02dm", s/3600, s/60%60)
	}
	return fmt.Sprintf("%dd %dh", s/86400, s/3600%24)
}

func actTarget(c Conn) string {
	h := c.Host
	if h == "" {
		h = c.IP
	}
	return fmt.Sprintf("%s:%d", h, c.Port)
}

// actShortErr is the last part of an error, as "i/o timeout" of
// "dial tcp 1.2.3.4:443: i/o timeout", for a row; the detail has it all.
func actShortErr(e string) string {
	if e == "" {
		return i18n.T("连接失败", "Connection failed")
	}
	if i := strings.LastIndex(e, ": "); i >= 0 && i+2 < len(e) {
		return e[i+2:]
	}
	return e
}

func actRule(c Conn) string {
	if c.Payload == "" {
		return c.Rule
	}
	return c.Rule + "(" + c.Payload + ")"
}

func (u *UI) actRouteName(c Conn) string {
	if n := u.route(c.Route).Name; n != "" {
		return n
	}
	if c.Route != "" {
		return c.Route
	}
	return i18n.T("未知路由", "Unknown route")
}

// ---- The page ----

func (u *UI) activityPage(c *ui.Context, p *pal) {
	a := &u.act
	a.poll(u)
	if a.net == "" {
		a.net = actAllNet
	}
	if actIsAllApps(a.app) { // also after the language changed
		a.app = actAllApps()
	}
	c.OnWindowShortcut(0, ui.KeyEscape, func() { a.open = "" })
	ui.Column(c).Fill().Children(func() {
		u.actHead(c, p)
		if a.logTab {
			u.actLogBar(c, p)
			u.actLogList(c, p)
		} else {
			u.actQueryBar(c, p)
			u.actTable(c, p)
		}
	})
}

func (u *UI) actHead(c *ui.Context, p *pal) {
	a := &u.act
	n := a.count("active")
	if len(a.conns) == 0 && u.snap.Takeover.On {
		n = u.tick.Active // before the first fetch
	}
	title := fmt.Sprintf(i18n.T("%d 个连接", "%d connections"), n)
	sub := fmt.Sprintf(i18n.T("↓ %s · ↑ %s · 今日 %s", "↓ %s · ↑ %s · Today %s"), fmtRate(u.tick.Down), fmtRate(u.tick.Up), fmtBytes(u.tick.TodayDown+u.tick.TodayUp))
	pageHead(c, p, i18n.T("日志", "Log"), title, sub, false, func() {
		ui.Row(c).Gap(18).Padding(0, 0, 4, 0).Children(func() {
			for _, tab := range []struct {
				key, label string
				logs       bool
			}{{"conns", i18n.T("连接", "Connections"), false}, {"corelog", i18n.T("内核日志", "Core log"), true}} {
				on := a.logTab == tab.logs
				b := ui.ButtonBase(c.Key(tab.key)).Padding(7, 0, 3, 0).Margin(-7, 0, 0, 0).BorderWidth(0, 0, 1.5, 0).BorderColor(ui.Color{}).OnClick(func() { a.logTab = tab.logs })
				col, w := p.mute, 500
				if on {
					b.BorderColor(p.ink)
					col, w = p.ink, 600
				} else if b.Hovered() {
					col = p.ink2
				}
				b.Children(func() { ui.Text(c, tab.label).FontSize(13).FontWeight(w).TextColor(col) })
			}
		})
	})
}

// actLive is the 实时 / 已暂停 mark and the button that toggles it.
func (u *UI) actLive(c *ui.Context, p *pal) {
	a := &u.act
	ui.Row(c).Gap(6).AlignItems(ui.Center).Children(func() {
		col, label := p.good, i18n.T("实时", "Live")
		if a.paused {
			col, label = p.faint, i18n.T("已暂停", "Paused")
		}
		ui.Box(c).Size(7, 7).Radius(p.r(4)).Background(col)
		ui.Text(c, label).FontSize(12).TextColor(p.ink2).NoWrap()
	})
	label := i18n.T("❙❙ 暂停", "❙❙ Pause")
	if a.paused {
		label = i18n.T("▶ 继续", "▶ Resume")
	}
	btn(c.Key("pause"), p, label).OnClick(func() { a.paused = !a.paused })
}

// actSeg is the prototype's .seg: a track of labels, the one chosen raised.
func actSeg(c *ui.Context, p *pal, sel *int, labels []string) ui.Element {
	seg := ui.SegmentedBase(c, sel, len(labels))
	seg.Track.Padding(2).Radius(p.r(8)).Background(p.hair2).Gap(0).Children(func() {
		for i, l := range labels {
			s := seg.Segment(i).Padding(3, 11).Radius(p.r(6))
			col, w := p.ink2, 500
			if i == *sel {
				s.Background(p.card).Shadow(0, 1, 2, 0, ui.RGBA(0, 0, 0, .12))
				col, w = p.ink, 600
			}
			s.Children(func() { ui.Text(c, l).FontSize(12).FontWeight(w).TextColor(col).NoWrap() })
		}
	})
	return seg.Track
}

// actSelect is a small drop-down in the prototype's look.
func actSelect(c *ui.Context, p *pal, sel *string, options []string, label string) {
	s := ui.SelectBase(c, sel)
	s.Trigger.Row().Gap(6).AlignItems(ui.Center).Padding(3, 8).Radius(p.r(7)).Border(1, p.hair).Background(p.card).Label(label)
	if s.Trigger.Hovered() {
		s.Trigger.BorderColor(p.ink)
	}
	s.Trigger.Children(func() {
		ui.Text(c, *sel).FontSize(12).TextColor(p.ink).NoWrap()
		actChevron(c, p.mute)
	})
	s.Popup(func(panel ui.Element) {
		panel.Margin(4, 0, 0, 0).Padding(4).Radius(p.r(9)).Background(p.sheet).Border(1, p.hair).Shadow(0, 6, 20, 0, ui.RGBA(0, 0, 0, .12)).MinWidth(120)
		for _, o := range options {
			item := s.Item(o).Padding(5, 10).Radius(p.r(6))
			col := p.ink
			if item.Highlighted() {
				item.Background(p.ink)
				col = p.paper
			}
			item.Children(func() { ui.Text(c, o).FontSize(12).TextColor(col).NoWrap() })
		}
	})
}

// actChevron is the small down arrow of a drop-down.
func actChevron(c *ui.Context, col ui.Color) {
	ui.Box(c).Size(7, 4).Shrink(0).Margin(1, 0, 0, 0).Draw(func(pt *ui.Painter, r ui.Rect) {
		var path ui.Path
		path.MoveTo(r.X, r.Y).LineTo(r.X+r.W/2, r.Y+r.H).LineTo(r.X+r.W, r.Y)
		pt.StrokePath(&path, 1.3, col)
	})
}

func (u *UI) actQueryBar(c *ui.Context, p *pal) {
	a := &u.act
	rows := u.actFiltered()
	var live []string
	for _, cn := range rows {
		if cn.Status == "active" {
			live = append(live, cn.ID)
		}
	}
	armed := !a.armedAt.IsZero() && c.Now().Sub(a.armedAt) < actArmFor && a.armedSig == a.sig()
	if armed {
		c.After(actArmFor - c.Now().Sub(a.armedAt))
	}
	ui.Column(c).Padding(0, 26).BorderWidth(0, 0, 1, 0).BorderColor(p.hair).Children(func() {
		ui.Row(c).Gap(14).Padding(6, 0, 8, 0).AlignItems(ui.Center).Children(func() {
			ui.Row(c).Grow(1).Gap(10).AlignItems(ui.Center).Children(func() {
				ui.SearchField(c.Key("q"), &a.q).Placeholder(i18n.T("搜索域名、IP、应用、节点、规则", "Search domains, IPs, apps, nodes, rules")).Label(i18n.T("搜索连接", "Search connections")).Grow(1)
				ui.Text(c, "app: route: node: host:").Font(actMono).FontSize(11).TextColor(p.mute).NoWrap()
			})
			u.actLive(c, p)
			if !u.snap.External { // its connections are read, not closed
				label := i18n.T("断开筛选结果", "Close filtered")
				if armed {
					label = fmt.Sprintf(i18n.T("确认断开 %d 个？", "Close %d? Click again"), len(live))
				}
				b := ui.ButtonBase(c.Key("closeall")).Padding(5, 11).Radius(p.r(7)).Disabled(len(live) == 0 && !armed)
				col := p.signal
				switch {
				case armed:
					b.Background(p.signal)
					col = ui.Hex("#ffffff")
				case len(live) == 0:
					b.Border(1, p.hair)
					col = p.faint
				case b.Hovered():
					b.Border(1, p.signal)
				default:
					b.Border(1, p.hair)
				}
				b.OnClick(func() {
					if armed {
						a.armedAt = time.Time{}
						a.close(u, live)
						return
					}
					a.armedAt, a.armedSig = c.Now(), a.sig()
				}).Children(func() { ui.Text(c, label).FontSize(12).FontWeight(500).TextColor(col).NoWrap() })
			}
		})
		ui.Row(c).Gap(16).Padding(2, 0, 10, 0).AlignItems(ui.Center).Children(func() {
			actSeg(c.Key("status"), p, &a.status, []string{
				fmt.Sprintf(i18n.T("活跃 %d", "Active %d"), a.count("active")), fmt.Sprintf(i18n.T("已关闭 %d", "Closed %d"), a.count("closed")),
				fmt.Sprintf(i18n.T("失败 %d", "Failed %d"), a.count("failed")), i18n.T("全部", "All")})
			ui.Row(c).Gap(14).AlignItems(ui.Center).Shrink(1).MinWidth(0).ClipX().Children(func() {
				chip := func(id, label string, mark bool) {
					on := a.route == id
					b := ui.ButtonBase(c.Key("route:"+id)).Row().Gap(5).Padding(6, 0).Margin(-6, 0).AlignItems(ui.Center).OnClick(func() { a.route = id })
					col, w := p.mute, 500
					if on {
						col, w = p.ink, 600
					} else if b.Hovered() {
						col = p.ink2
					}
					b.Children(func() {
						if mark {
							swatch(c, p.routeColor(id), 8)
						}
						ui.Text(c, label).FontSize(12).FontWeight(w).TextColor(col).NoWrap()
					})
				}
				chip("", i18n.T("所有路由", "All routes"), false)
				for _, r := range u.snap.Routes {
					chip(r.ID, r.Name, true)
				}
			})
			ui.Spacer(c)
			apps := []string{actAllApps()}
			seen := map[string]bool{}
			for _, cn := range a.conns {
				if cn.App != "" && !seen[cn.App] {
					seen[cn.App] = true
				}
			}
			if !actIsAllApps(a.app) {
				seen[a.app] = true
			}
			var names []string
			for n := range seen {
				names = append(names, n)
			}
			sort.Slice(names, func(i, j int) bool { return strings.ToLower(names[i]) < strings.ToLower(names[j]) })
			apps = append(apps, names...)
			actSelect(c.Key("app"), p, &a.app, apps, i18n.T("应用", "App"))
			actSelect(c.Key("net"), p, &a.net, []string{actAllNet, "TCP", "UDP"}, i18n.T("网络", "Network"))
		})
	})
}

// ---- The table ----

type actCol struct {
	zh, en string  // the title
	sort   string  // the sort key, "" when not sortable
	w      float32 // a fixed width, or 0 to share by grow
	grow   float32
	min    float32
	end    bool
}

var actCols = []actCol{
	{w: 12},
	{zh: "应用", en: "App", w: 72},
	{zh: "目标", en: "Target", grow: 1.45, min: 150},
	{zh: "协议", en: "Net", w: 36},
	{zh: "命中规则", en: "Rule", grow: 1.2, min: 100},
	{zh: "路由 · 出口", en: "Route · Exit", grow: 1.2, min: 120},
	{zh: "速率", en: "Rate", sort: "rate", w: 76, end: true},
	{zh: "下载", en: "Down", sort: "down", w: 66, end: true},
	{zh: "上传", en: "Up", sort: "up", w: 66, end: true},
	{zh: "时长", en: "Time", sort: "dur", w: 60, end: true},
	{w: 18},
}

const actGap, actPadX = 10, 4

// actRowWidth is the least width of a row: narrower, the table scrolls sideways.
func actRowWidth() float32 {
	w := float32(2*actPadX + actGap*(len(actCols)-1))
	for _, col := range actCols {
		w += col.w + col.min
	}
	return w
}

// actCell sizes an element as column i.
func actCell(e ui.Element, i int) ui.Element {
	col := actCols[i]
	if col.w > 0 {
		return e.Width(col.w).Shrink(0)
	}
	return e.Grow(col.grow).Basis(0).MinWidth(col.min)
}

func actClip(e ui.Element) ui.Element { return e.MaxLines(1).Ellipsis("…") }

func (u *UI) actTable(c *ui.Context, p *pal) {
	a := &u.act
	rows := u.actFiltered()
	if !u.snap.Takeover.On && len(a.conns) > 0 {
		ui.Row(c).Gap(10).Padding(8, 26).Background(p.hair2).AlignItems(ui.Center).Children(func() {
			ui.Box(c).Size(7, 7).Radius(p.r(4)).Background(p.faint)
			ui.Text(c, i18n.T("bowel 现在没有接管流量，新的连接不会经过这里。", "bowel is not taking over traffic; new connections won’t show up here.")).FontSize(12).TextColor(p.ink2).Grow(1)
			btn(c, p, i18n.T("去今日开启 →", "Turn on in Proxy →")).OnClick(func() { u.page = "home" })
		})
	}
	head := ui.Row(c).Gap(actGap).Padding(9, actPadX+26, 7, actPadX+26).AlignItems(ui.Center).BorderWidth(0, 0, 1, 0).BorderColor(p.hair2).ClipX()
	head.Children(func() {
		for i, col := range actCols {
			if col.sort == "" {
				t := ui.Text(c, i18n.T(col.zh, col.en)).FontSize(10.5).FontWeight(600).LetterSpacing(.8).TextColor(p.mute).NoWrap().Ellipsis("…")
				actCell(t, i)
				continue
			}
			key := col.sort
			on := a.sort == key || a.sort == "" && key == "rate"
			label := i18n.T(col.zh, col.en)
			if on {
				label += map[bool]string{true: " ↑", false: " ↓"}[a.asc]
			}
			b := ui.ButtonBase(c.Key("sort:" + key)).Row().Justify(ui.End).OnClick(func() {
				if on {
					a.asc = !a.asc
				} else {
					a.sort, a.asc = key, false
				}
			})
			actCell(b, i).Children(func() {
				colr := p.mute
				if on {
					colr = p.ink
				}
				ui.Text(c, label).FontSize(10.5).FontWeight(600).LetterSpacing(.8).TextColor(colr).NoWrap()
			})
		}
	})
	a.list.Key = func(i int) any { return rows[i].ID }
	a.list.KeepOffset = true
	list := ui.List(c.Key("conns"), &a.list, len(rows), func(i int) { u.actRow(c, p, i, rows[i]) }).
		Grow(1).Padding(0, 26, 16, 26).RowWidth(actRowWidth())
	list.Children(func() {
		if len(rows) > 0 {
			return
		}
		ui.Column(c).Padding(48, 0).Gap(6).AlignItems(ui.Center).Children(func() {
			if !u.snap.Takeover.On && len(a.conns) == 0 {
				ui.Text(c, i18n.T("流量现在不经过 bowel", "Traffic isn’t going through bowel")).FontSize(17).FontWeight(700).TextColor(p.ink2)
				ui.Text(c, i18n.T("开启接管后，经过 bowel 的连接会显示在这里。", "Turn on Takeover, and connections through bowel show up here.")).FontSize(12.5).TextColor(p.mute)
				btn(c, p, i18n.T("去今日开启 →", "Turn on in Proxy →")).Margin(10, 0, 0, 0).OnClick(func() { u.page = "home" })
				return
			}
			ui.Text(c, i18n.T("没有符合条件的连接", "No matching connections")).FontSize(12.5).TextColor(p.mute)
		})
	})
	head.ScrollWith(list)
}

func (u *UI) actRow(c *ui.Context, p *pal, i int, cn Conn) {
	a := &u.act
	open := a.open == cn.ID
	id := cn.ID
	ui.Column(c.Key(id)).Children(func() {
		row := ui.ButtonBase(c.Key("row")).Row().Gap(actGap).Padding(8, actPadX).AlignItems(ui.Center).BorderWidth(0, 0, 1, 0).BorderColor(p.hair2).
			Label(cn.App + " " + actTarget(cn)).Cursor(ui.CursorPointer)
		switch {
		case open:
			row.Background(p.paper2)
		case row.Hovered():
			row.Background(p.hover)
		}
		row.OnClick(func() {
			if a.open == id {
				a.open = ""
			} else {
				// Its detail opens under it: a row near the bottom brings
				// the detail into view rather than open it off the edge.
				a.open = id
				a.list.ScrollIntoView(i)
			}
		})
		ink, ink2 := p.ink, p.ink2
		if cn.Status != "active" {
			ink, ink2 = p.ink2, p.mute
		}
		row.Children(func() {
			dot := p.good
			switch cn.Status {
			case "closed":
				dot = p.faint
			case "failed":
				dot = p.signal
			}
			actCell(ui.Row(c), 0).Children(func() { ui.Box(c).Size(7, 7).Radius(p.r(4)).Background(dot) })
			actCell(actClip(ui.Text(c, cn.App).FontSize(12).TextColor(ink2)), 1)
			host := cn.Host
			if host == "" {
				host = cn.IP
			}
			actCell(actClip(ui.RichText(c, ui.Span{Text: host, Size: 12, Weight: 600, Color: ink}, ui.Span{Text: fmt.Sprintf(":%d", cn.Port), Size: 11, Color: p.mute})), 2)
			actCell(ui.Text(c, strings.ToUpper(cn.Network)).FontSize(11.5).TextColor(p.mute).NoWrap(), 3)
			actCell(actClip(ui.Text(c, actRule(cn)).FontSize(11.5).TextColor(p.mute)), 4)
			actCell(ui.Row(c).Gap(6).AlignItems(ui.Center), 5).Children(func() {
				swatch(c, p.routeColor(cn.Route), 8).Shrink(0)
				spans := []ui.Span{{Text: u.actRouteName(cn), Size: 12, Color: ink2}}
				if cn.Status == "failed" {
					msg := actShortErr(cn.Error)
					if cn.Node != "" {
						msg = cn.Node + " " + msg
					}
					spans = append(spans, ui.Span{Text: " · " + msg, Size: 12, Color: p.signal})
				} else if cn.Node != "" || !u.route(cn.Route).Direct {
					spans = append(spans, ui.Span{Text: " · " + actNode(cn), Size: 12, Color: ink2})
				}
				actClip(ui.RichText(c, spans...)).Grow(1).Shrink(1).MinWidth(0)
			})
			rate := "—"
			if cn.Status == "active" {
				rate = fmtRate(cn.DownRate + cn.UpRate)
			}
			num := func(i int, s string, col ui.Color) {
				actCell(ui.Text(c, s).FontSize(12).FontFeatures("tnum").TextColor(col).NoWrap().TextAlign(ui.End), i)
			}
			num(6, rate, ink)
			if cn.Status == "failed" && cn.Down == 0 && cn.Up == 0 {
				num(7, "—", p.mute)
				num(8, "—", p.mute)
			} else {
				num(7, fmtBytes(cn.Down), ink)
				num(8, fmtBytes(cn.Up), p.mute)
			}
			if cn.Status == "failed" {
				num(9, i18n.T("失败", "Failed"), p.signal)
			} else {
				num(9, actFmtDur(actDuration(cn, time.Now())), ink2)
			}
			actCell(ui.Row(c).Justify(ui.Center), 10).Children(func() {
				if cn.Status != "active" || u.snap.External {
					return
				}
				x := ui.ButtonBase(c.Key("x")).Size(26, 26).Margin(-6, -4, -6, -4).Radius(p.r(6)).Justify(ui.Center).AlignItems(ui.Center).
					Label(i18n.T("断开", "Close")).Tooltip(i18n.T("断开", "Close")).OnClick(func() { a.close(u, []string{id}) })
				col := p.mute
				if x.Hovered() {
					col = p.signal
					x.Background(p.signal.Alpha(.1))
				}
				x.Children(func() { ui.Text(c, "×").FontSize(15).TextColor(col) })
			})
		})
		if open {
			u.actDetail(c, p, cn)
		}
	})
}

// actDetail is what a connection's row opens into.
func (u *UI) actDetail(c *ui.Context, p *pal, cn Conn) {
	a := &u.act
	failed := cn.Status == "failed"
	kv := func(rows func(pair func(k string, v func()))) {
		// The labels' column is as wide as its widest: "ネットワーク" is
		// half again as wide as "网络" and must not break.
		ui.Grid(c).ColumnTracks(ui.FitContent(), ui.Fr(1)).GapX(12).GapY(5).Grow(1).Basis(0).MinWidth(0).AlignContent(ui.Start).Children(func() {
			rows(func(k string, v func()) {
				ui.Text(c, k).FontSize(12).TextColor(p.mute).NoWrap()
				ui.Box(c).MinWidth(0).Children(v)
			})
		})
	}
	mono := func(s string, col ui.Color) func() {
		return func() { ui.Text(c, s).Font(actMono).FontSize(11.5).TextColor(col) }
	}
	ui.Row(c.Key("detail")).Gap(18).Padding(12, 18, 16, 26).Background(p.paper2).BorderWidth(0, 0, 1, 0).BorderColor(p.hair).
		Transition(ui.ElementTransition{Animation: &motion, Enter: &ui.Motion{Collapse: true, Opacity: 0}}).ClipY().
		Children(func() {
			kv(func(pair func(string, func())) {
				proc := cn.Process
				if proc == "" {
					proc = cn.App
				}
				pair(i18n.T("进程", "Process"), mono(proc, p.ink))
				pair(i18n.T("目标", "Target"), mono(actTarget(cn), p.ink))
				ip := cn.IP
				if ip == "" {
					ip = "—"
				}
				pair(i18n.T("目标 IP", "Target IP"), mono(ip, p.ink))
				pair(i18n.T("网络", "Network"), mono(strings.ToUpper(cn.Network), p.ink))
				if !cn.Start.IsZero() {
					pair(i18n.T("开始", "Started"), mono(cn.Start.Format("15:04:05"), p.ink))
				}
			})
			kv(func(pair func(string, func())) {
				rule := cn.Rule
				if cn.Payload != "" {
					rule += "," + cn.Payload
				}
				if _, mine := u.ruleOf(cn); mine {
					pair(i18n.T("规则", "Rule"), func() {
						ui.Row(c).Gap(6).AlignItems(ui.Center).Children(func() {
							ui.Text(c, rule).Font(actMono).FontSize(11.5).TextColor(p.ink).Shrink(1).MinWidth(0)
							mineTag(c, p)
						})
					})
				} else {
					pair(i18n.T("规则", "Rule"), mono(rule, p.ink))
				}
				pair(i18n.T("链路", "Chain"), func() { u.actChain(c, p, cn) })
				exit := i18n.T("本机网络", "Local network")
				if cn.Node != "" {
					exit = cn.Node
					for _, n := range u.snap.Nodes {
						if n.Display == cn.Node || n.Name == cn.Node {
							exit = placeName(n) + " · " + n.Type
							break
						}
					}
				}
				pair(i18n.T("出口", "Exit"), mono(exit, p.ink))
				pair(i18n.T("流量", "Traffic"), mono(fmt.Sprintf("↓ %s · ↑ %s", fmtBytes(cn.Down), fmtBytes(cn.Up)), p.ink))
				if failed || cn.Error != "" {
					msg := cn.Error
					if msg == "" {
						msg = i18n.T("连接失败", "Connection failed")
					}
					pair(i18n.T("错误", "Error"), mono(msg, p.signal))
				}
			})
			ui.Column(c).Gap(6).MinWidth(96).Shrink(0).Children(func() {
				if cn.Status == "active" && !u.snap.External {
					b := ui.ButtonBase(c.Key("kill")).Padding(5, 11).Radius(p.r(7)).Border(1, p.hair).OnClick(func() { a.close(u, []string{cn.ID}) })
					if b.Hovered() {
						b.BorderColor(p.signal)
					}
					b.Children(func() { ui.Text(c, i18n.T("断开", "Close")).FontSize(12).FontWeight(500).TextColor(p.signal) })
				}
				host := cn.Host
				if host == "" {
					host = cn.IP
				}
				if !u.snap.External { // nor does it take our rules
					u.actRouteBtn(c, p, cn)
				}
				btn(c.Key("copy"), p, i18n.T("复制域名", "Copy domain")).OnClick(func() {
					c.WriteClipboard(host)
					u.toasts = append(u.toasts, i18n.T("已复制 ", "Copied ")+host)
				})
				btn(c.Key("logs"), p, i18n.T("相关日志", "Related logs")).OnClick(func() { a.logTab, a.logQ, a.level = true, host, 0 })
			})
		})
}

// actChain is the way a connection went: the app, its route, the chain
// of groups and nodes, the last red when it failed.
func (u *UI) actChain(c *ui.Context, p *pal, cn Conn) {
	steps := []string{cn.App}
	chain := cn.Chain
	rname := u.actRouteName(cn)
	if len(chain) == 0 {
		chain = []string{actNode(cn)}
	}
	ui.Row(c).Wrap().GapX(6).GapY(2).AlignItems(ui.Center).Children(func() {
		arrow := func() { ui.Text(c, "→").FontSize(12).TextColor(p.faint) }
		for _, s := range steps {
			ui.Text(c, s).FontSize(12).FontWeight(600).TextColor(p.ink)
		}
		arrow()
		ui.Row(c).Gap(5).AlignItems(ui.Center).Children(func() {
			swatch(c, p.routeColor(cn.Route), 8)
			ui.Text(c, rname).FontSize(12).FontWeight(600).TextColor(p.ink)
		})
		for i, s := range chain {
			if i == 0 && s == rname && len(chain) > 1 {
				continue
			}
			arrow()
			col := p.ink
			if cn.Status == "failed" && i == len(chain)-1 {
				col = p.signal
			}
			ui.Text(c, s).FontSize(12).FontWeight(600).TextColor(col)
		}
	})
}

// ---- Logs ----

func (a *activityUI) filteredLogs() []LogLine {
	lv := actLevels[min(max(a.level, 0), len(actLevels)-1)].id
	q := strings.ToLower(strings.TrimSpace(a.logQ))
	var out []LogLine
	for _, l := range a.logs {
		if lv != "" && actLevel(l.Level) != lv {
			continue
		}
		if q != "" && !strings.Contains(strings.ToLower(l.Text), q) {
			continue
		}
		out = append(out, l)
	}
	return out
}

// actLevel folds the names a level goes by into the four of actLevels.
func actLevel(s string) string {
	switch strings.ToLower(s) {
	case "debug", "dbg", "silent":
		return "debug"
	case "warning", "warn":
		return "warning"
	case "error", "err", "fatal":
		return "error"
	}
	return "info"
}

func (u *UI) actLogBar(c *ui.Context, p *pal) {
	a := &u.act
	ui.Row(c).Gap(14).Padding(6, 26, 10, 26).AlignItems(ui.Center).BorderWidth(0, 0, 1, 0).BorderColor(p.hair).Children(func() {
		ui.SearchField(c.Key("lq"), &a.logQ).Placeholder(i18n.T("搜索日志", "Search logs")).Label(i18n.T("搜索日志", "Search logs")).Grow(1)
		labels := make([]string, len(actLevels))
		for i, l := range actLevels {
			labels[i] = i18n.T(l.zh, l.en)
		}
		actSeg(c.Key("level"), p, &a.level, labels)
		u.actLive(c, p)
	})
}

func (u *UI) actLogList(c *ui.Context, p *pal) {
	a := &u.act
	lines := a.filteredLogs()
	a.logList.Key = func(i int) any { return lines[i].Seq }
	a.logList.FollowEnd = true
	ui.List(c.Key("logs"), &a.logList, len(lines), func(i int) {
		l := lines[i]
		lv := actLevel(l.Level)
		tag, col, text := "INFO", p.mute, p.ink2
		switch lv {
		case "debug":
			tag, col, text = "DBG", p.faint, p.mute
		case "warning":
			tag, col, text = "WARN", p.warn, p.warn
		case "error":
			tag, col, text = "ERR", p.signal, p.signal
		}
		ui.Row(c.Key(l.Seq)).Gap(12).Padding(5, 4).BorderWidth(0, 0, 1, 0).BorderColor(p.hair2).Children(func() {
			ui.Text(c, l.Time.Format("15:04:05")).Font(actMono).FontSize(11.5).FontFeatures("tnum").TextColor(p.faint).Width(64).Shrink(0)
			ui.Text(c, tag).Font(actMono).FontSize(11.5).FontWeight(700).TextColor(col).Width(40).Shrink(0)
			ui.Text(c, l.Text).Font(actMono).FontSize(11.5).LineHeight(1.5).TextColor(text).Grow(1).Shrink(1).MinWidth(0).Selectable()
		})
	}).Grow(1).Padding(0, 26, 16, 26).Children(func() {
		if len(lines) > 0 {
			return
		}
		msg := i18n.T("还没有日志", "No logs yet")
		if len(a.logs) > 0 {
			msg = i18n.T("没有符合条件的日志", "No matching logs")
		}
		ui.Text(c, msg).FontSize(12.5).TextColor(p.mute).Padding(40, 0).TextAlign(ui.Center).FillWidth()
	})
}
