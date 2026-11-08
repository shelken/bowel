package main

// The native interface: one window of MyGo's `ui`, drawn on the GPU. The
// view reads u.snap and u.tick on the main thread; the app pushes new ones
// with setSnapshot and setTick. Actions run on goroutines through u.do,
// which turns errors into toasts.
//
// Design: docs/product-ia.md (§4–5.1) and prototype/index.html (v8).

import (
	"embed"
	"fmt"
	"maps"
	"math"
	"strings"
	"sync"
	"time"

	"github.com/egoist/mygo"
	"github.com/egoist/mygo/ui"

	"github.com/oboo/moho/internal/geo"
	"github.com/oboo/moho/internal/i18n"
	"github.com/oboo/moho/internal/settings"
)

type UI struct {
	app  *App
	win  *mygo.Window
	snap Snapshot
	tick Tick

	page   string // home, activity, providers
	toasts []string

	home  homeUI
	act   activityUI
	prov  providersUI
	set   settingsUI
	panel panelUI
	gw    gatewayUI
}

func newUI(a *App) *UI { return &UI{app: a, page: "home", home: newHomeUI()} }

func (u *UI) setSnapshot(s Snapshot) {
	if u.win == nil {
		u.snap = s
		return
	}
	u.win.Update(func() { u.snap = s; u.settingsRedraw() })
}

func (u *UI) setTick(t Tick) {
	if u.win == nil {
		u.tick = t
		return
	}
	u.win.Update(func() { u.tick = t; u.home.pushTick(t) })
}

// do runs an action off the main thread; a failure becomes a toast.
func (u *UI) do(fn func() error) {
	go func() {
		if err := fn(); err != nil {
			u.toast(err.Error())
		}
	}()
}

func (u *UI) toast(msg string) {
	if u.win == nil {
		return
	}
	u.win.Update(func() { u.toasts = append(u.toasts, msg) })
}

func (u *UI) pending(key string) bool {
	for _, p := range u.snap.Pending {
		if p == key {
			return true
		}
	}
	return false
}

// ---- Palette and type ----

// pal is the editorial palette: paper and ink (in the dark, a warm
// graphite, not a blue-black), one signal color, and the
// route colors, which appear only on the map and as small marks.
type pal struct {
	paper, paper2, card, sheet ui.Color
	ink, ink2, mute, faint     ui.Color
	hair, hair2, hover         ui.Color
	signal, good, warn         ui.Color
	route                      map[string]ui.Color
	// accent fills what is chosen and what to press, the theme color of
	// the settings; onAccent is text on it. accentLine is the accent where
	// it must read on paper, as a line or a word: ink when it is too pale.
	// accentOf is the setting they were made from.
	accent, onAccent, accentLine ui.Color
	accentOf                     string
	// dark is the dark palette's: the presets have a color for each.
	dark bool
	// The skin's (skins.go): font is the face of text, display of big
	// type; round scales corners, 0 square; glow, when not clear, haloes
	// big type and what is filled with the accent; deco is the texture laid
	// over a window.
	font, display string
	round         float32
	glow          ui.Color
	deco          func(pt *ui.Painter, r ui.Rect, p *pal)
}

var lightPal = pal{
	paper: ui.Hex("#f6f4ef"), paper2: ui.Hex("#efece5"), card: ui.Hex("#fbfaf7"), sheet: ui.Hex("#fdfcf9"),
	ink: ui.Hex("#151413"), ink2: ui.Hex("#3b3936"), mute: ui.Hex("#8b877e"), faint: ui.Hex("#c2bdb2"),
	hair: ui.RGBA(21, 20, 19, .11), hair2: ui.RGBA(21, 20, 19, .06), hover: ui.RGBA(21, 20, 19, .035),
	signal: ui.Hex("#e2462b"), good: ui.Hex("#2c9a5a"), warn: ui.Hex("#cf8a12"),
	route: map[string]ui.Color{"default": ui.Hex("#2f5bea"), "ai": ui.Hex("#7a4de0"), "media": ui.Hex("#e0457b"), "dev": ui.Hex("#0e9db1"), "direct": ui.Hex("#9a968c")},
	font:  sans, display: sans, round: 1,
}

