package main

import (
	"strings"
	"testing"
	"time"

	"github.com/egoist/mygo"
	"github.com/egoist/mygo/ui"

	"github.com/oboo/moho/internal/settings"
)

// externalUI is the fixture as an external core would report it: no
// automatic group, and routes that list their own group's members, one
// of them a group.
func externalUI() *UI {
	u := fixtureUI()
	u.snap.External = true
	n := u.snap.Nodes
	u.snap.Exit = &Exit{Node: n[3].Name, Where: n[3].Where}
	sub := NodeView{Name: "selfhost", Display: "selfhost", Type: "Selector", Tested: false, History: []int{}}
	for i := range u.snap.Routes {
		r := &u.snap.Routes[i]
		r.Auto = false
		if r.Direct {
			continue
		}
		r.Members = []NodeView{n[1], n[3], sub}
		r.Picked, r.Node = n[3].Name, n[3].Name
	}
	return u
}

// A core that has no automatic group has no "Auto" to offer, and what
// a route can be pointed at is its group's members, not every node.
func TestExternalPanelListsMembers(t *testing.T) {
	u := externalUI()
	tt := panelTester(u, 640, false)
	wantTexts(t, tt, "香港 02", "东京 01", "selfhost", "全部节点…", "3 个")
	for _, s := range []string{"自动", "最快 + 故障转移", "香港 03 IPLC", "新加坡 01", "接管", "模式", "全局"} {
		if tt.HasText(s) {
			t.Errorf("the panel shows %q: %q", s, tt.Texts())
		}
	}
}

func TestExternalPanelWithoutNodes(t *testing.T) {
	u := externalUI()
	u.snap.Nodes = nil
	for i := range u.snap.Routes {
		u.snap.Routes[i].Members = nil
	}
	tt := panelTester(u, 640, false)
	if tt.HasText("提供商") {
		t.Errorf("the panel sends people to add a provider: %q", tt.Texts())
	}
}

func TestExternalRoutePicker(t *testing.T) {
	u := externalUI()
	tt := ui.NewTesterWith(u.view, 1060, 760, ui.TesterOptions{})
	u.home.x, u.home.sub = "routes", "ai"
	tt.Frame()
	tt.Advance(2 * time.Second)
	wantTexts(t, tt, "香港 02", "东京 01", "selfhost")
	for _, s := range []string{"自动", "香港 03 IPLC", "新加坡 01"} {
		if tt.HasText(s) {
			t.Errorf("the picker shows %q: %q", s, tt.Texts())
		}
	}
}

// An external core has no mode or takeover for bowel to offer: those
// cells say what the core is and where it is, and do not open.
func TestExternalBoardShowsTheCore(t *testing.T) {
	u := externalUI()
	u.snap.Core.Version = "sing-box 1.14.4"
	u.snap.ControllerTCP = "127.0.0.1:9090"
	tt := ui.NewTesterWith(u.view, 1060, 760, ui.TesterOptions{})
	tt.Advance(time.Second)
	shot(t, tt, "external-home")
	wantTexts(t, tt, "sing-box 1.14.4", "127.0.0.1:9090")
	for _, s := range []string{"系统代理", "TUN", "全局", "接管", "自动选最快", "来自"} {
		if tt.HasText(s) {
			t.Errorf("the board shows %q: %q", s, tt.Texts())
		}
	}
	for _, id := range []string{"mode", "takeover"} {
		u.home.x = id
		tt.Frame()
		tt.Advance(time.Second)
		if u.home.x != "" {
			t.Errorf("the %s cell opened", id)
		}
	}
}

// The tray menu of an external core has no takeover or mode to set, and
// its node list is the route's own members.
func TestExternalTrayMenu(t *testing.T) {
	u := externalUI()
	a := &App{}
	var labels []string
	var walk func(items []*mygo.MenuItem)
	walk = func(items []*mygo.MenuItem) {
		for _, it := range items {
			labels = append(labels, it.Label)
			walk(it.Submenu)
		}
	}
	walk(a.trayMenu(u.snap).Items())
	got := strings.Join(labels, "|")
	for _, s := range []string{"接管", "规则模式", "全局模式", "自动（最快 + 故障转移）", "香港 03 IPLC"} {
		if strings.Contains(got, s) {
			t.Errorf("the menu has %q: %s", s, got)
		}
	}
	for _, s := range []string{"香港 02", "selfhost"} {
		if !strings.Contains(got, s) {
			t.Errorf("the menu lacks %q: %s", s, got)
		}
	}
}

// An external core has no providers: bowel has no page for them, and
// neither a shortcut nor a pasted link opens one.
func TestExternalHasNoProviderPage(t *testing.T) {
	u := externalUI()
	tt := ui.NewTesterWith(u.view, 1060, 760, ui.TesterOptions{})
	tt.Advance(time.Second)
	if tt.HasText("提供商") {
		t.Errorf("the tabs show the provider page: %q", tt.Texts())
	}
	u.page = "providers"
	tt.Frame()
	tt.Advance(time.Second)
	if u.page != "home" {
		t.Errorf("the provider page opened: %q", u.page)
	}
}

// Settings about bowel's own core have nothing to set on someone else's:
// the window keeps what is bowel's, and names the core as it is.
func TestExternalSettingsWindow(t *testing.T) {
	u := externalUI()
	u.snap.Settings = settings.Defaults.Clone()
	u.snap.Core.Version = "sing-box 1.14.4"
	tt := ui.NewTesterWith(u.settingsView, 760, 560, ui.TesterOptions{})
	tt.Advance(time.Second)
	wantTexts(t, tt, "通用", "内核", "MCP", "关于", "sing-box 1.14.4")
	for _, s := range []string{"入站", "规则", "DNS", "嗅探", "TUN", "GeoData", "外部控制", "mihomo"} {
		if tt.HasText(s) {
			t.Errorf("the window shows %q: %q", s, tt.Texts())
		}
	}
	if err := tt.Click("内核"); err != nil {
		t.Fatal(err)
	}
	tt.Frame()
	if tt.HasText("日志级别") || tt.HasText("TLS 指纹") {
		t.Errorf("the core page offers bowel's own core's options: %q", tt.Texts())
	}
}

// An external core's connections can be read, not changed: no button
// closes one, and no menu writes a rule for it.
func TestExternalActivityOnlyReads(t *testing.T) {
	u := actUI()
	u.snap.External = true
	tt := ui.NewTester(u.view, 1060, 760)
	tt.Advance(time.Second)
	if err := tt.Click("api.anthropic.com:443"); err != nil {
		t.Fatal(err)
	}
	tt.Advance(time.Second)
	if u.act.open != "c04" {
		t.Fatalf("open %q", u.act.open)
	}
	wantTexts(t, tt, "复制域名", "相关日志")
	for _, s := range []string{"断开", "让它走…", "断开筛选结果"} {
		if hasTextPart(tt, s) {
			t.Errorf("the page offers %q: %q", s, tt.Texts())
		}
	}
	if err := tt.Click("断开"); err == nil {
		t.Error("a close button is on the page")
	}
}
