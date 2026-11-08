package main

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"hash/fnv"
	"log"
	"maps"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"runtime"
	"slices"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"
	"unicode"

	"github.com/egoist/mygo"
	"github.com/egoist/mygo/netproxy"
	"github.com/egoist/mygo/ui"

	"github.com/oboo/moho/internal/core"
	"github.com/oboo/moho/internal/gateway"
	"github.com/oboo/moho/internal/geo"
	"github.com/oboo/moho/internal/i18n"
	"github.com/oboo/moho/internal/locate"
	"github.com/oboo/moho/internal/mihomo"
	"github.com/oboo/moho/internal/monitor"
	"github.com/oboo/moho/internal/procinfo"
	"github.com/oboo/moho/internal/provider"
	"github.com/oboo/moho/internal/routes"
	"github.com/oboo/moho/internal/settings"
	"github.com/oboo/moho/internal/sysproxy"
	"github.com/oboo/moho/internal/tun"
)

// App ties the core, the stores, the monitors and the window together.
type App struct {
	dataDir   string
	settings  *settings.Store
	providers *provider.Store
	routes    []routes.Route
	core      *core.Manager
	mon       *monitor.Monitor
	loc       *locate.Locator
	http      *http.Client
	coreBin   string // the bundled core
	tunBin    string // the setuid copy TUN runs
	probePort int
	gw        *gateway.Gateway

	setMu sync.Mutex // serialises updateSettings
	// openAtLogin is whether Moho is a login item, as last read.
	openAtLogin bool
	mu          sync.Mutex
	pending     map[string]bool
	locating    map[string]bool
	sys         sysproxy.Status
	proxies     map[string]mihomo.Proxy
	provNodes   map[string][]string // provider id → node names, in the provider's order
	// nodesSig is the names /proxies had when provNodes was read, at
	// nodesAt: the providers' lists are read again when they change.
	nodesSig   uint64
	nodesAt    time.Time
	you        *locate.Result
	lastNode   map[string]string // route id → node, to notice automatic switches
	switched   *switchNote
	shareDay   string
	routeBytes map[string]int64 // today's download per route id
	groupSeen  map[string]int64 // the monitor's GroupBytes when last added in
	monCancel  context.CancelFunc
	lastHash   [32]byte
	emitQueued bool
	win        *mygo.Window
	ui         *UI
	// winShown is whether the main window is shown, kept from its events
	// for goroutines that may not wait on the main thread to ask.
	winShown atomic.Bool
	tray     *mygo.Tray
	quitting bool
	lastTick Tick
	// The gateway as last measured, and what its last change said.
	gwProbe   gwProbe
	gwProbing bool
	gwErr     error
}

type switchNote struct {
	route, from, to string // route is the route's ID
	at              time.Time
}

func newApp() (*App, error) {
	data, err := mygo.App.Path(mygo.PathUserData)
	if err != nil {
		return nil, err
	}
	migrateData(data)
	st, err := settings.Open(data)
	if err != nil {
		return nil, err
	}
	i18n.Set(st.Get().Language)
	ps, err := provider.Open(filepath.Join(data, "providers"))
	if err != nil {
		return nil, err
	}
	bin := bundledCore()
	tunBin := filepath.Join(data, "bin", filepath.Base(bin))
	run := bin
	if tun.Installed(tunBin) {
		run = tunBin
	}
	s := st.Get()
	var cm *core.Manager
	if s.CoreSource == "external" {
		cm = core.NewExternal(s.CoreAddr, s.CoreSecret)
	} else if cm, err = core.New(run, filepath.Join(data, "core")); err != nil {
		return nil, err
	}
	probe, err := core.FreePort()
	if err != nil {
		return nil, err
	}
	a := &App{
		dataDir: data, settings: st, providers: ps, routes: routes.Template(), core: cm,
		mon:  monitor.New(filepath.Join(data, "stats.json")),
		loc:  locate.New(filepath.Join(data, "locations.json")),
		http: &http.Client{Transport: netproxy.Transport()}, coreBin: bin, tunBin: tunBin, probePort: probe,
		pending: map[string]bool{}, locating: map[string]bool{}, proxies: map[string]mihomo.Proxy{},
		provNodes: map[string][]string{}, lastNode: map[string]string{}, routeBytes: map[string]int64{},
		gw: gateway.New(osGateway(), filepath.Join(data, "gateway-before.json")),
	}
	cm.SafePaths = []string{filepath.Join(data, "providers")}
	a.mon.SkipGroup = core.ProbeGroup // the location probe's own traffic
	a.mon.FindProcess = procinfo.New().Find
	a.mon.Every = a.pollEvery
	cm.OnStatus = func(s core.Status) { a.onCoreStatus(s) }
	a.mon.OnTick(a.onTick)
	return a, nil
}

// bundledCore finds mihomo: $MOHO_CORE, else bin/mihomo among the resources.
func bundledCore() string {
	if p := os.Getenv("MOHO_CORE"); p != "" {
		return p
	}
	name := "mihomo"
	if runtime.GOOS == "windows" {
		name += ".exe"
	}
	dir, _ := mygo.App.Path(mygo.PathResources)
	if p := filepath.Join(dir, "bin", name); exists(p) {
		return p
	}
	return filepath.Join(dir, runtime.GOOS+"-"+runtime.GOARCH, "bin", name)
}

// registerFonts adds the bundled interface face (ui.go's sans), shipped
// unmodified with its license, as the OPPO Sans license requires.
func registerFonts() {
	res, _ := mygo.App.Path(mygo.PathResources)
	b, err := os.ReadFile(filepath.Join(res, "fonts", "OPPOSans4.0.ttf"))
	if err == nil {
		err = ui.RegisterFont(b, "")
	}
	if err != nil {
		log.Println("font:", err)
	}
}

func exists(p string) bool { _, err := os.Stat(p); return err == nil }

// copyGeo puts the bundled rule data next to the core's config once.
func (a *App) copyGeo() {
	res, _ := mygo.App.Path(mygo.PathResources)
	for _, f := range []string{"GeoSite.dat", "GeoIP.dat"} {
		dst := filepath.Join(a.core.Dir, f)
		if exists(dst) {
			continue
		}
		if b, err := os.ReadFile(filepath.Join(res, "geo", f)); err == nil {
			_ = os.WriteFile(dst, b, 0o644)
		}
	}
}

// ---- Lifecycle ----

func (a *App) start() {
	// Here, not in newApp: SetSource waits for the main thread, which a
	// test's goroutine has no loop on.
	mygo.Theme.SetSource(mygo.ThemeSource(appearanceOf(a.settings.Get())))
	registerFonts()
	a.openAtLogin = mygo.App.OpenAtLogin()
	a.openWindow()
	a.setupTray()
	if !a.core.External() {
		a.copyGeo()
	}
	go func() {
		if err := a.startCore(); err != nil {
			log.Println("core:", err)
		}
	}()
	go a.loop()
	go a.debugDriver()
	a.startMCP()
}