var darkPal = pal{
	dark:  true,
	paper: ui.Hex("#171615"), paper2: ui.Hex("#1c1b1a"), card: ui.Hex("#201f1d"), sheet: ui.Hex("#242321"),
	ink: ui.Hex("#f3f1ec"), ink2: ui.Hex("#cfccc5"), mute: ui.Hex("#8e8a82"), faint: ui.Hex("#4d4a45"),
	hair: ui.RGBA(244, 243, 238, .11), hair2: ui.RGBA(244, 243, 238, .06), hover: ui.RGBA(244, 243, 238, .04),
	signal: ui.Hex("#ff5a3c"), good: ui.Hex("#3fc07a"), warn: ui.Hex("#f0a630"),
	route: map[string]ui.Color{"default": ui.Hex("#5b80ff"), "ai": ui.Hex("#9a74ff"), "media": ui.Hex("#ff5f93"), "dev": ui.Hex("#22c3d6"), "direct": ui.Hex("#77736b")},
	font:  sans, display: sans, round: 1,
}

// themeAccent is the theme color the windows draw with: the settings'
// Accent, which each window's view sets (useAccent) before it draws.
var themeAccent = settings.Defaults.Accent

// useAccent makes a the theme color of what is drawn next; "" is the
// default's.
func useAccent(a string) {
	if a == "" {
		a = settings.Defaults.Accent
	}
	themeAccent = a
}

// windowPal is p for a window that fades when it loses the user's input,
// as AppKit fades an inactive window's selection: there the accent, what
// is chosen and what to press, goes a quiet gray with ink on it, and the
// accent's lines and selected text go muted. The menu bar's panel and the
// QR hint, never the active window, keep the accent.
func windowPal(c *ui.Context, p *pal) *pal {
	if c.WindowActive() {
		return p
	}
	q := *p
	q.accent, q.onAccent, q.accentLine = p.paper.Mix(p.ink, inactiveMix(p)), p.ink, p.mute
	return &q
}

// inactiveMix is how much ink the inactive gray takes over the paper: a
// touch more in the dark, where the same share reads fainter.
func inactiveMix(p *pal) float32 {
	if p.dark {
		return .2
	}
	return .14
}

// palette is the skin's palette (themeSkin) for the Mac's appearance.
func palette(c *ui.Context) *pal {
	p := skinByID(themeSkin).pal(c.Theme().Dark)
	if p.accentOf != themeAccent {
		p.setAccent(themeAccent)
	}
	return p
}

// accentPresets are the theme colors the settings offer as swatches, with
// their names; "ink" is none, the interface in black and white. hex is
// what the settings keep; light and dark are the colors drawn on each
// paper, "" for hex itself: a lime that glows on graphite is all but
// white on paper, and an indigo that reads on paper sinks into graphite.
var accentPresets = []struct{ hex, light, dark, zh, en string }{
	{"#DDFB4C", "#7A9E00", "", "青柠", "Lime"},
	{"#4B3BF5", "", "#7B6DFF", "靛蓝", "Indigo"},
	{"#FF6A3D", "#E8552A", "", "珊瑚", "Coral"},
	{"#14B8A6", "#0E9F8F", "", "青绿", "Teal"},
	{"#FF5FA2", "#E54888", "", "桃红", "Pink"},
	{"ink", "", "", "墨色", "Ink"},
}

// accentHex is the color theme color a draws as on the dark paper or the
// light: a preset's own for it, or a itself, as a color typed in is.
func accentHex(a string, dark bool) string {
	for _, pr := range accentPresets {
		if !strings.EqualFold(pr.hex, a) {
			continue
		}
		if dark && pr.dark != "" {
			return pr.dark
		}
		if !dark && pr.light != "" {
			return pr.light
		}
	}
	return a
}

// setAccent makes p's accent colors from a theme color (settings.Accent).
func (p *pal) setAccent(a string) {
	p.accentOf = a
	if a == "ink" || !settings.ValidAccent(a) {
		p.accent, p.onAccent, p.accentLine = p.ink, p.paper, p.ink
		return
	}
	p.accent = ui.Hex(accentHex(a, p.dark))
	// Text on it: whichever of near-black and white reads better.
	dark := ui.Hex("#171615")
	if contrast(dark, p.accent) >= contrast(ui.Hex("#ffffff"), p.accent) {
		p.onAccent = dark
	} else {
		p.onAccent = ui.Hex("#ffffff")
	}
	p.accentLine = p.accent
	if contrast(p.accent, p.paper) < 2.2 {
		p.accentLine = p.ink
	}
}

