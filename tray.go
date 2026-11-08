package main

import (
	"bytes"
	"fmt"
	"image"
	"image/color"
	"image/png"
	"log"
	"math"
	"runtime"
	"sort"
	"sync"

	"github.com/egoist/mygo"
	"github.com/egoist/mygo/ui"

	"github.com/oboo/moho/internal/i18n"
)

// The menu bar (L0 of docs/product-ia.md): a template icon, bright when
// Moho has taken over and faint when it has not; nothing beside it. A click opens the
// panel (ui_panel.go) under the icon; a right click, or a Control-click,
// the menu, which takes over, switches the node and the mode without
// opening the window.

func (a *App) setupTray() {
	tray, err := mygo.NewTray(mygo.TrayOptions{ID: "moho", Icon: trayIcon(false), IconIsTemplate: true, ToolTip: "bowel"})
	if err != nil {
		log.Println("tray:", err)
		return
	}
	a.mu.Lock()
	a.tray = tray
	a.mu.Unlock()
	if runtime.GOOS == "linux" {
		// AppIndicator shows menus only and tells of no clicks: the menu
		// stays set, and refreshTray keeps it current.
		tray.SetMenu(a.trayMenu(a.snapshot()))
	} else {
		// No menu set, so that clicks reach OnClick; the menu is set for
		// its popup only.
		tray.OnClick(a.togglePanel)
		tray.OnRightClick(func() {
			if u := a.uiRef(); u != nil {
				u.panelClose()
			}
			tray.SetMenu(a.trayMenu(a.snapshot()))
			defer tray.SetMenu(nil)
			tray.PopUpMenu()
		})
	}
	mygo.App.OnWillQuit(func(*mygo.QuitEvent) { tray.Destroy() })
	a.refreshTray(a.snapshot())
}

// togglePanel opens the panel under the icon, or closes it. On the main
// thread, as tray callbacks run.
func (a *App) togglePanel() {
	a.mu.Lock()
	tray := a.tray
	a.mu.Unlock()
	u := a.uiRef()
	if tray == nil || u == nil {
		a.showWindow()
		return
	}
	if fl := u.panel.win.Load(); fl != nil {
		fl.Close()
		return
	}
	fl := mygo.NewFlyout(mygo.FlyoutOptions{
		Tray:         tray,
		Placement:    mygo.PlacementBottom,
		Gap:          4,
		Width:        panelWidth,
		Height:       480,
		FitContent:   true,
		MinWidth:     panelWidth,
		MaxWidth:     panelWidth,
		MaxHeight:    820,
		Focusable:    true, // a click elsewhere closes it
		Shadow:       true,
		CornerRadius: 12,
		Content:      ui.View(u.panelView),
	})
	u.panel.win.Store(fl)
	if u.app != nil {
		u.app.mon.Wake() // the panel shows rates: at once, and every second
	}
	fl.OnClosed(func() { u.panel.win.CompareAndSwap(fl, nil) })
}

// trayOn is the icon last drawn: bright (taken over) or faint.
var trayOn = struct {
	sync.Mutex
	set, on bool
}{}

func (a *App) refreshTray(s Snapshot) {
	a.mu.Lock()
	tray := a.tray
	a.mu.Unlock()
	if tray == nil {
		return
	}
	on := s.Takeover.On
	trayOn.Lock()
	changed := !trayOn.set || trayOn.on != on
	trayOn.set, trayOn.on = true, on
	trayOn.Unlock()
	if changed {
		_ = tray.SetIcon(trayIcon(on), true)
	}
	if runtime.GOOS == "linux" {
		tray.SetMenu(a.trayMenu(s))
	}
	// The window's Update set the snapshot; the redraw queues after it.
	if u := a.uiRef(); u != nil {
		u.panelRedraw()
	}
}

// trayTick redraws the open panel's speeds; the icon has none.
func (a *App) trayTick() {
	if u := a.uiRef(); u != nil {
		u.panelRedraw()
	}
}