// startCore brings the core up: it starts the built-in one with the
// current config, or attaches to the external one, whose config is not
// ours to write.
func (a *App) startCore() error {
	if a.core.External() {
		return a.core.Connect()
	}
	return a.applyConfig()
}

func (a *App) openWindow() {
	u := newUI(a)
	win := mygo.NewWindow(mygo.WindowOptions{
		Title: "bowel", Width: 1060, Height: 760, MinWidth: 960, MinHeight: 660,
		TitleBarStyle: mygo.TitleBarHidden, StateKey: "main",
		// The title bar is then as tall as the controls plus twice Y, and
		// they sit in its middle: with 14pt buttons, 50, as ui.go's
		// titleBar, so they line up with the tabs.
		TrafficLightPosition: &mygo.Point{X: 18, Y: 18},
		BackgroundColor:      "light-dark(#f6f4ef, #171615)",
		Content:              ui.View(u.view),
		// Opened at login, Moho can stay in the menu bar.
		Hidden: a.settings.Get().StartHidden && mygo.App.WasOpenedAtLogin(),
	})
	u.win = win
	a.mu.Lock()
	a.ui = u
	a.mu.Unlock()
	u.setSnapshot(a.snapshot())
	win.OnClose(func(e *mygo.CloseEvent) {
		a.mu.Lock()
		q := a.quitting
		a.mu.Unlock()
		if !q { // closing keeps Moho in the menu bar
			e.PreventDefault()
			win.Hide()
		}
	})
	// The Dock icon is there while a window is: hidden, Moho lives in
	// the menu bar alone.
	win.OnShow(func() { a.winShown.Store(true); a.mon.Wake(); a.syncDock() })
	win.OnHide(func() { a.winShown.Store(false); a.syncDock() })
	a.winShown.Store(!(a.settings.Get().StartHidden && mygo.App.WasOpenedAtLogin()))
	a.mu.Lock()
	a.win = win
	a.mu.Unlock()
	a.syncDock()
}

// syncDock shows the Dock icon while the main or settings window is
// open, and hides it otherwise. On the main thread.
func (a *App) syncDock() {
	if runtime.GOOS != "darwin" {
		return
	}
	a.mu.Lock()
	win, u := a.win, a.ui
	a.mu.Unlock()
	open := win != nil && win.IsVisible()
	if u != nil && u.set.win.Load() != nil {
		open = true
	}
	want := mygo.ActivationPolicyAccessory
	if open {
		want = mygo.ActivationPolicyRegular
	}
	if mygo.App.ActivationPolicy() != want {
		mygo.App.SetActivationPolicy(want)
	}
}

func (a *App) showWindow() {
	a.mu.Lock()
	win := a.win
	a.mu.Unlock()
	// Back in the Dock before the window shows, so it comes to the front
	// with the menu bar.
	if runtime.GOOS == "darwin" {
		mygo.App.Dock.Show()
	}
	if win != nil {
		win.Show()
		win.Focus()
	}
	mygo.App.Focus()
}

func (a *App) uiRef() *UI {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.ui
}

func (a *App) shutdown() {
	a.mu.Lock()
	a.quitting = true
	a.mu.Unlock()
	stopMCP()
	// Other devices lose the network now: forwarding goes back as it was
	// before the gateway, and the Mac may sleep again.
	if err := a.gw.Disable(); err != nil {
		log.Println("gateway:", err)
	}
	// Never leave the system pointing at a proxy that is gone: put back
	// what it had before Moho took it over.
	port := a.settings.Get().MixedPort
	if s, err := sysproxy.Check(port); err == nil {
		if s.Ours {
			if err := a.disableProxy(port); err != nil {
				log.Println("system proxy:", err)
			}
		} else {
			a.dropProxyBackup() // another program has the proxy now
		}
	}
	a.core.Stop()
}

// pollEvery is how often the monitor reads the core's connections: every
// second while the window or the menu bar panel shows them, every 5 s
// otherwise. The totals stay exact, as they are the core's own counters;
// what goes is the rates' detail and connections too short to be seen.
func (a *App) pollEvery() time.Duration {
	if a.winShown.Load() {
		return time.Second
	}
	if u := a.uiRef(); u != nil && u.panel.win.Load() != nil {
		return time.Second
	}
	return 5 * time.Second
}

// nodeSig tells apart the sets of names in px.
func nodeSig(px map[string]mihomo.Proxy) uint64 {
	names := slices.Sorted(maps.Keys(px))
	h := fnv.New64a()
	for _, n := range names {
		h.Write([]byte(n))
		h.Write([]byte{0})
	}
	return h.Sum64()
}

// loop refreshes what the core and the OS say, and runs the periodic jobs.
func (a *App) loop() {
	fast := time.NewTicker(2 * time.Second)
	slow := time.NewTicker(10 * time.Minute)
	defer fast.Stop()
	defer slow.Stop()
	a.refresh()
	for {
		select {
		case <-fast.C:
			a.refresh()
		case <-slow.C:
			a.autoUpdate()
			a.locateExits(false)
		}
	}
}

func (a *App) refresh() {
	a.probeSoon()
	// The system proxy is judged only while it is wanted: otherwise no
	// scutil every 2 s for nothing.
	if st := a.settings.Get(); a.takeoverOn(st) && st.SystemProxy {
		if s, err := sysproxy.Check(st.MixedPort); err == nil {
			a.mu.Lock()
			a.sys = s
			a.mu.Unlock()
		}
	}
	if c, err := a.core.Client(); err == nil {
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		px, err := c.Proxies(ctx)
		// The providers' lists hold the same nodes again, in their order:
		// read them only when the names change, or a minute on.
		sig := nodeSig(px)
		a.mu.Lock()
		nodes, stale := a.provNodes, sig != a.nodesSig || time.Since(a.nodesAt) > time.Minute
		a.mu.Unlock()
		if err == nil && stale {
			var pp map[string]mihomo.ProxyProvider
			if pp, err = c.ProxyProviders(ctx); err == nil {
				nodes = map[string][]string{}
				for name, p := range pp {
					id, ok := strings.CutPrefix(name, "p-")
					if !ok {
						continue
					}
					for _, n := range p.Proxies {
						nodes[id] = append(nodes[id], n.Name)
					}
				}
			}
		}
		cancel()
		if err == nil {
			a.mu.Lock()
			a.proxies, a.provNodes = px, nodes
			if stale {
				a.nodesSig, a.nodesAt = sig, time.Now()
			}
			a.mu.Unlock()
			a.noticeSwitches()
		}
	}
	a.emit()
}

func (a *App) onCoreStatus(s core.Status) {
	a.mu.Lock()
	if a.monCancel != nil {
		a.monCancel()
		a.monCancel = nil
	}
	a.mu.Unlock()
	if s.State == core.Running {
		if c, err := a.core.Client(); err == nil {
			ctx, cancel := context.WithCancel(context.Background())
			a.mu.Lock()
			a.monCancel = cancel
			a.mu.Unlock()
			go a.mon.Run(ctx, c)
		}
		go func() {
			a.applySystemProxy()
			a.refresh()
			a.locateExits(true)
		}()
	}
	a.emit()
}

