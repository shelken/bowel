package main

// Gateway mode: this Mac as the LAN's gateway. The core's side (TUN, the
// resolver on port 53) is in the config; the system's (IP forwarding,
// staying awake) is internal/gateway's. What the interface shows is
// measured, every few seconds while it is wanted.

import (
	"log"
	"net"
	"time"

	"github.com/oboo/moho/internal/gateway"
	"github.com/oboo/moho/internal/i18n"
	"github.com/oboo/moho/internal/settings"
	"github.com/oboo/moho/internal/tun"
)

// osGateway is the system the gateway changes. Tests replace it, so that
// they never touch the Mac's forwarding or its sleep.
var osGateway = gateway.OS

// osTun installs TUN's core, as the gateway needs it. Tests replace it.
var osTun = struct {
	installed func(path string) bool
	step      func(src, dst string) (tun.Step, error)
	admin     func(prompt string, steps ...tun.Step) error
}{tun.Installed, tun.InstallStep, tun.Admin}

// gwProbe is what the last look at the gateway found.
type gwProbe struct {
	at      time.Time // zero: not looked yet
	net     gateway.Network
	fwd     bool // IPv4 forwarding is on
	dns     bool // the resolver answers on the LAN address's port 53
	devices int  // other devices with connections through the core
}

// gatewayPrompt is what the administrator dialog says when it is shown
// for the gateway, with or without TUN's installation.
func gatewayPrompt() string {
	return i18n.T("bowel 需要授权以启用网关模式（只需一次）", "bowel needs authorization to turn on gateway mode (only once)")
}

// gatewayOn is whether the gateway should be in effect: wanted, and TUN
// in effect (takeover on, TUN wanted and installed).
func (a *App) gatewayOn(s settings.Settings) bool {
	return s.Gateway && a.takeoverOn(s) && s.Tun && osTun.installed(a.tunBin)
}

// setGateway turns gateway mode on or off. On needs TUN, so it turns TUN
// and takeover on too; the first time it asks for an administrator's
// authorisation, once for TUN and the gateway together.
func (a *App) setGateway(on bool) error {
	if err := a.errExternal(); err != nil {
		return err
	}
	done := a.begin("gateway")
	defer done()
	if on {
		if err := a.prepareGateway(); err != nil {
			return err
		}
	}
	if _, err := a.settings.Update(func(s *settings.Settings) {
		s.Gateway = on
		if on {
			s.Tun, s.Paused = true, false
		}
	}); err != nil {
		return err
	}
	err := a.afterTakeover()
	a.syncGateway() // also when the core failed to take the new config
	a.probeGateway()
	a.emit()
	if err != nil {
		return err
	}
	return a.gatewayErr()
}

// prepareGateway gets what the gateway needs root for: TUN's core, and
// the right to change forwarding. Both under one authorisation; nothing
// is asked when both are there.
func (a *App) prepareGateway() error {
	sys := a.gw.System()
	needTun := !osTun.installed(a.tunBin)
	if !needTun && sys.Ready() {
		return nil
	}
	var pre []tun.Step
	if needTun {
		st, err := osTun.step(a.coreBin, a.tunBin)
		if err != nil {
			return err
		}
		pre = append(pre, st)
	}
	var err error
	if sys.Ready() {
		err = osTun.admin(tun.Prompt(), pre...)
	} else {
		err = sys.Prepare(gatewayPrompt(), pre...)
	}
	if err != nil {
		return err
	}
	if needTun {
		// The core must be the setuid copy from now on.
		a.core.Stop()
		a.core.SetBin(a.tunBin)
	}
	return nil
}

// syncGateway makes forwarding follow the settings: on while the gateway
// is in effect, back as it was otherwise (also after a crash left it on).
func (a *App) syncGateway() {
	var err error
	if a.gatewayOn(a.settings.Get()) {
		err = a.gw.Enable()
	} else {
		err = a.gw.Disable()
	}
	a.mu.Lock()
	a.gwErr = err
	a.mu.Unlock()
	if err != nil {
		log.Println("gateway:", err)
	}
}

func (a *App) gatewayErr() error {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.gwErr
}

// probeGateway looks at the gateway as it really is, while it is wanted.
func (a *App) probeGateway() {
	s := a.settings.Get()
	if !s.Gateway {
		a.mu.Lock()
		a.gwProbe = gwProbe{}
		a.mu.Unlock()
		return
	}
	sys := a.gw.System()
	pr := gwProbe{at: time.Now(), net: sys.Network()}
	pr.fwd, _ = sys.Forwarding()
	if pr.net.IP != "" {
		pr.dns = sys.AnswersDNS(net.JoinHostPort(pr.net.IP, "53"))
	}
	pr.devices = gateway.Devices(a.mon.Sources(), gateway.LocalAddrs())
	a.mu.Lock()
	a.gwProbe = pr
	a.mu.Unlock()
}

// probeSoon looks again in the background, at most every 4 seconds and
// one look at a time.
func (a *App) probeSoon() {
	a.mu.Lock()
	due := a.settings.Get().Gateway && !a.gwProbing && time.Since(a.gwProbe.at) >= 4*time.Second
	if due {
		a.gwProbing = true
	}
	a.mu.Unlock()
	if !due {
		return
	}
	go func() {
		a.probeGateway()
		a.mu.Lock()
		a.gwProbing = false
		a.mu.Unlock()
		a.emit()
	}()
}

// gatewayView is the gateway as the interface shows it: on only when
// everything it needs is measured to work.
func gatewayView(s settings.Settings, on bool, tunStatus string, ready, awake bool, pr gwProbe, err error) GatewayView {
	g := GatewayView{Wanted: s.Gateway, Status: "off", Ready: ready, Network: pr.net,
		Forwarding: pr.fwd, DNS: pr.dns, Tun: tunStatus == "on", Awake: awake, Devices: pr.devices}
	if !s.Gateway {
		return g
	}
	switch {
	case !on || !s.Tun:
		g.Status, g.Detail = "paused", i18n.T("接管暂停了，网关也暂停", "Takeover is paused, so the gateway is too")
	case tunStatus == "install" || (!ready && !pr.fwd):
		g.Status, g.Detail = "install", i18n.T("需要授权一次", "Needs authorization once")
	case tunStatus != "on":
		g.Status, g.Detail = "error", i18n.T("TUN 没有运行", "TUN isn't running")
	case pr.at.IsZero():
		g.Status = "checking"
	case pr.net.IP == "":
		g.Status, g.Detail = "error", i18n.T("这台 Mac 没有连上局域网", "This Mac isn't on a LAN")
	case !pr.fwd:
		g.Status, g.Detail = "error", i18n.T("IP 转发没有打开", "IP forwarding is off")
		if err != nil {
			g.Detail = err.Error()
		}
	case !pr.dns:
		g.Status, g.Detail = "error", i18n.T("53 端口没有应答 DNS", "Nothing answers DNS on port 53")
	default:
		g.Status = "on"
	}
	return g
}