// accentTheme puts p's accent in t: the fills of switches, selections and
// primary buttons, and the focus ring.
func accentTheme(t *ui.Theme, p *pal) {
	t.Accent, t.AccentText = p.accent, p.onAccent
	t.AccentHover, t.AccentPressed = p.accent.Mix(p.onAccent, .1), p.accent.Mix(p.onAccent, .2)
	// Not the toolkit's own fade: the panel, never active, keeps the
	// accent, and the windows fade theirs to a gray made for ink on it
	// (windowPal), where a pale accent's dark text would fade with it.
	t.InactiveAccent = p.accent
	t.Focus = p.accentLine.Alpha(.55)
	t.Selection = p.accentLine.Alpha(.25)
}

// contrast is WCAG's contrast ratio of two opaque colors, 1 to 21.
func contrast(a, b ui.Color) float64 {
	lum := func(c ui.Color) float64 {
		ch := func(v uint8) float64 {
			s := float64(v) / 255
			if s <= .04045 {
				return s / 12.92
			}
			return math.Pow((s+.055)/1.055, 2.4)
		}
		return .2126*ch(c.R) + .7152*ch(c.G) + .0722*ch(c.B)
	}
	l1, l2 := lum(a), lum(b)
	if l1 < l2 {
		l1, l2 = l2, l1
	}
	return (l1 + .05) / (l2 + .05)
}

func (p *pal) routeColor(id string) ui.Color {
	if col, ok := p.route[id]; ok {
		return col
	}
	return p.mute
}

// routePal is p with colors for a core's own routes. Their ids are the
// core's group names, which the palette does not know; each takes the
// color of the token it carries ("--r-media" is p.route["media"]).
func (u *UI) routePal(p *pal) *pal {
	if !u.snap.External {
		return p
	}
	q := *p
	q.route = maps.Clone(p.route)
	for _, r := range u.snap.Routes {
		if col, ok := p.route[strings.TrimPrefix(r.Color, "--r-")]; ok {
			q.route[r.ID] = col
		}
	}
	return &q
}

// sans is the face of all text, headlines included: OPPO Sans, bundled
// (registerFonts), with the system's for what it lacks or when it did not
// load.
const sans = "OPPO Sans 4.0, PingFang SC, system-ui"

// motion is the one curve of the interface (the prototype's cubic-bezier(.32,.86,.22,1)).
var motion = ui.Animation{Duration: 520 * time.Millisecond, Ease: ui.CubicBezier(.32, .86, .22, 1)}

// ---- The window ----

func (u *UI) view(c *ui.Context) {
	useLook(u.shownSettings())
	p := windowPal(c, u.routePal(palette(c)))
	applyTheme(c, p, true)
	// The caret in ink: the accent may be too pale for a line on paper.
	c.Root().CaretColor(p.ink)
	for _, m := range u.toasts {
		c.Toast(m)
	}
	u.toasts = nil
	c.OnShortcut(ui.Cmd, ui.KeyW, func() { u.win.Hide() })
	c.OnShortcut(ui.Cmd, ui.Key1, func() { u.page = "home" })
	c.OnShortcut(ui.Cmd, ui.Key2, func() { u.page = "activity" })
	if u.snap.External && u.page == "providers" {
		u.page = "home" // its nodes are its own: no providers to manage
	}
	if !u.snap.External {
		c.OnShortcut(ui.Cmd, ui.Key3, func() { u.page = "providers" })
	}
	c.OnShortcut(ui.Cmd, ui.KeyComma, u.openSettings)
	// ⌘V outside a text field, on another page, with a link or node list on
	// the clipboard: open Provider with it in the add bar. Provider's own
	// page handles ⌘V itself.
	if u.page != "providers" && !u.snap.External && !c.FocusTakesText() && c.Shortcut(ui.Cmd, ui.KeyV) {
		if s := strings.TrimSpace(c.ReadClipboard()); strings.Contains(s, "://") || strings.Contains(s, "proxies:") {
			u.page, u.prov.input, u.prov.focus = "providers", s, true
		}
	}

	ui.Column(c).Fill().Background(p.paper).Children(func() {
		u.titleBar(c, p)
		// Pages switch at once: a tab is a place, not a story.
		ui.Box(c).Grow(1).Children(func() {
			ui.Column(c.Key(u.page)).Absolute().Fill().Children(func() {
				switch u.page {
				case "activity":
					u.activityPage(c, p)
				case "providers":
					u.providersPage(c, p)
				default:
					u.homePage(c, p)
				}
			})
		})
		skinDeco(c, p)
	})
}