// ---- Config ----

// An external core is not ours to point the system at: it takes over
// by itself (a sing-box with a TUN inbound, say).
func (a *App) takeoverOn(s settings.Settings) bool {
	return !a.core.External() && !s.Paused && (s.SystemProxy || s.Tun)
}

// applyConfig rebuilds the runtime config and makes the core use it.
func (a *App) applyConfig() error {
	s := a.settings.Get()
	var refs []core.ProviderRef
	for _, p := range a.providers.List() {
		if p.Enabled {
			refs = append(refs, core.ProviderRef{ID: p.ID, Path: a.providers.Path(p.ID)})
		}
	}
	cfg, err := core.BuildConfig(core.Input{
		Providers: refs, Routes: a.routes, UserRules: s.Rules, Mode: s.Mode, MixedPort: s.MixedPort, AllowLan: s.AllowLan,
		Tun:      s.Tun && a.takeoverOn(s) && tun.Installed(a.tunBin),
		Gateway:  a.gatewayOn(s),
		LogLevel: s.LogLevel, TestURL: s.TestURL, ProbePort: a.probePort, Controller: a.core.ControllerFields(),
		Options: s.Options,
	})
	if err != nil {
		return err
	}
	if err := a.core.Apply(cfg); err != nil {
		a.emit()
		return err
	}
	if s.Mode == "global" {
		a.pointGlobal()
	}
	a.mu.Lock()
	a.nodesAt = time.Time{} // providers may hold other nodes now
	a.mu.Unlock()
	a.syncGateway()
	a.refresh()
	return nil
}

// pointGlobal makes global mode use the default route.
func (a *App) pointGlobal() {
	if c, err := a.core.Client(); err == nil {
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		_ = c.Select(ctx, "GLOBAL", routes.Group(a.route(routes.Default)))
	}
}

// osProxy is what changes the system proxy. Tests replace it, so that they
// can never change the machine's settings.
var osProxy = struct {
	enable   func(host string, port int, bypass []string) error
	disable  func() error
	snapshot func() (sysproxy.Saved, error)
	restore  func(sysproxy.Saved) error
}{sysproxy.Enable, sysproxy.Disable, sysproxy.Snapshot, sysproxy.Restore}

// applySystemProxy makes the OS match the settings. prev are mixed ports
// the system may still point at, as after the port changed.
func (a *App) applySystemProxy(prev ...int) {
	s := a.settings.Get()
	want := a.takeoverOn(s) && s.SystemProxy
	cur, err := sysproxy.Check(s.MixedPort)
	if err != nil {
		return
	}
	switch {
	case want && !cur.Ours:
		if err := a.enableProxy(s, prev...); err != nil {
			log.Println("system proxy:", err)
		}
	case !want && cur.Ours:
		if err := a.disableProxy(s.MixedPort); err != nil {
			log.Println("system proxy:", err)
		}
	case !want:
		a.dropProxyBackup() // left by a crash, and the proxy has moved on since
	}
}

// proxyBackup is the system proxy as it was before Moho took it over. It is
// kept on disk, so that a crash does not lose it.
func (a *App) proxyBackup() string { return filepath.Join(a.dataDir, "sysproxy-before.json") }

// enableProxy points the system proxy at the core. The first time, it
// keeps what the system had, for disableProxy to put back; while the
// system still points at Moho (s's port, or one of prev), it keeps the
// first copy.
func (a *App) enableProxy(s settings.Settings, prev ...int) error {
	ports := append([]int{s.MixedPort}, prev...)
	keep := false
	if exists(a.proxyBackup()) {
		for _, p := range ports {
			if cur, err := sysproxy.Check(p); err == nil && cur.Ours {
				keep = true
			}
		}
	}
	if !keep {
		saved, err := osProxy.snapshot()
		if err != nil {
			return fmt.Errorf("reading the system proxy: %w", err)
		}
		saved.Forget(ports...)
		b, err := json.MarshalIndent(saved, "", "  ")
		if err != nil {
			return err
		}
		if err := os.WriteFile(a.proxyBackup(), b, 0o600); err != nil {
			return err
		}
	}
	return osProxy.enable("127.0.0.1", s.MixedPort, s.Bypass)
}

// disableProxy puts back the system proxy enableProxy kept, or, with no
// copy kept, turns the proxies off.
func (a *App) disableProxy(port int) error {
	b, err := os.ReadFile(a.proxyBackup())
	var saved sysproxy.Saved
	if err != nil || json.Unmarshal(b, &saved) != nil {
		return osProxy.disable()
	}
	if err := osProxy.restore(saved); err != nil {
		// Never leave it pointing at Moho, whatever else failed.
		if cur, cerr := sysproxy.Check(port); cerr == nil && cur.Ours {
			_ = osProxy.disable()
		}
		a.dropProxyBackup()
		return err
	}
	a.dropProxyBackup()
	return nil
}

func (a *App) dropProxyBackup() { _ = os.Remove(a.proxyBackup()) }

// ---- Pending (loading states) ----

func (a *App) begin(key string) func() {
	a.mu.Lock()
	a.pending[key] = true
	a.mu.Unlock()
	a.emit()
	return func() {
		a.mu.Lock()
		delete(a.pending, key)
		a.mu.Unlock()
		a.emit()
	}
}

func (a *App) busy(key string) bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.pending[key]
}

// ---- Actions ----

func (a *App) setTakeover(on bool) error {
	done := a.begin("takeover")
	defer done()
	_, err := a.settings.Update(func(s *settings.Settings) {
		s.Paused = !on
		if on && !s.SystemProxy && !s.Tun {
			s.SystemProxy = true
		}
	})
	if err != nil {
		return err
	}
	return a.afterTakeover()
}

func (a *App) setSystemProxy(on bool) error {
	done := a.begin("entry:sys")
	defer done()
	if _, err := a.settings.Update(func(s *settings.Settings) {
		s.SystemProxy = on
		if on {
			s.Paused = false
		}
	}); err != nil {
		return err
	}
	return a.afterTakeover()
}

func (a *App) setTun(on bool) error {
	done := a.begin("entry:tun")
	defer done()
	if on && !tun.Installed(a.tunBin) {
		if err := tun.Install(a.coreBin, a.tunBin); err != nil {
			return err
		}
		// The core must be the setuid copy from now on.
		a.core.Stop()
		a.core.SetBin(a.tunBin)
	}
	if _, err := a.settings.Update(func(s *settings.Settings) {
		s.Tun = on
		if on {
			s.Paused = false
		}
	}); err != nil {
		return err
	}
	return a.afterTakeover()
}

func (a *App) afterTakeover() error {
	err := a.applyConfig()
	a.applySystemProxy()
	a.refresh()
	return err
}

func (a *App) setLan(on bool) error {
	done := a.begin("lan")
	defer done()
	if _, err := a.settings.Update(func(s *settings.Settings) { s.AllowLan = on }); err != nil {
		return err
	}
	return a.applyConfig()
}

