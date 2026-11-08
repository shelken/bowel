package main

// The view model. The interface renders from two values: a Snapshot,
// rebuilt whenever anything changes, and a Tick, every second while the
// core runs. Actions are App methods; their results show in the next
// Snapshot.

import (
	"time"

	"github.com/oboo/moho/internal/gateway"
	"github.com/oboo/moho/internal/settings"
)

// ---- State ----

// State is the one light of the product.
type State string

const (
	StateOK       State = "ok"
	StateDegraded State = "degraded"
	StateError    State = "error"
	StateOff      State = "off"
)

// Snapshot is everything the page shows.
type Snapshot struct {
	State State  `json:"state"`
	Issue *Issue `json:"issue,omitempty"`
	Core  Core   `json:"core"`
	// External: the core is someone else's, with its own routes and nodes.
	External bool `json:"external"`
	// Mode is "rule", "global" or "direct".
	Mode      string         `json:"mode"`
	Takeover  Takeover       `json:"takeover"`
	Routes    []RouteView    `json:"routes"`
	Nodes     []NodeView     `json:"nodes"`
	Exit      *Exit          `json:"exit,omitempty"`
	You       *Located       `json:"you,omitempty"`
	Providers []ProviderView `json:"providers"`
	RuleCount int            `json:"ruleCount"`
	// Pending lists actions being applied, for loading states:
	// "entry:sys", "entry:tun", "mode", "node:<route>", "test:<route>",
	// "provider:<id>", "providers", "retake", "add".
	Pending []string `json:"pending"`
	Version string   `json:"version"`
	// Settings are the saved options the 设置 page edits.
	Settings   settings.Settings `json:"settings"`
	ConfigPath string            `json:"configPath"`
	// OpenAtLogin: Moho is a login item.
	OpenAtLogin bool `json:"openAtLogin"`
	// ControllerTCP is Moho's own controller address when it fell back to
	// TCP; the dashboards' controller is then that one.
	ControllerTCP string `json:"controllerTcp,omitempty"`
}

// Issue is the news line: shown only when degraded or broken.
type Issue struct {
	Level string `json:"level"` // warn, error
	Text  string `json:"text"`  // short: it sits in the title bar
	// Detail says more, on hover.
	Detail string `json:"detail,omitempty"`
	// Action is what the button does: "retake", "activity", "providers", "".
	Action string `json:"action"`
	Label  string `json:"label"`
}

type Core struct {
	State   string `json:"state"` // stopped, starting, running, error
	Version string `json:"version"`
	Error   string `json:"error,omitempty"`
}

// Entry is one way traffic enters Moho, as it really is.
type Entry struct {
	// Wanted is the user's choice.
	Wanted bool `json:"wanted"`
	// Status: off, on, hijacked (system proxy points elsewhere), install
	// (TUN needs its one-time authorisation), error.
	Status string `json:"status"`
	Detail string `json:"detail,omitempty"`
}

type Takeover struct {
	// On is whether Moho takes over traffic at all (not paused and an entry wanted).
	On      bool     `json:"on"`
	System  Entry    `json:"system"`
	Tun     Entry    `json:"tun"`
	Port    int      `json:"port"`
	Lan     bool     `json:"lan"`
	LanAddr string   `json:"lanAddr,omitempty"`
	Bypass  []string `json:"bypass"`
	// Gateway is this Mac as the LAN's gateway, as it really is.
	Gateway GatewayView `json:"gateway"`
}

// GatewayView is gateway mode as measured.
type GatewayView struct {
	Wanted bool `json:"wanted"`
	// Status: off, checking (not looked at yet), on, paused (takeover or
	// TUN is off), install (needs its one-time authorisation), error.
	Status string `json:"status"`
	Detail string `json:"detail,omitempty"`
	// Ready: forwarding can change without asking (authorised before).
	Ready bool `json:"ready"`
	// The network, for the guide and the tips.
	gateway.Network
	// What was measured: forwarding on, the resolver answering on the LAN
	// address, TUN running, the Mac kept awake.
	Forwarding bool `json:"forwarding"`
	DNS        bool `json:"dns"`
	Tun        bool `json:"tun"`
	Awake      bool `json:"awake"`
	// Devices is how many other devices have connections through bowel.
	Devices int `json:"devices"`
}

