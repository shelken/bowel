package main

// The menu bar panel (L0 of docs/product-ia.md, "看一眼、改一下"): a flyout
// hanging from the tray icon. It reads the same u.snap and u.tick as the
// window; tray.go opens it, closes it and redraws it as they change.
// Nothing in it moves: it opens, changes and closes at once.
//
//	● 正常 · 全局模式               [接管]
//	  (the issue line and its action)
//	↓ 1.31 MB/s  ↑ 60 KB/s     今日 2.4 GB
//	节点
//	✓ 自动   最快 + 故障转移
//	  🇭🇰 香港 03 IPLC     当前  41 ms
//	  …the fastest nodes
//	  全部节点…                  42 个
//	模式                   规则 全局 直连
//	打开 bowel                         ⌘O
//	退出 bowel                         ⌘Q

import (
	"fmt"
	"sort"
	"sync/atomic"

	"github.com/egoist/mygo"
	"github.com/egoist/mygo/ui"

	"github.com/oboo/moho/internal/i18n"
)

// panelUI is the panel's state. The flyout is set and cleared on the main
// thread and read from the goroutines that redraw it.
type panelUI struct {
	win atomic.Pointer[mygo.Window] // the open flyout, nil when closed
}

const (
	panelWidth = 320
	panelNodes = 8 // the fastest nodes the panel lists
)

// panelRedraw redraws the open panel. Safe from any goroutine; queued on
// the main thread after the window's Update that changed the state.
func (u *UI) panelRedraw() {
	if fl := u.panel.win.Load(); fl != nil {
		fl.Invalidate()
	}
}

func (u *UI) panelClose() {
	if fl := u.panel.win.Load(); fl != nil {
		fl.Close()
	}
}

// panelShow closes the panel and brings the window up, changing what it
// shows first.
func (u *UI) panelShow(apply func()) {
	u.panelClose()
	if u.app == nil {
		return
	}
	if u.win != nil && apply != nil {
		u.win.Update(apply)
	}
	u.app.showWindow()
}

func panelTheme(c *ui.Context, p *pal) { applyTheme(c, p, true) }

func (u *UI) panelView(c *ui.Context) {
	c.DisableAnimations() // switches and hovers land at once
	useLook(u.shownSettings())
	p := palette(c)
	panelTheme(c, p)
	c.OnShortcut(ui.Cmd, ui.KeyO, func() { u.panelShow(nil) })
	c.OnShortcut(ui.Cmd, ui.KeyQ, mygo.App.Quit)
	c.Root().Background(ui.Transparent)
	// The flyout's CornerRadius cuts it and its Shadow outlines it: no border here.
	ui.Column(c).Width(panelWidth).Radius(p.r(12)).Background(p.paper).Padding(0, 0, 6, 0).Children(func() {
		u.panelHead(c, p)
		u.panelIssue(c, p)
		u.panelSpeed(c, p)
		panelRule(c, p)
		u.panelNodes(c, p)
		panelRule(c, p)
		if !u.snap.External { // its mode is its own
			u.panelMode(c, p)
			panelRule(c, p)
		}
		panelCmd(c, p, "open", i18n.T("打开 bowel", "Open bowel"), "⌘O").OnClick(func() { u.panelShow(nil) })
		panelCmd(c, p, "quit", i18n.T("退出 bowel", "Quit bowel"), "⌘Q").OnClick(mygo.App.Quit)
	})
}

// panelHead: the state, the mode when it is not the usual one, and the
// one switch.
func (u *UI) panelHead(c *ui.Context, p *pal) {
	s := u.snap
	ui.Row(c).Padding(14, 16, 12, 16).Gap(8).AlignItems(ui.Center).Children(func() {
		ui.Box(c).Size(8, 8).Radius(p.r(4)).Background(u.stateColor(p))
		word := stateName(s.State)
		if !s.Takeover.On {
			word = stateName(StateOff)
		}
		// While the switch applies, and while the core it started comes up,
		// the word shimmers.
		starting := u.pending("takeover") || (s.Takeover.On && s.Core.State == "starting")
		shimmer(disp(ui.Text(c, word), p).FontSize(21).FontWeight(700).LetterSpacing(-.3).TextColor(p.ink), starting, p)
		if s.Takeover.On && s.Mode != "rule" && !s.External {
			ui.Text(c, "· "+panelModeName(s.Mode)).FontSize(12).FontWeight(500).TextColor(p.mute)
		}
		ui.Spacer(c)
		on := s.Takeover.On
		if s.External { // there is nothing for bowel to take over
			return
		}
		if ui.Switch(c.Key("takeover"), &on).Label(i18n.T("接管", "Takeover")).Disabled(u.pending("takeover")).Changed() {
			u.do(func() error { return u.app.setTakeover(on) })
		}
	})
}