func (a *App) setMode(mode string) error {
	switch mode {
	case "rule", "global", "direct":
	default:
		return errors.New(i18n.T("未知模式: ", "Unknown mode: ") + mode)
	}
	done := a.begin("mode")
	defer done()
	if _, err := a.settings.Update(func(s *settings.Settings) { s.Mode = mode }); err != nil {
		return err
	}
	c, err := a.core.Client()
	if err != nil {
		return nil // applied at the next start
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := c.Patch(ctx, map[string]any{"mode": mode}); err != nil {
		return err
	}
	if mode == "global" {
		a.pointGlobal()
	}
	_ = c.CloseAllConnections(ctx) // old connections keep their old route otherwise
	a.refresh()
	return nil
}

func (a *App) retake() error {
	done := a.begin("retake")
	defer done()
	if err := a.enableProxy(a.settings.Get()); err != nil {
		return err
	}
	a.refresh()
	return nil
}

// updateSettings changes the core's options from the 设置 page: it checks
// them, saves them and reloads the core. The system proxy follows a new
// port or bypass list. On a failure nothing is kept.
// migrateData brings the data of the app's old name, Moho, to dir, the
// first time it runs as bowel ("Moho Dev" to "bowel Dev" in development):
// the settings, providers, stats and the core with its root copy for TUN.
// Renames keep the copy's setuid. What dir has already stays.
func migrateData(dir string) {
	base := filepath.Base(dir)
	if !strings.HasPrefix(base, "bowel") {
		return
	}
	if _, err := os.Stat(filepath.Join(dir, "settings.json")); err == nil {
		return // moved before, or new data of its own
	}
	old := filepath.Join(filepath.Dir(dir), "Moho"+strings.TrimPrefix(base, "bowel"))
	ents, err := os.ReadDir(old)
	if err != nil {
		return
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return
	}
	for _, e := range ents {
		to := filepath.Join(dir, e.Name())
		if _, err := os.Lstat(to); err == nil {
			continue
		}
		_ = os.Rename(filepath.Join(old, e.Name()), to)
	}
	_ = os.Remove(old) // when all of it moved
}

// appOnly says whether next differs from old only in what the core does
// not see: the language, starting hidden, the theme color, the appearance,
// the skin and the board's borders.
func appOnly(old, next settings.Settings) bool {
	next.Language, next.StartHidden, next.Accent, next.Appearance = old.Language, old.StartHidden, old.Accent, old.Appearance
	next.Skin, next.Borders = old.Skin, old.Borders
	next.MCPPolicy = old.MCPPolicy // the MCP server's, not the core's
	return reflect.DeepEqual(old, next)
}

func (a *App) updateSettings(fn func(*settings.Settings)) error {
	a.setMu.Lock() // one change at a time: each starts from the last
	defer a.setMu.Unlock()
	done := a.begin("settings")
	defer done()
	old := a.settings.Get()
	next := old.Clone()
	fn(&next)
	if err := next.Validate(); err != nil {
		return err
	}
	// A port the core does not listen on yet must be free.
	for _, p := range [][2]int{{old.MixedPort, next.MixedPort}, {old.HTTPPort, next.HTTPPort},
		{old.SocksPort, next.SocksPort}, {old.ExtPort(), next.ExtPort()}} {
		if p[1] == 0 || p[1] == p[0] {
			continue
		}
		if p[1] == a.probePort {
			return fmt.Errorf(i18n.T("端口 %d 已被 bowel 自己占用", "Port %d is taken by bowel itself"), p[1])
		}
		if slices.Contains([]int{old.MixedPort, old.HTTPPort, old.SocksPort, old.ExtPort()}, p[1]) {
			continue // the core's already: it moves between listeners on reload
		}
		l, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", p[1]))
		if err != nil {
			return fmt.Errorf(i18n.T("端口 %d 被其他程序占用", "Port %d is taken by another program"), p[1])
		}
		l.Close()
	}
	if _, err := a.settings.Update(func(s *settings.Settings) { *s = next }); err != nil {
		return err
	}
	i18n.Set(next.Language)
	if appearanceOf(next) != appearanceOf(old) {
		mygo.Theme.SetSource(mygo.ThemeSource(appearanceOf(next)))
	}
	if appOnly(old, next) { // the core does not care
		a.refresh()
		return nil
	}
	if err := a.applyConfig(); err != nil {
		// Back to what worked, so that the next start does not fail too.
		_, _ = a.settings.Update(func(s *settings.Settings) { *s = old })
		i18n.Set(old.Language)
		_ = a.applyConfig()
		return err
	}
	if next.MixedPort != old.MixedPort {
		a.applySystemProxy(old.MixedPort) // points the system at the new port
	} else if !slices.Equal(next.Bypass, old.Bypass) && a.takeoverOn(next) && next.SystemProxy {
		if cur, err := sysproxy.Check(next.MixedPort); err == nil && cur.Ours {
			if err := a.enableProxy(next); err != nil {
				return err
			}
		}
	}
	a.refresh()
	return nil
}

// resetSettings puts the core's options back to Moho's defaults.
func (a *App) resetSettings() error {
	return a.updateSettings((*settings.Settings).ResetCore)
}

// coreCall runs one controller request, with a timeout.
func (a *App) coreCall(timeout time.Duration, fn func(context.Context, *mihomo.Client) error) error {
	c, err := a.core.Client()
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	return fn(ctx, c)
}

// updateGeo asks the core to download the geo databases again. It answers
// at once and downloads in the background, then loads them.
func (a *App) updateGeo() error {
	done := a.begin("geo")
	defer done()
	return a.coreCall(2*time.Minute, func(ctx context.Context, c *mihomo.Client) error { return c.UpdateGeo(ctx) })
}

// flushDNS forgets the resolver's cache and its fake-ip mappings.
func (a *App) flushDNS() error {
	return a.coreCall(5*time.Second, func(ctx context.Context, c *mihomo.Client) error {
		if err := c.FlushDNS(ctx); err != nil {
			return err
		}
		return c.FlushFakeIP(ctx)
	})
}

// setOpenAtLogin makes Moho a login item of the user, or not.
func (a *App) setOpenAtLogin(on bool) error {
	if err := mygo.App.SetOpenAtLogin(on); err != nil {
		return err
	}
	a.mu.Lock()
	a.openAtLogin = mygo.App.OpenAtLogin()
	a.mu.Unlock()
	a.refresh()
	return nil
}

// readOpenAtLogin asks the system again: the user can change it in System
// Settings.
func (a *App) readOpenAtLogin() {
	on := mygo.App.OpenAtLogin()
	a.mu.Lock()
	changed := a.openAtLogin != on
	a.openAtLogin = on
	a.mu.Unlock()
	if changed {
		a.refresh()
	}
}

// revealConfig shows the core's runtime config in Finder.
func (a *App) revealConfig() error {
	return exec.Command("open", "-R", a.core.ConfigPath()).Run()
}

func (a *App) route(id string) routes.Route {
	for _, r := range a.routes {
		if r.ID == id {
			return r
		}
	}
	return routes.Route{}
}

func (a *App) selectNode(routeID, node string) error {
	r := a.route(routeID)
	if r.ID == "" || r.Direct {
		return errors.New(i18n.T("没有这条路由", "No such route"))
	}
	c, err := a.core.Client()
	if err != nil {
		return err
	}
	done := a.begin("node:" + routeID)
	defer done()
	target := node
	if node == "" {
		target = routes.Auto(r)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := c.Select(ctx, routes.Group(r), target); err != nil {
		return err
	}
	a.refresh()
	if node != "" {
		go a.probe(node)
	}
	return nil
}

func (a *App) testRoute(ctx context.Context, routeID string) error {
	r := a.route(routeID)
	c, err := a.core.Client()
	if err != nil {
		return err
	}
	done := a.begin("test:" + routeID)
	defer done()
	ctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	_, err = c.GroupDelay(ctx, routes.Group(r), a.settings.Get().TestURL, 5*time.Second)
	a.refresh()
	return err
}

func (a *App) locate(ctx context.Context, node string) error {
	_, err := a.probeErr(ctx, node)
	return err
}

// probe measures a node's exit in the background, showing "locating".
func (a *App) probe(node string) {
	if r := a.loc.Guess(node, display(node)); r.Source == "probe" {
		return
	}
	_, _ = a.probeErr(context.Background(), node)
}

func (a *App) probeErr(ctx context.Context, node string) (locate.Result, error) {
	// The probe is a listener that bowel adds to the core's config.
	if a.core.External() {
		return locate.Result{}, errors.New(i18n.T("外部内核无法实测出口位置", "An external core cannot measure exit locations"))
	}
	c, err := a.core.Client()
	if err != nil {
		return locate.Result{}, err
	}
	a.mu.Lock()
	a.locating[node] = true
	a.mu.Unlock()
	a.emit()
	r, err := a.loc.Probe(ctx, c, a.probePort, node)
	a.mu.Lock()
	delete(a.locating, node)
	if node == "DIRECT" && err == nil {
		a.you = &r
	}
	a.mu.Unlock()
	a.emit()
	return r, err
}

// locateExits measures the user's own place and the nodes routes use now.
func (a *App) locateExits(force bool) {
	if force || a.you == nil {
		_, _ = a.probeErr(context.Background(), "DIRECT")
	}
	a.mu.Lock()
	px := a.proxies
	a.mu.Unlock()
	seen := map[string]bool{}
	for _, r := range a.routes {
		if r.Direct {
			continue
		}
		if n := currentNode(px, r); n != "" && !seen[n] {
			seen[n] = true
			if force {
				_, _ = a.probeErr(context.Background(), n)
			} else {
				a.probe(n)
			}
		}
	}
}

// currentNode is the node a route uses now, following its automatic group.
func currentNode(px map[string]mihomo.Proxy, r routes.Route) string {
	g, ok := px[routes.Group(r)]
	if !ok {
		return ""
	}
	if g.Now == routes.Auto(r) {
		return px[routes.Auto(r)].Now
	}
	return g.Now
}

// noticeSwitches records a route's automatic group moving off a failed node.
func (a *App) noticeSwitches() {
	a.mu.Lock()
	defer a.mu.Unlock()
	for _, r := range a.routes {
		if r.Direct {
			continue
		}
		n := currentNode(a.proxies, r)
		prev := a.lastNode[r.ID]
		a.lastNode[r.ID] = n
		if prev == "" || prev == n || a.proxies[routes.Group(r)].Now != routes.Auto(r) {
			continue
		}
		if p, ok := a.proxies[prev]; ok && len(p.History) > 0 && p.History[len(p.History)-1].Delay == 0 {
			a.switched = &switchNote{route: r.ID, from: a.label(a.provNodes, prev), to: a.label(a.provNodes, n), at: time.Now()}
		}
	}
}

// ---- Providers ----

func (a *App) detectProvider(input string) (string, error) {
	k, err := provider.Detect(input)
	return string(k), err
}

func (a *App) addProvider(ctx context.Context, input string) (ProviderView, error) {
	done := a.begin("add")
	defer done()
	p, err := a.providers.Add(ctx, a.http, input)
	if err != nil {
		return ProviderView{}, err
	}
	return a.afterProviderAdded(p)
}

func (a *App) addProviderFile(ctx context.Context) (*ProviderView, error) {
	paths, err := mygo.Dialog.Open(mygo.OpenDialogOptions{
		Parent: mygo.CallerWindow(ctx), Title: i18n.T("添加 Provider", "Add Provider"),
		Filters: []mygo.FileFilter{{Name: i18n.T("Clash 配置或节点列表", "Clash config or node list"), Extensions: []string{"yaml", "yml", "txt", "conf"}}},
	})
	if err != nil || len(paths) == 0 {
		return nil, err
	}
	done := a.begin("add")
	defer done()
	p, err := a.providers.AddFile(paths[0])
	if err != nil {
		return nil, err
	}
	v, err := a.afterProviderAdded(p)
	return &v, err
}

func (a *App) afterProviderAdded(p provider.Provider) (ProviderView, error) {
	if err := a.applyConfig(); err != nil {
		_ = a.providers.Delete(p.ID)
		_ = a.applyConfig()
		return ProviderView{}, fmt.Errorf(i18n.T("内核无法读取这个 Provider：%w", "The core can't read this Provider: %w"), err)
	}
	go a.healthCheck(p.ID)
	return a.providerView(p), nil
}

// healthCheck tests a provider's nodes and locates the ones in use.
func (a *App) healthCheck(id string) {
	if c, err := a.core.Client(); err == nil {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		_ = c.HealthCheck(ctx, "p-"+id)
		cancel()
	}
	a.refresh()
	a.locateExits(false)
}

func (a *App) updateProvider(ctx context.Context, id string) error {
	done := a.begin("provider:" + id)
	defer done()
	if _, err := a.providers.Update(ctx, a.http, id); err != nil {
		a.emit()
		return err
	}
	err := a.applyConfig()
	go a.healthCheck(id)
	return err
}

func (a *App) updateAll(ctx context.Context) error {
	done := a.begin("providers")
	defer done()
	var errs []error
	for _, p := range a.providers.List() {
		if p.Kind == provider.Subscription && p.Enabled {
			if _, err := a.providers.Update(ctx, a.http, p.ID); err != nil {
				errs = append(errs, fmt.Errorf("%s: %w", p.Name, err))
			}
		}
	}
	if err := a.applyConfig(); err != nil {
		errs = append(errs, err)
	}
	return errors.Join(errs...)
}

func (a *App) autoUpdate() {
	ids := a.providers.Due(time.Now())
	for _, id := range ids {
		if _, err := a.providers.Update(context.Background(), a.http, id); err != nil {
			log.Println("auto update:", err)
		}
	}
	if len(ids) > 0 {
		_ = a.applyConfig()
	}
}

func (a *App) setProviderEnabled(id string, on bool) error {
	done := a.begin("provider:" + id)
	defer done()
	if _, err := a.providers.SetEnabled(id, on); err != nil {
		return err
	}
	return a.applyConfig()
}

func (a *App) renameProvider(id, name string) error {
	_, err := a.providers.Rename(id, name)
	a.emit()
	return err
}

func (a *App) setAutoUpdate(id string, hours int) error {
	_, err := a.providers.SetAutoUpdate(id, hours)
	a.emit()
	return err
}

func (a *App) deleteProvider(id string) error {
	done := a.begin("provider:" + id)
	defer done()
	if err := a.providers.Delete(id); err != nil {
		return err
	}
	return a.applyConfig()
}

// ---- The user's rules ----

// addRule sends r's traffic to its route, in place of an older rule for
// the same traffic. Connections it matches that went elsewhere are closed,
// so that the change shows at once: their apps connect again, by the new
// way. It returns how many were closed.
func (a *App) addRule(r routes.UserRule) (int, error) {
	if a.route(r.Route).ID == "" {
		return 0, errors.New(i18n.T("没有这条路由", "No such route"))
	}
	if err := a.updateSettings(func(s *settings.Settings) { s.Rules = routes.Upsert(s.Rules, r) }); err != nil {
		return 0, err
	}
	return a.reroute(r, r.Route), nil
}

// removeRule takes out the rule for r's traffic, and closes the
// connections it matched.
func (a *App) removeRule(r routes.UserRule) (int, error) {
	if err := a.updateSettings(func(s *settings.Settings) { s.Rules = routes.Remove(s.Rules, r) }); err != nil {
		return 0, err
	}
	return a.reroute(r, ""), nil
}

// reroute closes the open connections r matches that are not on route
// keep. A failure only leaves them open: the rule holds for new ones.
func (a *App) reroute(r routes.UserRule, keep string) int {
	c, err := a.core.Client()
	if err != nil {
		return 0
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	snap, err := c.Connections(ctx)
	if err != nil {
		return 0
	}
	var ids []string
	for _, cn := range snap.Connections {
		host := cn.Metadata.Host
		if host == "" {
			host = cn.Metadata.SniffHost
		}
		if !r.Matches(host, cn.Metadata.DestinationIP) {
			continue
		}
		group := ""
		if len(cn.Chains) > 0 {
			group = cn.Chains[len(cn.Chains)-1]
		}
		if keep == "" || a.routeOfGroup(group, cn.Rule) != keep {
			ids = append(ids, cn.ID)
		}
	}
	if len(ids) > 0 && a.closeConns(ids) == nil {
		a.mon.Wake()
		return len(ids)
	}
	return 0
}

// ---- Activity ----

// routeOfGroup maps a mihomo group name (or DIRECT) to a route id.
func (a *App) routeOfGroup(group, rule string) string {
	for _, r := range a.routes {
		if !r.Direct && (group == routes.Group(r) || group == routes.Auto(r)) {
			return r.ID
		}
	}
	if group == "DIRECT" && rule != "" && rule != "Match" {
		for _, r := range a.routes {
			if r.Direct {
				return r.ID
			}
		}
	}
	return ""
}

// display is a node's name without Moho's provider marker.
func display(node string) string {
	if i := strings.Index(node, core.NodeSep); i >= 0 {
		return node[:i]
	}
	return node
}

// label is what people read for a node: its own name, but a node that is
// all of a pasted link provider goes by the provider's name. That name was
// taken from the link's #fragment, so renaming the provider renames the node
// for people; mihomo keeps the node's name, and with it the choices made.
// provNodes is the providers' nodes, as a.provNodes.
func (a *App) label(provNodes map[string][]string, node string) string {
	id := providerOf(node)
	if len(provNodes[id]) == 1 {
		if p, ok := a.providers.Get(id); ok && p.Kind == provider.Link {
			return trimFlag(p.Name)
		}
	}
	return trimFlag(display(node))
}

// trimFlag drops a flag emoji leading a name ("🇺🇸美国纽约" is "美国纽约"):
// the interface draws the place's flag before the name itself. A name that
// is nothing but a flag stays.
func trimFlag(s string) string {
	t := strings.TrimLeftFunc(s, func(r rune) bool {
		return r >= 0x1F1E6 && r <= 0x1F1FF || unicode.IsSpace(r)
	})
	if t == "" {
		return s
	}
	return t
}

// display is a node's label; not with a.mu held.
func (a *App) display(node string) string {
	a.mu.Lock()
	pn := a.provNodes
	a.mu.Unlock()
	return a.label(pn, node)
}

func providerOf(node string) string {
	if i := strings.Index(node, core.NodeSep); i >= 0 {
		return node[i+len(core.NodeSep):]
	}
	return ""
}

// groupTitle is a chain's step as the interface shows it: a route's group
// by the route's title, anything else (a node, DIRECT) as it is.
func (a *App) groupTitle(name string) string {
	for _, r := range a.routes {
		switch name {
		case routes.Group(r):
			return routes.Title(r)
		case routes.Auto(r):
			return routes.Title(r) + i18n.T("·自动", " · Auto")
		}
	}
	return name
}

func (a *App) convConn(c monitor.Conn) Conn {
	group := ""
	if len(c.Chains) > 0 {
		group = c.Chains[len(c.Chains)-1]
	}
	v := Conn{
		ID: c.ID, App: c.App, Process: c.Process, Host: c.Host, IP: c.IP, Port: c.Port, Network: c.Network,
		Rule: c.Rule, Payload: c.Payload, Route: a.routeOfGroup(group, c.Rule),
		Down: c.Down, Up: c.Up, DownRate: c.DownRate, UpRate: c.UpRate, Start: c.Start, End: c.End,
		Status: c.Status, Error: c.Error,
	}
	for i := len(c.Chains) - 1; i >= 0; i-- {
		v.Chain = append(v.Chain, a.groupTitle(a.display(c.Chains[i])))
	}
	if len(c.Chains) > 1 || (len(c.Chains) == 1 && c.Chains[0] != "DIRECT" && group != c.Chains[0]) {
		v.Node = a.display(c.Chains[0])
	}
	return v
}

func (a *App) connections() []Conn {
	src := a.mon.Conns()
	out := make([]Conn, 0, len(src))
	for _, c := range src {
		out = append(out, a.convConn(c))
	}
	return out
}

func (a *App) closeConns(ids []string) error {
	c, err := a.core.Client()
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	var errs []error
	for _, id := range ids {
		if err := c.CloseConnection(ctx, id); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

func (a *App) logs(after int64) []LogLine {
	src := a.mon.Logs(after)
	out := make([]LogLine, len(src))
	for i, l := range src {
		out[i] = LogLine{Seq: l.Seq, Time: l.Time, Level: l.Level, Text: l.Text}
	}
	return out
}

// ---- Tick ----

func (a *App) onTick() {
	t := a.buildTick()
	a.mu.Lock()
	a.lastTick = t
	a.mu.Unlock()
	if u := a.uiRef(); u != nil {
		u.setTick(t)
	}
	a.trayTick()
}

func (a *App) tick() Tick {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.lastTick
}

func (a *App) buildTick() Tick {
	n := a.mon.Now()
	t := Tick{Down: n.Down, Up: n.Up, ByRoute: map[string]int64{}, Active: n.Active,
		TodayDown: n.TodayDown, TodayUp: n.TodayUp, MonthDown: n.MonthDown, Apps: []AppRate{}, Events: []Event{}}
	for g, v := range n.ByGroup {
		// A DIRECT group can be the direct route or the default's DIRECT; count it as direct.
		id := a.routeOfGroup(g, "x")
		if id != "" {
			t.ByRoute[id] += v
		}
	}
	// Today's share per route: the bytes since the last tick, as the
	// monitor counted them, however long ago that was.
	day := time.Now().Format("2006-01-02")
	a.mu.Lock()
	if a.shareDay != day {
		a.shareDay, a.routeBytes = day, map[string]int64{}
	}
	for g, v := range n.GroupBytes {
		if d := v - a.groupSeen[g]; d > 0 {
			if id := a.routeOfGroup(g, "x"); id != "" {
				a.routeBytes[id] += d
			}
		}
	}
	a.groupSeen = n.GroupBytes
	a.mu.Unlock()

	// Each app's busiest connection names its host: the fastest, the
	// oldest of equals, so that it holds still from one tick to the next.
	type top struct {
		rate  int64
		start time.Time
		id    string
	}
	apps := map[string]*AppRate{}
	best := map[string]top{}
	for _, c := range a.mon.Rates() {
		r := apps[c.App]
		if r == nil {
			r = &AppRate{App: c.App}
			apps[c.App] = r
		}
		r.Down += c.DownRate
		r.Up += c.UpRate
		r.Conns++
		b, seen := best[c.App]
		if !seen || c.DownRate > b.rate || c.DownRate == b.rate &&
			(c.Start.Before(b.start) || c.Start.Equal(b.start) && c.ID > b.id) {
			best[c.App] = top{c.DownRate, c.Start, c.ID}
			r.Host, r.Route = c.Host, a.routeOfGroup(c.Group, c.Rule)
		}
	}
	for _, r := range apps {
		t.Apps = append(t.Apps, *r)
	}
	sort.Slice(t.Apps, func(i, j int) bool {
		if t.Apps[i].Down != t.Apps[j].Down {
			return t.Apps[i].Down > t.Apps[j].Down
		}
		return t.Apps[i].Conns > t.Apps[j].Conns
	})
	if len(t.Apps) > 8 {
		t.Apps = t.Apps[:8]
	}
	for _, e := range a.mon.Events(30) {
		group := ""
		if len(e.Chains) > 0 {
			group = e.Chains[len(e.Chains)-1]
		}
		ev := Event{Time: e.Time, Text: e.App + " → " + e.Host, Route: a.routeOfGroup(group, "x"), Failed: e.Failed}
		if len(e.Chains) > 1 {
			ev.Node = a.display(e.Chains[0])
		}
		t.Events = append(t.Events, ev)
	}
	return t
}

// ---- Snapshot ----

// emit sends the snapshot when it changed, at most every 100 ms.
func (a *App) emit() {
	a.mu.Lock()
	if a.emitQueued {
		a.mu.Unlock()
		return
	}
	a.emitQueued = true
	a.mu.Unlock()
	time.AfterFunc(100*time.Millisecond, func() {
		a.mu.Lock()
		a.emitQueued = false
		a.mu.Unlock()
		s := a.snapshot()
		b, _ := json.Marshal(s)
		h := sha256.Sum256(b)
		a.mu.Lock()
		same := h == a.lastHash
		a.lastHash = h
		a.mu.Unlock()
		if !same {
			if u := a.uiRef(); u != nil {
				u.setSnapshot(s)
			}
			a.refreshTray(s)
		}
	})
}

func (a *App) snapshot() Snapshot {
	s := a.settings.Get()
	cs := a.core.Status()
	a.mu.Lock()
	sys, px, provNodes, you, sw := a.sys, a.proxies, a.provNodes, a.you, a.switched
	openAtLogin := a.openAtLogin
	gwp, gwErr := a.gwProbe, a.gwErr
	pending := make([]string, 0, len(a.pending))
	for k := range a.pending {
		pending = append(pending, k)
	}
	locating := copyMap(a.locating)
	routeBytes := copyMap(a.routeBytes)
	a.mu.Unlock()
	slices.Sort(pending)

	on := a.takeoverOn(s)
	snap := Snapshot{
		Mode: s.Mode, Pending: pending, Version: mygo.App.Version(), RuleCount: routes.Count(a.routes) + len(s.Rules),
		Settings: s, ConfigPath: a.core.ConfigPath(), OpenAtLogin: openAtLogin,
		ControllerTCP: a.core.TCPAddr(),
		Core:          Core{State: string(cs.State), Version: cs.Version, Error: cs.Error},
		Routes:        []RouteView{},
		Nodes:         []NodeView{},
		Providers:     []ProviderView{},
	}

	// Takeover, as it really is.
	tk := Takeover{On: on, Port: s.MixedPort, Lan: s.AllowLan, Bypass: s.Bypass}
	tk.System = Entry{Wanted: s.SystemProxy, Status: "off"}
	if on && s.SystemProxy {
		switch {
		case sys.Ours:
			tk.System.Status = "on"
		case sys.On:
			tk.System.Status, tk.System.Detail = "hijacked", fmt.Sprintf(i18n.T("现在指向 %s:%d", "Now points at %s:%d"), sys.Host, sys.Port)
		default:
			tk.System.Status, tk.System.Detail = "hijacked", i18n.T("系统代理被关掉了", "The system proxy was turned off")
		}
	}
	installed := tun.Installed(a.tunBin)
	tk.Tun = Entry{Wanted: s.Tun, Status: "off"}
	switch {
	case on && s.Tun && installed && cs.State == core.Running:
		tk.Tun.Status = "on"
	case on && s.Tun && installed:
		tk.Tun.Status, tk.Tun.Detail = "error", i18n.T("内核没有运行", "The core isn't running")
	case !installed:
		tk.Tun.Status = "install"
	}
	if s.AllowLan {
		tk.LanAddr = lanAddr(s.MixedPort)
	}
	tk.Gateway = gatewayView(s, on, tk.Tun.Status, a.gw.System().Ready(), a.gw.Awake(), gwp, gwErr)
	snap.Takeover = tk

	// Nodes, in their providers' order.
	view := func(name string) NodeView {
		p := px[name]
		v := NodeView{Name: name, Display: a.label(provNodes, name), Provider: providerOf(name), Type: p.Type, UDP: p.UDP, History: []int{}}
		for _, h := range p.History {
			v.History = append(v.History, h.Delay)
		}
		if len(v.History) > 12 {
			v.History = v.History[len(v.History)-12:]
		}
		if len(p.History) > 0 {
			v.Delay, v.Tested = p.History[len(p.History)-1].Delay, true
		}
		v.Where = located(a.loc.Guess(name, display(name)))
		if locating[name] {
			v.Where.Source = "locating"
		}
		return v
	}
	for _, p := range a.providers.List() {
		if !p.Enabled {
			continue
		}
		for _, n := range provNodes[p.ID] {
			snap.Nodes = append(snap.Nodes, view(n))
		}
	}

	// Routes.
	var total int64
	for _, v := range routeBytes {
		total += v
	}
	for _, r := range a.routes {
		rv := RouteView{ID: r.ID, Name: routes.Title(r), Color: r.Color, Summary: routes.Describe(r), Rules: len(r.Rules), Direct: r.Direct}
		if r.ID == routes.Default {
			rv.Rules = 1
		}
		for _, u := range s.Rules {
			if u.Route == r.ID {
				rv.Mine++
			}
		}
		rv.Rules += rv.Mine
		if !r.Direct {
			rv.Auto = px[routes.Group(r)].Now == routes.Auto(r)
			rv.Node = currentNode(px, r)
			if rv.Node == "DIRECT" {
				rv.Node = ""
			}
		}
		if total > 0 {
			rv.Share = float64(routeBytes[r.ID]) / float64(total)
		}
		snap.Routes = append(snap.Routes, rv)
	}

	// Exit and the user's place.
	if n := currentNode(px, a.route(routes.Default)); n != "" && n != "DIRECT" {
		snap.Exit = &Exit{Node: n, Where: view(n).Where}
	}
	if you != nil {
		l := located(*you)
		snap.You = &l
	}

	// Providers.
	for _, p := range a.providers.List() {
		v := a.providerView(p)
		v.Nodes = len(provNodes[p.ID])
		snap.Providers = append(snap.Providers, v)
	}

	// The one light, and the news line.
	snap.State, snap.Issue = a.judge(on, s, cs, tk, snap, sw)
	return snap
}

func (a *App) judge(on bool, s settings.Settings, cs core.Status, tk Takeover, snap Snapshot, sw *switchNote) (State, *Issue) {
	if !on {
		return StateOff, nil
	}
	if cs.State == core.Failed {
		return StateError, &Issue{Level: "error", Text: i18n.T("内核未运行", "The core isn't running"), Detail: firstLine(cs.Error)}
	}
	if tk.System.Status == "hijacked" {
		return StateError, &Issue{Level: "error", Text: i18n.T("系统代理被其他程序改掉了", "Another program changed the system proxy"), Detail: i18n.T("网页流量现在没有经过 bowel。", "Web traffic isn't going through bowel now."), Action: "retake", Label: i18n.T("重新接管", "Take over again")}
	}
	if g := tk.Gateway; g.Status == "error" {
		return StateDegraded, &Issue{Level: "warn", Text: i18n.T("网关模式没有生效", "Gateway mode isn't working"), Detail: g.Detail}
	}
	if len(snap.Nodes) == 0 && s.Mode != "direct" {
		return StateDegraded, &Issue{Level: "warn", Text: i18n.T("还没有节点，流量正在直连", "No nodes yet; traffic is going direct"), Action: "providers", Label: i18n.T("添加提供商", "Add Provider")}
	}
	if sw != nil && time.Since(sw.at) < 10*time.Minute {
		return StateDegraded, &Issue{Level: "warn", Text: fmt.Sprintf(i18n.T("%s：%s 不可用，已换到 %s", "%s: %s is down, switched to %s"), routes.Title(a.route(sw.route)), sw.from, sw.to), Action: "activity", Label: i18n.T("查看", "View")}
	}
	for _, r := range snap.Routes {
		if r.Direct || r.Node == "" {
			continue
		}
		for _, n := range snap.Nodes {
			if n.Name == r.Node && n.Tested && n.Delay == 0 && (s.Mode == "rule" || r.ID == routes.Default) {
				return StateDegraded, &Issue{Level: "warn", Text: fmt.Sprintf(i18n.T("%s：%s 测速失败", "%s: %s failed the speed test"), r.Name, n.Display), Action: "", Label: ""}
			}
		}
	}
	for _, p := range snap.Providers {
		switch p.Warn {
		case "quota":
			return StateDegraded, &Issue{Level: "warn", Text: fmt.Sprintf(i18n.T("%s 流量快用完了", "%s is almost out of data"), p.Name), Action: "providers", Label: i18n.T("查看", "View")}
		case "expire":
			return StateDegraded, &Issue{Level: "warn", Text: fmt.Sprintf(i18n.T("%s 快到期了", "%s expires soon"), p.Name), Action: "providers", Label: i18n.T("查看", "View")}
		}
	}
	return StateOK, nil
}

func (a *App) providerView(p provider.Provider) ProviderView {
	v := ProviderView{ID: p.ID, Name: p.Name, Kind: string(p.Kind), Source: maskSource(p), Enabled: p.Enabled,
		AutoUpdate: p.AutoUpdate, UpdatedAt: p.UpdatedAt, Error: p.Error}
	if u := p.Usage; u != nil {
		v.Usage = &Usage{Upload: u.Upload, Download: u.Download, Total: u.Total, Expire: u.Expire}
		if u.Total > 0 && float64(u.Upload+u.Download)/float64(u.Total) > .9 {
			v.Warn = "quota"
		} else if u.Expire > 0 && time.Until(time.Unix(u.Expire, 0)) < 7*24*time.Hour {
			v.Warn = "expire"
		}
	}
	return v
}

// maskSource hides a subscription's token: only scheme and host are shown.
func maskSource(p provider.Provider) string {
	switch p.Kind {
	case provider.Subscription:
		if u, err := url.Parse(p.Source); err == nil {
			return u.Scheme + "://" + u.Host + "/…"
		}
	case provider.File:
		return p.Source
	}
	return ""
}

func located(r locate.Result) Located {
	l := Located{Source: r.Source, IP: r.IP}
	if r.Place != nil {
		l.Place = &Place{Country: r.Place.Country, City: geo.Display(r.Place.City), Flag: r.Place.Flag, Lat: r.Place.Lat, Lon: r.Place.Lon}
	}
	return l
}

var _ = geo.Place{} // locate's places are geo.Places

func lanAddr(port int) string {
	addrs, _ := net.InterfaceAddrs()
	for _, ad := range addrs {
		if ip, ok := ad.(*net.IPNet); ok && ip.IP.To4() != nil && !ip.IP.IsLoopback() && ip.IP.IsPrivate() {
			return fmt.Sprintf("%s:%d", ip.IP, port)
		}
	}
	return ""
}

func firstLine(s string) string {
	s, _, _ = strings.Cut(s, "\n")
	return s
}

func copyMap[K comparable, V any](m map[K]V) map[K]V {
	out := make(map[K]V, len(m))
	for k, v := range m {
		out[k] = v
	}
	return out
}