// pages are the window's tabs, named in each language.
var pages = []struct{ id, zh, en string }{
	{"home", "代理", "Proxy"},
	{"activity", "日志", "Log"},
	{"providers", "提供商", "Provider"},
}

// titleBar: the window's controls, the tabs, what needs attention, and the
// state on the right. No line under it. Its 50 is what app.go's
// TrafficLightPosition gives, so the controls center with the tabs.
func (u *UI) titleBar(c *ui.Context, p *pal) {
	bar := c.TitleBar()
	ui.Row(c).Height(max(bar.Height, 50)).Padding(0, bar.Right+16, 0, bar.Left+14).Gap(16).AlignItems(ui.Center).DragWindow().Children(func() {
		ui.Row(c).Gap(2).Padding(3).Radius(p.r(10)).Background(p.hair2).Shrink(0).Children(func() {
			for _, pg := range pages {
				id := pg.id
				if id == "providers" && u.snap.External {
					continue
				}
				on := u.page == id
				b := ui.ButtonBase(c.Key(id)).Padding(4, 16).Radius(p.r(7))
				if on {
					lit(b.Background(p.accent), p)
				}
				b.OnClick(func() { u.page = id }).Children(func() {
					ui.Row(c).Gap(6).Children(func() {
						col := p.ink2
						if on {
							col = p.onAccent
						}
						ui.Text(c, i18n.T(pg.zh, pg.en)).FontSize(13).FontWeight(600).TextColor(col)
						if id == "providers" && u.providerWarn() {
							ui.Box(c).Size(5, 5).Radius(p.r(3)).Background(p.warn)
						}
					})
				})
			}
		})
		u.notice(c, p)
		u.statePill(c, p)
		u.settingsButton(c, p)
	})
}

// stateName is a state in the interface's language.
func stateName(s State) string {
	switch s {
	case StateOK:
		return i18n.T("正常", "OK")
	case StateDegraded:
		return i18n.T("降级", "Degraded")
	case StateError:
		return i18n.T("异常", "Error")
	}
	return i18n.T("未接管", "Off")
}

func (u *UI) stateColor(p *pal) ui.Color {
	switch u.snap.State {
	case StateOK:
		return p.good
	case StateDegraded:
		return p.warn
	case StateError:
		return p.signal
	}
	return p.faint
}

func (u *UI) statePill(c *ui.Context, p *pal) {
	ui.Row(c).Gap(6).Padding(4, 11).Radius(p.r(999)).Background(p.hair2).Children(func() {
		ui.Box(c).Size(7, 7).Radius(p.r(4)).Background(u.stateColor(p)).Transition(ui.ElementTransition{Colors: true})
		ui.Text(c, stateName(u.snap.State)).FontSize(12).FontWeight(600).TextColor(p.ink2)
	})
}

func (u *UI) providerWarn() bool {
	for _, pv := range u.snap.Providers {
		if pv.Warn != "" || pv.Error != "" {
			return true
		}
	}
	return false
}