func (a *App) trayMenu(s Snapshot) *mygo.Menu {
	head := stateName(s.State)
	if s.Exit != nil && s.Mode != "direct" && s.State != StateOff {
		where := ""
		if p := s.Exit.Where.Place; p != nil {
			where = p.Flag + " "
		}
		head += " · " + where + a.display(s.Exit.Node)
	}
	items := []*mygo.MenuItem{{Label: head, Disabled: true}}
	if s.Issue != nil {
		items = append(items, &mygo.MenuItem{Label: s.Issue.Text, Disabled: true})
	}
	if !s.External { // bowel takes over nothing of an external core
		items = append(items,
			&mygo.MenuItem{Label: i18n.T("接管", "Takeover"), Type: mygo.MenuItemCheckbox, Checked: s.Takeover.On, Accelerator: "CmdOrCtrl+T",
				Click: func(it *mygo.MenuItem, _ *mygo.Window) { on := it.Checked; go func() { a.logErr(a.setTakeover(on)) }() }},
			mygo.Separator(),
		)
	}

	// One node list, the default exit's, as the panel has; the window has the routes.
	for _, r := range s.Routes {
		if r.ID != "default" || r.Direct {
			continue
		}
		label := i18n.T("节点", "Node")
		for _, n := range s.Nodes {
			if n.Name == r.Node {
				label = fmt.Sprintf(i18n.T("节点", "Node")+"\t%s%s", flagOf(n), n.Display)
			}
		}
		nodes := s.Nodes
		if s.External {
			nodes = r.Members
		}
		items = append(items, &mygo.MenuItem{Label: label, Submenu: a.routeMenu(r, nodes, s.External),
			Disabled: len(s.Nodes) == 0 || !s.Takeover.On || s.Mode == "direct"})
	}
	items = append(items, mygo.Separator())
	if !s.External {
		for _, m := range [][2]string{{"rule", i18n.T("规则模式", "Rule mode")}, {"global", i18n.T("全局模式", "Global mode")}, {"direct", i18n.T("直连模式", "Direct mode")}} {
			id := m[0]
			items = append(items, &mygo.MenuItem{Label: m[1], Type: mygo.MenuItemRadio, Checked: s.Mode == id,
				Click: func(*mygo.MenuItem, *mygo.Window) { go func() { a.logErr(a.setMode(id)) }() }})
		}
		items = append(items, mygo.Separator())
	}
	items = append(items,
		&mygo.MenuItem{Label: i18n.T("打开 bowel", "Open bowel"), Accelerator: "CmdOrCtrl+O", Click: func(*mygo.MenuItem, *mygo.Window) { a.showWindow() }},
		&mygo.MenuItem{Label: i18n.T("设置…", "Settings…"), Accelerator: "CmdOrCtrl+,", Click: func(*mygo.MenuItem, *mygo.Window) {
			if u := a.uiRef(); u != nil {
				u.openSettings()
			}
		}},
		&mygo.MenuItem{Role: mygo.RoleQuit, Label: i18n.T("退出 bowel", "Quit bowel")},
	)
	return mygo.NewMenu(items)
}

// routeMenu lists automatic and the fastest nodes; the window has the rest.
// An external core's route has no Auto, and its members are its group's.
func (a *App) routeMenu(r RouteView, all []NodeView, external bool) []*mygo.MenuItem {
	rid := r.ID
	var items []*mygo.MenuItem
	if !external {
		items = []*mygo.MenuItem{{Label: i18n.T("自动（最快 + 故障转移）", "Auto (fastest + failover)"), Type: mygo.MenuItemRadio, Checked: r.Auto,
			Click: func(*mygo.MenuItem, *mygo.Window) { go func() { a.logErr(a.selectNode(rid, "")) }() }}, mygo.Separator()}
	}
	nodes := append([]NodeView(nil), all...)
	sort.SliceStable(nodes, func(i, j int) bool { return rank(nodes[i]) < rank(nodes[j]) })
	if len(nodes) > 15 {
		nodes = nodes[:15]
	}
	for _, n := range nodes {
		name := n.Name
		d := "—"
		if n.Tested {
			d = i18n.T("超时", "Timeout")
			if n.Delay > 0 {
				d = fmt.Sprintf("%d ms", n.Delay)
			}
		}
		items = append(items, &mygo.MenuItem{Label: flagOf(n) + n.Display + "\t" + d, Type: mygo.MenuItemRadio, Checked: !r.Auto && (r.Node == name && !external || external && r.Picked == name),
			Click: func(*mygo.MenuItem, *mygo.Window) { go func() { a.logErr(a.selectNode(rid, name)) }() }})
	}
	if len(all) > 15 {
		items = append(items, mygo.Separator(), &mygo.MenuItem{Label: fmt.Sprintf(i18n.T("全部 %d 个节点…", "All %d nodes…"), len(all)), Click: func(*mygo.MenuItem, *mygo.Window) { a.showWindow() }})
	}
	return items
}

func rank(n NodeView) int {
	if !n.Tested {
		return 1 << 20
	}
	if n.Delay == 0 {
		return 1 << 21
	}
	return n.Delay
}

func flagOf(n NodeView) string {
	if n.Where.Place != nil && n.Where.Place.Flag != "" && flagDrawn(n.Where.Place.Flag) {
		return n.Where.Place.Flag + " "
	}
	return ""
}

func (a *App) logErr(err error) {
	if err != nil {
		log.Println(err)
		a.refreshTray(a.snapshot()) // undo a checkbox the click toggled
	}
}

// trayIcon draws the template icon, 36×36 px: the logo's squiggle in one
// ink, full when on and faded when off. That is all it tells.
func trayIcon(on bool) []byte {
	const n = 36
	cov := make([]float64, n*n)
	// The squiggle and its stroke span 194 of the logo's 200 units; fill 34 px.
	const scale = 34.0 / (170 + logoWidth)
	const o = 18 - 100*scale
	strokeLogo(logoPoints, cov, n, n, scale, o, o, logoWidth)
	ink := 1.0
	if !on {
		ink = .4
	}
	img := image.NewNRGBA(image.Rect(0, 0, n, n))
	for y := 0; y < n; y++ {
		for x := 0; x < n; x++ {
			if c := cov[y*n+x] * ink; c > 0 {
				img.SetNRGBA(x, y, color.NRGBA{A: uint8(math.Min(1, c) * 255)})
			}
		}
	}
	var buf bytes.Buffer
	_ = png.Encode(&buf, img)
	return buf.Bytes()
}