// RouteView is a kind of traffic and where it goes now.
type RouteView struct {
	ID      string `json:"id"`
	Name    string `json:"name"`
	Color   string `json:"color"`
	Summary string `json:"summary"`
	Rules   int    `json:"rules"` // the user's included
	Mine    int    `json:"mine"`  // the user's rules for it
	Direct  bool   `json:"direct"`
	// Auto: the route picks the fastest node and fails over by itself.
	Auto bool `json:"auto"`
	// Node is the node in use (a NodeView name), "" for direct routes.
	Node string `json:"node"`
	// Members and Picked are an external core's: what its group can be
	// pointed at (nodes, and groups of its own), and the one it is.
	Members []NodeView `json:"members,omitempty"`
	Picked  string     `json:"picked,omitempty"`
	// Share is the route's part of today's download, 0–1.
	Share float64 `json:"share"`
}

// Place is where something is on the map.
type Place struct {
	Country string  `json:"country"`
	City    string  `json:"city"`
	Flag    string  `json:"flag"`
	Lat     float64 `json:"lat"`
	Lon     float64 `json:"lon"`
}

// Located is a place and how it is known: "probe" (measured through the
// node), "name" (guessed from the node's name), "unknown", "locating".
type Located struct {
	Place  *Place `json:"place,omitempty"`
	Source string `json:"source"`
	IP     string `json:"ip,omitempty"`
}

type NodeView struct {
	// Name is the node's id in mihomo; Display what people read.
	Name     string `json:"name"`
	Display  string `json:"display"`
	Provider string `json:"provider"` // provider id
	Type     string `json:"type"`
	UDP      bool   `json:"udp"`
	// Delay of the last test in ms; 0 failed or untested (see Tested).
	Delay   int     `json:"delay"`
	Tested  bool    `json:"tested"`
	History []int   `json:"history"`
	Where   Located `json:"where"`
}

// Exit is where the default route's traffic leaves.
type Exit struct {
	Node  string  `json:"node"`
	Where Located `json:"where"`
}

type Usage struct {
	Upload   int64 `json:"upload"`
	Download int64 `json:"download"`
	Total    int64 `json:"total"`
	Expire   int64 `json:"expire"`
}

type ProviderView struct {
	ID         string    `json:"id"`
	Name       string    `json:"name"`
	Kind       string    `json:"kind"`   // subscription, link, file
	Source     string    `json:"source"` // masked URL or file path
	Enabled    bool      `json:"enabled"`
	AutoUpdate int       `json:"autoUpdate"` // hours, 0 never
	UpdatedAt  time.Time `json:"updatedAt"`
	Usage      *Usage    `json:"usage,omitempty"`
	Error      string    `json:"error,omitempty"`
	Nodes      int       `json:"nodes"`
	// Warn: "quota" (over 90% used), "expire" (within 7 days), "".
	Warn string `json:"warn,omitempty"`
}

// ---- Tick ----

type Tick struct {
	Down int64 `json:"down"` // bytes per second
	Up   int64 `json:"up"`
	// ByRoute is the download rate of each route id.
	ByRoute   map[string]int64 `json:"byRoute"`
	Apps      []AppRate        `json:"apps"`
	Events    []Event          `json:"events"` // newest first
	Active    int              `json:"active"`
	TodayDown int64            `json:"todayDown"`
	TodayUp   int64            `json:"todayUp"`
	MonthDown int64            `json:"monthDown"`
}

type AppRate struct {
	App   string `json:"app"`
	Host  string `json:"host"` // its busiest destination
	Route string `json:"route"`
	Down  int64  `json:"down"`
	Up    int64  `json:"up"`
	Conns int    `json:"conns"`
}

type Event struct {
	Time   time.Time `json:"time"`
	Text   string    `json:"text"` // "Arc → youtube.com"
	Route  string    `json:"route"`
	Node   string    `json:"node"` // display name, "" for direct
	Failed bool      `json:"failed"`
}

// ---- Activity ----

type Conn struct {
	ID      string `json:"id"`
	App     string `json:"app"`
	Process string `json:"process"` // executable path
	Host    string `json:"host"`
	IP      string `json:"ip"`
	Port    int    `json:"port"`
	Network string `json:"network"` // tcp, udp
	Rule    string `json:"rule"`
	Payload string `json:"payload"`
	Route   string `json:"route"` // route id, "" when unknown
	// Node is the display name of the node, "" for direct.
	Node     string    `json:"node"`
	Chain    []string  `json:"chain"` // display names, outermost first
	Down     int64     `json:"down"`
	Up       int64     `json:"up"`
	DownRate int64     `json:"downRate"`
	UpRate   int64     `json:"upRate"`
	Start    time.Time `json:"start"`
	End      time.Time `json:"end,omitzero"`
	Status   string    `json:"status"` // active, closed, failed
	Error    string    `json:"error,omitempty"`
}

type LogLine struct {
	Seq   int64     `json:"seq"`
	Time  time.Time `json:"time"`
	Level string    `json:"level"` // debug, info, warning, error
	Text  string    `json:"text"`
}