// notice is what needs attention, in the title bar after the tabs: only
// when degraded or broken. It takes the room left, and shortens.
func (u *UI) notice(c *ui.Context, p *pal) {
	row := ui.Row(c).Grow(1).Basis(0).Shrink(1).Gap(8).AlignItems(ui.Center)
	is := u.snap.Issue
	if is == nil || u.snap.State == StateOff {
		return
	}
	col := p.warn
	if is.Level == "error" {
		col = p.signal
	}
	row.Children(func() {
		ui.Row(c.Key("notice")).Gap(8).AlignItems(ui.Center).Shrink(1).
			Transition(ui.ElementTransition{Animation: &motion, Enter: &ui.Motion{Opacity: 0}, Exit: &ui.Motion{Opacity: 0}}).
			Children(func() {
				ui.Box(c).Size(7, 7).Radius(p.r(4)).Background(col).Shrink(0)
				t := ui.Text(c, is.Text).FontSize(12.5).FontWeight(500).TextColor(p.ink).NoWrap().Ellipsis("…").Shrink(1)
				if is.Detail != "" {
					t.Tooltip(is.Detail)
				} else {
					t.Tooltip(is.Text)
				}
				if is.Action != "" {
					busy := is.Action == "retake" && u.pending("retake")
					label := is.Label
					if busy {
						label = i18n.T("正在重新接管…", "Retaking…")
					}
					ui.ButtonBase(c).Shrink(0).Padding(5, 6).Margin(-5, -6).OnClick(func() { u.issueAction(is.Action) }).Children(func() {
						shimmer(ui.Text(c, label).FontSize(12.5).FontWeight(600).Underline().TextColor(p.ink).NoWrap(), busy, p)
					})
				}
			})
	})
}

func (u *UI) issueAction(action string) {
	switch action {
	case "retake":
		u.do(u.app.retake)
	case "activity":
		u.page = "activity"
	case "providers":
		u.page = "providers"
	}
}

// ---- Shared parts ----

// kicker is the small spaced label above a section.
func kicker(c *ui.Context, p *pal, s string) ui.Element {
	return ui.Text(c, s).FontSize(10.5).FontWeight(600).LetterSpacing(1.5).TextColor(p.mute)
}

// pageHead is the head of a list page (日志, 提供商): kicker, big count
// and a muted line beside it, with the page's own controls on the right.
// One function, so switching between the pages doesn't shift the head.
// morph animates the count as it changes; leave it off for counts that
// tick every second.
func pageHead(c *ui.Context, p *pal, kick, title, sub string, morph bool, right func()) {
	ui.Row(c).Gap(18).Padding(18, 26, 12, 26).AlignItems(ui.End).Children(func() {
		ui.Column(c).Gap(4).Grow(1).Children(func() {
			kicker(c, p, kick)
			ui.Row(c).Gap(12).AlignItems(ui.Baseline).Children(func() {
				t := disp(ui.Text(c, title), p).FontSize(34).FontWeight(700).LetterSpacing(-.6).TextColor(p.ink).NoWrap()
				if morph {
					t.ContentTransition(morphText)
				}
				ui.Text(c, sub).FontSize(13).FontWeight(500).FontFeatures("tnum").TextColor(p.mute).NoWrap()
			})
		})
		if right != nil {
			right()
		}
	})
}

// shimmer makes a text show that what it names is being applied: a band
// of ink passes over it, as the prototype's .shimmer.
func shimmer(e ui.Element, on bool, p *pal) ui.Element {
	if !on {
		return e
	}
	t := e.Loop("shimmer", 1100*time.Millisecond, ui.Linear)
	return e.TextColor(p.faint).MaskLinearGradient(ui.LinearGradient{Angle: 90, Stops: []ui.GradientStop{
		{Color: ui.RGBA(0, 0, 0, .35), At: 0},
		{Color: ui.RGBA(0, 0, 0, .35), At: max(0, t-.25)},
		{Color: ui.RGBA(0, 0, 0, 1), At: t},
		{Color: ui.RGBA(0, 0, 0, .35), At: min(1, t+.25)},
		{Color: ui.RGBA(0, 0, 0, .35), At: 1},
	}}).TextColor(p.ink)
}

// pill is the prototype's round button: the accent, or a hairline ghost.
func pill(c *ui.Context, p *pal, label string, primary bool) ui.Element {
	b := ui.ButtonBase(c).Padding(7, 14).Radius(p.r(999))
	if primary {
		lit(b.Background(p.accent), p)
	} else {
		b.Border(1, p.hair)
	}
	return b.Children(func() {
		col := p.ink
		if primary {
			col = p.onAccent
		}
		ui.Text(c, label).FontSize(12.5).FontWeight(600).TextColor(col)
	})
}