// panelIssue is the news line, with its action.
func (u *UI) panelIssue(c *ui.Context, p *pal) {
	is := u.snap.Issue
	if is == nil || !u.snap.Takeover.On {
		return
	}
	col := p.warn
	if is.Level == "error" {
		col = p.signal
	}
	ui.Row(c.Key("issue")).Margin(0, 10, 10, 10).Padding(8, 10).Radius(p.r(8)).Background(col.Alpha(.11)).Gap(10).AlignItems(ui.Start).
		Children(func() {
			ui.Box(c).Size(3, 14).Radius(p.r(2)).Background(col).Margin(1, 0, 0, 0)
			ui.Text(c, is.Text).FontSize(12).TextColor(p.ink).Grow(1).Basis(0)
			if is.Action == "" {
				return
			}
			label, tc := is.Label, p.ink
			if is.Action == "retake" && u.pending("retake") {
				label, tc = i18n.T("正在重新接管…", "Retaking…"), p.mute
			}
			ui.ButtonBase(c.Key("act")).Label(is.Label).OnClick(func() { u.panelIssueAction(is.Action) }).Children(func() {
				ui.Text(c, label).FontSize(12).FontWeight(650).Underline().NoWrap().TextColor(tc)
			})
		})
}

func (u *UI) panelIssueAction(action string) {
	switch action {
	case "retake":
		if u.app != nil {
			u.do(u.app.retake)
		}
	case "activity", "providers":
		u.panelShow(func() { u.page = action })
	}
}

// panelSpeed: live down and up, and today's total.
func (u *UI) panelSpeed(c *ui.Context, p *pal) {
	if !u.snap.Takeover.On {
		return
	}
	t := u.tick
	ui.Row(c).Padding(0, 16, 12, 16).Gap(14).AlignItems(ui.Baseline).Children(func() {
		for _, r := range []struct {
			arrow string
			v     int64
		}{{"↓", t.Down}, {"↑", t.Up}} {
			v, unit := splitRate(r.v)
			ui.RichText(c, ui.Span{Text: r.arrow + " ", Size: 12, Color: p.mute},
				ui.Span{Text: v, Size: 13, Weight: 600, Color: p.ink},
				ui.Span{Text: " " + unit, Size: 11, Color: p.mute}).FontFeatures("tnum")
		}
		ui.Spacer(c)
		ui.Text(c, i18n.T("今日 ", "Today ")+fmtBytes(t.TodayDown+t.TodayUp)).FontSize(11.5).FontFeatures("tnum").TextColor(p.mute)
	})
}

// panelNodes: the default exit's nodes, listed right in the panel:
// automatic, the fastest ones, and the way to all of them. Global mode
// points at the same group, so the list serves rule and global alike.
func (u *UI) panelNodes(c *ui.Context, p *pal) {
	s := u.snap
	r := u.route("default")
	ui.Column(c).Padding(8, 6, 6, 6).Children(func() {
		ui.Row(c).Margin(0, 10, 4, 10).AlignItems(ui.Baseline).Children(func() {
			kicker(c, p, i18n.T("节点", "Node"))
			ui.Spacer(c)
			if u.pending("node:default") {
				ui.Text(c, i18n.T("切换中…", "Switching…")).FontSize(11).TextColor(p.mute)
			}
		})
		note := ""
		switch {
		case !s.Takeover.On:
			note = i18n.T("流量不经过 bowel", "Traffic isn’t going through bowel")
		case s.Mode == "direct":
			note = i18n.T("直连模式：不经过任何节点", "Direct mode: no node is used")
		case len(s.Nodes) == 0 && s.External:
			note = i18n.T("这个内核里还没有节点。", "The core has no nodes yet.")
		case len(s.Nodes) == 0:
			note = i18n.T("还没有节点 · 在 bowel 里添加提供商", "No nodes yet · Add a provider in bowel")
		case r.ID == "" || r.Direct:
			note = i18n.T("直接连接", "Direct connection")
		}
		if note != "" {
			ui.Text(c, note).FontSize(12.5).TextColor(p.mute).Margin(2, 10, 4, 10)
			return
		}
		u.panelNodeList(c, p, r)
	})
}