// btn is a small outlined button.
func btn(c *ui.Context, p *pal, label string) ui.Element {
	b := ui.ButtonBase(c).Padding(5, 11).Radius(p.r(7)).Border(1, p.hair)
	if b.Hovered() {
		b.BorderColor(p.ink)
	}
	// A label never breaks: "ドメインをコピー" on two lines reads as two buttons.
	return b.Children(func() { ui.Text(c, label).FontSize(12).FontWeight(500).TextColor(p.ink).NoWrap() })
}

// iconBtn is a bare glyph button (‹, ×) with a 32pt target and a hover
// wash, so it is not just the few pixels of the glyph. Callers pull it
// into place with negative margins where the glyph has to line up.
func iconBtn(c *ui.Context, p *pal, glyph string, size float32, label string) ui.Element {
	b := ui.ButtonBase(c).Size(32, 32).Shrink(0).Radius(p.r(8)).Justify(ui.Center).AlignItems(ui.Center).
		Label(label).Tooltip(label + " · Esc")
	col := p.mute
	if b.Pressed() {
		b.Background(p.hair)
		col = p.ink
	} else if b.Hovered() {
		b.Background(p.hair2)
		col = p.ink
	}
	return b.Children(func() { ui.Text(c, glyph).FontSize(size).TextColor(col) })
}

// swatch is a route's color mark.
func swatch(c *ui.Context, col ui.Color, size float32) ui.Element {
	return ui.Box(c).Size(size, size).Radius(2).Background(col)
}

// ---- Formatting ----

func fmtBytes(b int64) string {
	v := float64(b)
	units := []string{"B", "KB", "MB", "GB", "TB"}
	i := 0
	for v >= 1000 && i < len(units)-1 {
		v /= 1024
		i++
	}
	switch {
	case i == 0:
		return fmt.Sprintf("%d B", b)
	case v < 10:
		return fmt.Sprintf("%.2f %s", v, units[i])
	case v < 100:
		return fmt.Sprintf("%.1f %s", v, units[i])
	}
	return fmt.Sprintf("%.0f %s", v, units[i])
}

func fmtRate(b int64) string { return fmtBytes(b) + "/s" }

// splitRate is "1.31" and "MB/s", for a large number beside a small unit.
func splitRate(b int64) (string, string) {
	s := fmtBytes(b)
	v, unit, _ := strings.Cut(s, " ")
	return v, unit + "/s"
}

func fmtDelay(n NodeView) string {
	switch {
	case !n.Tested:
		return "—"
	case n.Delay <= 0:
		return i18n.T("超时", "Timeout")
	}
	return fmt.Sprintf("%d ms", n.Delay)
}

func delayColor(p *pal, ms int, tested bool) ui.Color {
	switch {
	case !tested:
		return p.mute
	case ms <= 0:
		return p.signal
	case ms < 120:
		return p.good
	case ms < 250:
		return p.warn
	}
	return p.signal
}

func ago(t time.Time) string {
	if t.IsZero() {
		return "—"
	}
	d := time.Since(t)
	switch {
	case d < time.Minute:
		return i18n.T("刚刚", "Just now")
	case d < time.Hour:
		return fmt.Sprintf(i18n.T("%d 分钟前", "%d min ago"), int(d.Minutes()))
	case d < 24*time.Hour:
		return fmt.Sprintf(i18n.T("%d 小时前", "%d h ago"), int(d.Hours()))
	}
	return fmt.Sprintf(i18n.T("%d 天前", "%d d ago"), int(d.Hours()/24))
}

// ---- Lookups ----

func (u *UI) node(name string) (NodeView, bool) {
	for _, n := range u.snap.Nodes {
		if n.Name == name {
			return n, true
		}
	}
	return NodeView{}, false
}

func (u *UI) route(id string) RouteView {
	for _, r := range u.snap.Routes {
		if r.ID == id {
			return r
		}
	}
	return RouteView{}
}

func (u *UI) providerName(id string) string {
	for _, pv := range u.snap.Providers {
		if pv.ID == id {
			return pv.Name
		}
	}
	return ""
}