func (u *UI) panelNodeList(c *ui.Context, p *pal, r RouteView) {
	choose := func(node string) {
		if u.app != nil {
			u.do(func() error { return u.app.selectNode(r.ID, node) })
		}
	}
	if !u.snap.External {
		panelNodeRow(c, p, "auto", r.Auto, func() {
			ui.Text(c, i18n.T("自动", "Auto")).FontSize(12.5).FontWeight(600).TextColor(p.ink)
			ui.Text(c, i18n.T("最快 + 故障转移", "Fastest + failover")).FontSize(11).TextColor(p.mute).Grow(1)
		}).OnClick(func() {
			if !r.Auto {
				choose("")
			}
		})
	}
	pickable := u.pickable(r)
	nodes := append([]NodeView(nil), pickable...)
	sort.SliceStable(nodes, func(i, j int) bool { return rankOf(nodes[i]) < rankOf(nodes[j]) })
	if len(nodes) > panelNodes {
		nodes = nodes[:panelNodes]
	}
	for _, n := range nodes {
		name := n.Name
		chosen := !r.Auto && u.picked(r, n)
		panelNodeRow(c, p, name, chosen, func() {
			flagged(c, p, n, 12.5, func() {
				ui.Text(c, n.Display).FontSize(12.5).TextColor(p.ink).MaxLines(1).Grow(1).Basis(0)
			}).Grow(1).Basis(0)
			if r.Auto && r.Node == name {
				ui.Text(c, i18n.T("当前", "Current")).FontSize(10.5).FontWeight(600).TextColor(p.mute)
			}
			ui.Text(c, fmtDelay(n)).FontSize(11.5).FontFeatures("tnum").TextColor(delayColor(p, n.Delay, n.Tested))
		}).OnClick(func() {
			if !chosen {
				choose(name)
			}
		})
	}
	panelNodeRow(c, p, "all", false, func() {
		ui.Text(c, i18n.T("全部节点…", "All nodes…")).FontSize(12.5).TextColor(p.ink2).Grow(1)
		ui.Text(c, fmt.Sprintf(i18n.T("%d 个", "%d"), len(pickable))).FontSize(11.5).FontFeatures("tnum").TextColor(p.mute)
	}).OnClick(func() {
		u.panelShow(func() {
			u.page = "home"
			u.home.x, u.home.sub, u.home.query, u.home.region = "routes", r.ID, "", ""
		})
	})
}

func panelNodeRow(c *ui.Context, p *pal, key string, checked bool, content func()) ui.Element {
	b := ui.ButtonBase(c.Key(key)).Row().Gap(8).Padding(5, 10, 5, 6).Radius(p.r(6)).AlignItems(ui.Center)
	if b.Hovered() {
		b.Background(p.hair2)
	}
	return b.Children(func() {
		mark := ""
		if checked {
			mark = "✓"
		}
		ui.Text(c, mark).FontSize(11.5).FontWeight(700).TextColor(p.ink).Width(12)
		content()
	})
}

// panelModeWord is a mode's short name; panelModeName its name in full.
func panelModeWord(m string) string {
	switch m {
	case "rule":
		return i18n.T("规则", "Rule")
	case "global":
		return i18n.T("全局", "Global")
	case "direct":
		return i18n.T("直连", "Direct")
	}
	return m
}

func panelModeName(m string) string {
	return fmt.Sprintf(i18n.T("%s模式", "%s mode"), panelModeWord(m))
}

// panelMode: the three modes in one segmented control.
func (u *UI) panelMode(c *ui.Context, p *pal) {
	s := u.snap
	ui.Row(c).Padding(10, 16).AlignItems(ui.Center).Children(func() {
		ui.Text(c, i18n.T("模式", "Mode")).FontSize(12.5).FontWeight(500).TextColor(p.ink2)
		ui.Spacer(c)
		ui.Row(c).Gap(2).Padding(2).Radius(p.r(8)).Background(p.hair2).Children(func() {
			for _, m := range []string{"rule", "global", "direct"} {
				on := s.Mode == m
				b := ui.ButtonBase(c.Key("mode-"+m)).Padding(3, 13).Radius(p.r(6)).Label(panelModeName(m))
				if on {
					b.Background(p.card).Shadow(0, 1, 2, 0, ui.RGBA(0, 0, 0, .12))
				}
				b.OnClick(func() {
					if !on && u.app != nil {
						u.do(func() error { return u.app.setMode(m) })
					}
				}).Children(func() {
					w, col := 500, p.ink2
					if on {
						w, col = 650, p.ink
						if u.pending("mode") {
							col = p.mute
						}
					}
					ui.Text(c, panelModeWord(m)).FontSize(12.5).FontWeight(w).TextColor(col)
				})
			}
		})
	})
}

// panelCmd is a command row at the bottom, with its shortcut.
func panelCmd(c *ui.Context, p *pal, key, label, kbd string) ui.Element {
	b := ui.ButtonBase(c.Key(key)).Row().Padding(6, 10).Margin(0, 6).Radius(p.r(7)).AlignItems(ui.Center).Label(label)
	if b.Hovered() {
		b.Background(p.hair2)
	}
	return b.Children(func() {
		ui.Text(c, label).FontSize(13).TextColor(p.ink).Grow(1)
		ui.Text(c, kbd).FontSize(12).TextColor(p.mute)
	})
}

func panelRule(c *ui.Context, p *pal) {
	ui.Box(c).Height(1).Margin(4, 16).Background(p.hair2)
}