// active says whether a route carries traffic in the current mode.
func (u *UI) active(id string) bool {
	if !u.snap.Takeover.On {
		return false
	}
	switch u.snap.Mode {
	case "global":
		return id == "default"
	case "direct":
		return id == "direct"
	}
	return true
}

// nodeFlag is the flag emoji of where n is, "" while that is not known.
func nodeFlag(n NodeView) string {
	if n.Where.Place != nil {
		return n.Where.Place.Flag
	}
	return ""
}

// flagged lays out n's flag before its name, a round badge sized to the
// name's text (size) sitting on its baseline, as a small picture in a line
// of text does.
func flagged(c *ui.Context, p *pal, n NodeView, size float32, name func()) ui.Element {
	return ui.Row(c).Gap(round(size * .34)).AlignItems(ui.Baseline).Children(func() {
		flagBadge(c, p, nodeFlag(n), round(size*.8))
		name()
	})
}

// flagBadge is the round flag of a place (its flag emoji) d DIPs across,
// drawn from assets/flags, with a hairline so that white flags show on
// paper. No flag is a dashed ring: the place isn't known. A country with
// no picture shows its letters on a plain disc.
func flagBadge(c *ui.Context, p *pal, flag string, d float32) ui.Element {
	cc := countryCode(flag)
	pic := flagSVG(cc)
	return ui.Box(c).Size(d, d).Shrink(0).Draw(func(pt *ui.Painter, r ui.Rect) {
		rad := r.W / 2
		switch {
		case flag == "":
			var ring ui.Path
			ring.Circle(r.X+rad, r.Y+rad, rad-.5).Dash(0, 2, 2)
			pt.StrokePath(&ring, 1, p.faint)
			return
		case pic != nil:
			pt.Image(pic, r, ui.Contain)
		default:
			pt.Fill(r, p.hair2, rad)
			span := ui.Span{Text: cc, Size: d * .4, Weight: 700, Color: p.ink2}
			tw, th := pt.MeasureText(0, span)
			pt.RichText(r.X+(r.W-tw)/2, r.Y+(r.H-th)/2, 0, span)
		}
		pt.Stroke(r, p.ink.Alpha(.12), rad, .75)
	})
}

//go:embed assets/flags/*.svg
var flagFiles embed.FS

var flagSVGs sync.Map // country code → *ui.SVG, nil when there is none

// flagSVG is the round flag of country cc ("HK"), parsed once; nil when
// assets/flags has none.
func flagSVG(cc string) *ui.SVG {
	if v, ok := flagSVGs.Load(cc); ok {
		return v.(*ui.SVG)
	}
	var s *ui.SVG
	if b, err := flagFiles.ReadFile("assets/flags/" + strings.ToLower(cc) + ".svg"); err == nil {
		s, _ = ui.ParseSVG(b)
	}
	flagSVGs.Store(cc, s)
	return s
}

// countryCode is the letters of a flag emoji, "HK" of 🇭🇰.
func countryCode(flag string) string {
	var b []rune
	for _, r := range flag {
		if r >= 0x1F1E6 && r <= 0x1F1FF {
			b = append(b, 'A'+r-0x1F1E6)
		}
	}
	return string(b)
}

func round(v float32) float32 { return float32(math.Round(float64(v))) }

func placeName(n NodeView) string {
	if n.Where.Place != nil {
		return geo.Display(n.Where.Place.City)
	}
	if n.Where.Source == "locating" {
		return i18n.T("定位中", "Locating")
	}
	return i18n.T("位置未知", "Unknown location")
}

// locTag is the small label saying how a place is known.
func locTag(c *ui.Context, p *pal, src string) {
	text, col := i18n.T("未知", "Unknown"), p.warn
	switch src {
	case "probe":
		text, col = i18n.T("实测", "Measured"), p.good
	case "name":
		text, col = i18n.T("按名称", "By name"), p.mute
	case "locating":
		text, col = i18n.T("定位中", "Locating"), p.warn
	}
	// A tag never breaks: squeezed, its letters would stack one a line.
	ui.Box(c).Padding(1, 6).Radius(p.r(4)).Background(col.Alpha(.12)).Shrink(0).Children(func() {
		ui.Text(c, text).FontSize(10.5).FontWeight(600).TextColor(col).NoWrap()
	})
}

var _ = math.Pi
