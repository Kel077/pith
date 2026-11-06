// Package api is the contract between Pith's interface and its backend.
//
// The interface (cmd/pith, internal/ui) depends on this package alone. The
// real backend, internal/client, keeps the configuration in the user's
// Application Support folder and drives the privileged helper, cmd/pithd,
// over a Unix socket; internal/mock implements the same Service in memory.
//
// Both sides build on this package: change it only by agreement (AGENTS.md).
package api

import (
	"context"
	"time"
)

// Service is everything the interface asks of the backend. All methods may
// block, so the interface calls them off its main thread; they are safe to
// call from any goroutine.
type Service interface {
	// LoadConfig returns the saved configuration, or DefaultConfig when
	// nothing is saved yet.
	LoadConfig(ctx context.Context) (Config, error)
	// SaveConfig validates and saves cfg. When the running scene changed,
	// the engine applies new rules at once. Open captured connections whose
	// decision or first-choice interface changes are closed to reconnect;
	// unaffected connections retain their route.
	SaveConfig(ctx context.Context, cfg Config) error

	// Status reports the helper and the engine.
	Status(ctx context.Context) (Status, error)
	// InstallHelper explicitly installs, updates or repairs the privileged
	// helper. macOS asks for an administrator's password: ErrCanceled when the
	// user cancels.
	InstallHelper(ctx context.Context) error
	// UninstallHelper stops routing and removes the helper.
	UninstallHelper(ctx context.Context) error

	// Start routes connections by the rules of a scene of the saved
	// configuration, replacing the scene that runs.
	Start(ctx context.Context, sceneID string) error
	// Stop stops routing: macOS routes every connection again.
	Stop(ctx context.Context) error

	// Subscribe streams events until ctx ends, then closes the channel.
	// The first two events are the current Status and Interfaces.
	Subscribe(ctx context.Context) (<-chan Event, error)

	// Interfaces lists the network interfaces rules can route through.
	Interfaces(ctx context.Context) ([]Interface, error)
	// Probe connects through one interface, by its Name, to target: a
	// "host:port" opened over TCP, or, when target is empty, Pith's HTTPS
	// test that also finds the interface's public addresses.
	Probe(ctx context.Context, iface, target string) (Probe, error)

	// Hotspot reads the mobile hotspot behind an interface, by its Name:
	// ErrUnsupported when Pith can't talk to it.
	Hotspot(ctx context.Context, iface string) (Hotspot, error)
	// SetHotspotPassword tries the admin password of the hotspot behind an
	// interface, ErrWrongPassword when it refuses it, and keeps it in the
	// keychain.
	SetHotspotPassword(ctx context.Context, iface, password string) error
	// RenewHotspot has the hotspot behind an interface reconnect to its
	// carrier, which gives it new public addresses, and returns once it is
	// connected again.
	RenewHotspot(ctx context.Context, iface string) (HotspotRenewal, error)

	// Connections returns logged connections, newest first.
	Connections(ctx context.Context, q ConnectionQuery) ([]Connection, error)
	// ClearConnections empties the connection log.
	ClearConnections(ctx context.Context) error
	// Traffic sums the bytes of the connections Pith handled.
	Traffic(ctx context.Context, q TrafficQuery) (Traffic, error)

	// Check tells which rule a connection would match and where it would go.
	Check(ctx context.Context, in CheckInput) (CheckResult, error)
}

// Event is one change the backend reports. Exactly one field is set.
type Event struct {
	// Status, on every change and once a second while routing (rates).
	Status *Status `json:"status,omitempty"`
	// Connection, when one opens, closes, or (at most once a second) when
	// its byte counts change. Connection.ID identifies it across events.
	Connection *Connection `json:"connection,omitempty"`
	// Interfaces, the whole list, when any interface changes.
	Interfaces []Interface `json:"interfaces,omitempty"`
}

// ---- Configuration ----------------------------------------------------------

// ConfigVersion is the version of the Config format this package describes.
const ConfigVersion = 1

// Config is everything the user configures. It is saved as JSON.
type Config struct {
	Version  int      `json:"version"`
	Scenes   []Scene  `json:"scenes"`   // at least one
	Selected string   `json:"selected"` // the scene the menu bar starts
	Settings Settings `json:"settings"`
	// Aliases are the names the user gave interfaces, by InterfaceRef.Key.
	Aliases map[string]string `json:"aliases,omitempty"`
}

// Scene is a named, ordered list of rules. One scene routes at a time.
type Scene struct {
	ID    string `json:"id"`
	Name  string `json:"name"` // empty: the interface shows its default name
	Rules []Rule `json:"rules"`
}

// Rule sends the connections it matches out through chosen interfaces, or
// blocks them.
//
// A connection matches an enabled rule when its app, its destination and
// its protocol all match. Block rules win over route rules; among route
// rules the first in the list wins. A connection no rule matches goes
// where macOS would send it without Pith.
type Rule struct {
	ID      string `json:"id"`
	Enabled bool   `json:"enabled"`
	Note    string `json:"note,omitempty"`
	// Apps the rule applies to; empty: every app.
	Apps []AppRef `json:"apps,omitempty"`
	// Targets are destinations, any of which matches, in the syntax of
	// ParseTarget; empty: every destination.
	Targets  []string `json:"targets,omitempty"`
	Protocol Protocol `json:"protocol"`
	Action   Action   `json:"action"`
	// Via are the interfaces a route rule uses, the first that connects
	// winning. Unused by block rules.
	Via []InterfaceRef `json:"via,omitempty"`
	// Fallback lets a route rule fall back to macOS's own routing when
	// none of Via connects. Off, the connection fails instead.
	Fallback bool `json:"fallback"`
}

// Protocol is the transport a rule matches.
type Protocol string

const (
	AnyProtocol Protocol = "any"
	TCP         Protocol = "tcp"
	UDP         Protocol = "udp"
)

// Action is what a rule does with the connections it matches.
type Action string

const (
	Route Action = "route"
	Block Action = "block"
)

// AppRef names an app. A connection belongs to an app when the process it
// is attributed to runs from inside the app's bundle (helpers, XPC services
// and login items inside it included) or, for a command-line tool, runs
// that very executable. A connection a process opens on behalf of another
// (delegated sockets, as WebKit's networking process and nsurlsessiond
// open) is attributed to the one it serves.
type AppRef struct {
	// Path is the app's bundle, /Applications/Safari.app, or a tool's
	// executable, /usr/bin/curl.
	Path string `json:"path"`
	// BundleID, when the app has one, also matches the app installed
	// elsewhere than Path.
	BundleID string `json:"bundleId,omitempty"`
	// Name is the name to show, saved with the rule.
	Name string `json:"name,omitempty"`
}

// InterfaceRef names a network interface in a rule.
type InterfaceRef struct {
	// Name is the BSD name the engine routes through: en0.
	Name string `json:"name"`
	// Label is the name the user chose it by: Wi-Fi, Ethernet.
	Label string `json:"label,omitempty"`
	// HardwareAddr finds a USB adapter whose BSD name changed after it was
	// plugged in again; the engine prefers it over Name when both exist.
	HardwareAddr string `json:"hardwareAddr,omitempty"`
}

// Key identifies the interface across BSD name changes: its hardware
// address when it has one, else its BSD name. Several interfaces can share
// a hardware address (a bridge and its members, the ports of one adapter),
// so look interfaces up with Resolve, not by Key.
func (r InterfaceRef) Key() string {
	if r.HardwareAddr != "" {
		return r.HardwareAddr
	}
	return r.Name
}

// Resolve finds the interface r names among ifaces: the one with both its
// name and its hardware address; else the only one with its hardware
// address (a USB adapter that came back under another BSD name); else, for
// a reference without a hardware address, the one with its name. Nil when
// none is.
func (r InterfaceRef) Resolve(ifaces []Interface) *Interface {
	var byAddr *Interface
	n := 0
	for i := range ifaces {
		switch {
		case ifaces[i].Name == r.Name && ifaces[i].HardwareAddr == r.HardwareAddr:
			return &ifaces[i]
		case r.HardwareAddr != "" && ifaces[i].HardwareAddr == r.HardwareAddr:
			byAddr = &ifaces[i]
			n++
		}
	}
	if n == 1 {
		return byAddr
	}
	if r.HardwareAddr == "" {
		for i := range ifaces {
			if ifaces[i].Name == r.Name {
				return &ifaces[i]
			}
		}
	}
	return nil
}

// Settings are the preferences of the app and of the engine.
type Settings struct {
	// The interface applies these.
	OpenAtLogin    bool   `json:"openAtLogin"`
	KeepRouting    bool   `json:"keepRouting"`        // keep routing after Pith quits; off, quitting stops it
	ResumeOnLaunch bool   `json:"resumeOnLaunch"`     // start the selected scene when Pith opens, if it was routing when it last quit or the Mac shut down
	Language       string `json:"language,omitempty"` // "", the system's, "en" or "zh-Hans"
	MenuBarSpeed   bool   `json:"menuBarSpeed"`       // show the speed beside the menu bar icon

	// The engine applies these.
	IPv6         bool `json:"ipv6"`         // route IPv6 connections too
	SniffDomains bool `json:"sniffDomains"` // read TLS SNI and HTTP Host to match domain rules
	SafeStart    bool `json:"safeStart"`    // undo a start that leaves the Mac without internet (ErrNetworkLost)
	LogDays      int  `json:"logDays"`      // days to keep the connection log on disk; 0: memory only
}

// DefaultSettings are the settings of a new configuration.
func DefaultSettings() Settings {
	return Settings{KeepRouting: true, IPv6: true, SniffDomains: true, SafeStart: true, LogDays: 7}
}

// DefaultConfig is the configuration before the user saves one: one empty
// scene, selected.
func DefaultConfig() Config {
	return Config{
		Version:  ConfigVersion,
		Scenes:   []Scene{{ID: "default"}},
		Selected: "default",
		Settings: DefaultSettings(),
	}
}

// ---- Status -----------------------------------------------------------------

// State is the state of the engine.
type State string

const (
	Stopped  State = "stopped"
	Starting State = "starting"
	Running  State = "running"
	Stopping State = "stopping"
	Failed   State = "failed" // stopped by an error, in Status.Error
)

// Status reports the helper and the engine.
type Status struct {
	Helper   HelperStatus `json:"helper"`
	State    State        `json:"state"`
	SceneID  string       `json:"sceneId,omitempty"` // the scene routing
	Since    time.Time    `json:"since,omitzero"`    // when it started
	Error    *Error       `json:"error,omitempty"`   // why it failed
	Warnings []Warning    `json:"warnings,omitempty"`
	// LastCapture identifies the most recently prepared capture in this helper
	// process, including after Stop or a SafeStart failure. Nil before the first
	// successful preparation and after a helper restart. This is historical
	// ownership evidence, not proof that capture is active or cleanup succeeded.
	LastCapture *CaptureIdentity `json:"lastCapture,omitempty"`
	// RouteDiagnostics retains at most 32 address-free technical summaries from
	// the latest capture attempt, including after Stop or startup failure. Entries
	// describe unusual routing messages (type, flags, index, address formats), not
	// packet contents or interface names. Cleared at Start and helper restart.
	RouteDiagnostics []string `json:"routeDiagnostics,omitempty"`

	Open        int    `json:"open"`        // connections open now
	Sent        uint64 `json:"sent"`        // bytes since Since
	Received    uint64 `json:"received"`    // bytes since Since
	SendRate    uint64 `json:"sendRate"`    // bytes a second, over the last second
	ReceiveRate uint64 `json:"receiveRate"` // bytes a second, over the last second
}

// CaptureIdentity comes from the helper's capture journal. Token distinguishes
// successive captures; names and indices may be reused by the OS. Callers must
// independently observe cleanup and never use this value to delete routes.
type CaptureIdentity struct {
	BootSession string   `json:"bootSession"`
	Token       string   `json:"token"`
	Interface   string   `json:"interface"`
	Index       int      `json:"index"`
	Prefixes    []string `json:"prefixes"`
	// TunnelSubnets contains the selected IPv4 /30 and optional IPv6 /126.
	// These identify addresses that must be absent before accepting interface reuse.
	TunnelSubnets []string `json:"tunnelSubnets,omitempty"`
	// EndedAt is set only after the device closed successfully and a subsequent
	// snapshot found no owned capture routes. Zero means unconfirmed (including
	// older helpers). It proves neither baseline restoration nor deletion authority.
	EndedAt time.Time `json:"endedAt,omitzero"`
}

// HelperStatus reports the privileged helper.
type HelperStatus struct {
	Installed bool   `json:"installed"`
	Running   bool   `json:"running"`           // it answers on its socket
	Version   string `json:"version,omitempty"` // informational helper version, never an authorization check
	// Outdated means the installed helper must be replaced to match this app's
	// expected artifact or protocol. It includes incompatible newer helpers.
	Outdated bool `json:"outdated"`
}

// Warning is something the user should know while routing works.
type Warning struct {
	Code WarningCode `json:"code"`
	// Detail names what it concerns, as an interface or a rule ID; it is
	// not a sentence, the interface writes those.
	Detail string `json:"detail,omitempty"`
}

// WarningCode identifies a warning.
type WarningCode string

const (
	// WarnVPN: another VPN routes all traffic (Detail: its interface).
	// Connections no rule matches still go through it.
	WarnVPN WarningCode = "vpn"
	// WarnInterfaceDown: an interface of an enabled rule is down (Detail:
	// its Name).
	WarnInterfaceDown WarningCode = "interface_down"
	// WarnNoDomains: domain rules can only match connections whose host
	// name Pith can see, and SniffDomains is off.
	WarnNoDomains WarningCode = "no_domains"
	// WarnDNSUnobserved: an existing configured host route prevents adding a
	// DNS capture route (Detail: the server's IP address). Queries using that
	// route are not observed; domain rules need another observed name, such as
	// TLS SNI, HTTP Host or QUIC SNI when SniffDomains is enabled.
	WarnDNSUnobserved WarningCode = "dns_unobserved"
)

// ---- Interfaces -------------------------------------------------------------

// InterfaceKind is what kind of link an interface is.
type InterfaceKind string

const (
	KindWiFi        InterfaceKind = "wifi"
	KindEthernet    InterfaceKind = "ethernet" // built in, USB or Thunderbolt adapters
	KindCellular    InterfaceKind = "cellular" // iPhone USB, a phone's hotspot
	KindThunderbolt InterfaceKind = "thunderbolt"
	KindVPN         InterfaceKind = "vpn"
	KindOther       InterfaceKind = "other"
)

// Interface is a network interface.
type Interface struct {
	Name         string        `json:"name"`  // BSD name: en0
	Label        string        `json:"label"` // as System Settings names it: Wi-Fi, Ethernet, iPhone USB
	Kind         InterfaceKind `json:"kind"`
	HardwareAddr string        `json:"hardwareAddr,omitempty"`
	Up           bool          `json:"up"`             // it has a link and an address: rules can use it
	IPv4         []string      `json:"ipv4,omitempty"` // addresses, without prefix lengths
	IPv6         []string      `json:"ipv6,omitempty"` // global addresses
	Router       string        `json:"router,omitempty"`
	Router6      string        `json:"router6,omitempty"`
	DNS          []string      `json:"dns,omitempty"`
	Primary      bool          `json:"primary"` // macOS's default route goes through it
}

// Probe is the result of connecting through one interface.
type Probe struct {
	Interface  string        `json:"interface"`
	Target     string        `json:"target,omitempty"`  // as given; empty: Pith's HTTPS test
	Address    string        `json:"address,omitempty"` // the "ip:port" it connected to
	At         time.Time     `json:"at"`
	OK         bool          `json:"ok"`
	Latency    time.Duration `json:"latency"` // DNS and TCP connect; for the HTTPS test, the whole request
	PublicIPv4 string        `json:"publicIPv4,omitempty"`
	PublicIPv6 string        `json:"publicIPv6,omitempty"`
	Problem    string        `json:"problem,omitempty"` // technical, when !OK
}

// Hotspot is a mobile hotspot behind one of the interfaces, as it reports
// its connection to its carrier.
type Hotspot struct {
	Interface  string `json:"interface"`
	Model      string `json:"model"`                // as it names itself: M2000
	Connected  bool   `json:"connected"`            // to its carrier
	Technology string `json:"technology,omitempty"` // 5G, LTE
	Bars       int    `json:"bars"`                 // signal, 0 to 5
	// Locked: Pith needs the hotspot's admin password, SetHotspotPassword,
	// to read its cell and to renew it.
	Locked bool         `json:"locked,omitempty"`
	Cell   *HotspotCell `json:"cell,omitempty"` // nil when Locked
	At     time.Time    `json:"at"`
}

// HotspotCell is the cell a hotspot uses.
type HotspotCell struct {
	Operator string `json:"operator,omitempty"` // the carrier: T-Mobile
	Band     string `json:"band,omitempty"`     // n41, B66
	Signal   int    `json:"signal"`             // dBm
	SNR      int    `json:"snr"`                // dB
	Roaming  bool   `json:"roaming,omitempty"`
}

// HotspotRenewal tells what renewing a hotspot changed: Pith's HTTPS test
// through its interface before and after.
type HotspotRenewal struct {
	Before Probe         `json:"before"`
	After  Probe         `json:"after"`
	Took   time.Duration `json:"took"` // until the hotspot was connected again
}

// ---- Connections and traffic ------------------------------------------------

// Outcome is what happened to a connection.
type Outcome string

const (
	Routed      Outcome = "routed"      // through the first interface of its rule
	Fallback    Outcome = "fallback"    // through a later interface of its rule
	System      Outcome = "system"      // where macOS sends it: no rule matched, or its rule fell back
	Blocked     Outcome = "blocked"     // by a block rule
	Unreachable Outcome = "unreachable" // no interface of its rule connected, and Fallback was off
)

// EndReason explains a normal connection termination that was not a failure.
// Empty means no additional termination reason was recorded.
type EndReason string

// EndRulesChanged closes a connection so its app can reconnect under new rules.
// It does not claim that the app has already reconnected successfully.
const EndRulesChanged EndReason = "rules_changed"

// Connection is a TCP connection or a UDP flow that Pith handled.
type Connection struct {
	ID        uint64    `json:"id"`
	Started   time.Time `json:"started"`
	Ended     time.Time `json:"ended,omitzero"`      // zero while open
	EndReason EndReason `json:"endReason,omitempty"` // normal termination; Outcome is retained and Problem is empty
	App       AppRef    `json:"app"`                 // the app it is attributed to
	Process   Process   `json:"process"`             // the process that owns the socket
	Protocol  Protocol  `json:"protocol"`            // TCP or UDP
	Local     string    `json:"local,omitempty"`     // application-side "ip:port" or "[ip6]:port"
	// OutboundLocal is the outbound socket's local ip:port before NAT. It is
	// empty when blocked or dialing did not succeed; it is not the public NAT tuple.
	OutboundLocal string  `json:"outboundLocal,omitempty"`
	Remote        string  `json:"remote"` // "ip:port", "[ip6]:port"
	Domain        string  `json:"domain,omitempty"`
	RuleID        string  `json:"ruleId,omitempty"` // the rule that decided; empty: none matched
	Outcome       Outcome `json:"outcome"`
	Interface     string  `json:"interface,omitempty"` // the interface it went out through
	Problem       string  `json:"problem,omitempty"`   // technical, when it failed
	Sent          uint64  `json:"sent"`
	Received      uint64  `json:"received"`
}

// Process is a running program.
type Process struct {
	PID  int    `json:"pid"`
	Name string `json:"name"`
	Path string `json:"path"`
}

// ConnectionQuery selects logged connections.
type ConnectionQuery struct {
	Limit  int       `json:"limit"` // 0: 500
	Since  time.Time `json:"since,omitzero"`
	Open   bool      `json:"open"`             // open connections only
	App    string    `json:"app,omitempty"`    // of this AppRef.Path only
	Search string    `json:"search,omitempty"` // in app names, domains and addresses, ignoring case
}

// TrafficQuery selects a period of traffic.
type TrafficQuery struct {
	Since time.Time `json:"since"`
	// Step is the interval of Traffic.Series; 0 means no series. For Step > 0,
	// samples are aligned to Since: At = Since + k*Step, summing stored buckets
	// whose timestamps fall in [At, At+Step). The backend keeps sub-hour buckets
	// for at least eight days, so week queries never rely on UTC daily totals.
	// Stored buckets before Since are excluded; they cannot be divided at
	// arbitrary boundaries. Missing samples represent zero traffic.
	Step time.Duration `json:"step"`
}

// Traffic sums the bytes of the connections Pith handled.
type Traffic struct {
	Since      time.Time          `json:"since"`
	Apps       []AppTraffic       `json:"apps"`       // most bytes first
	Interfaces []InterfaceTraffic `json:"interfaces"` // most bytes first
	Series     []TrafficSample    `json:"series,omitempty"`
}

// AppTraffic is the traffic of one app.
type AppTraffic struct {
	App         AppRef `json:"app"`
	Sent        uint64 `json:"sent"`
	Received    uint64 `json:"received"`
	Connections int    `json:"connections"`
}

// InterfaceTraffic is the traffic through one interface.
type InterfaceTraffic struct {
	Interface string `json:"interface"`
	Sent      uint64 `json:"sent"`
	Received  uint64 `json:"received"`
}

// TrafficSample is traffic through one interface in [At, At+Step), with At
// aligned to the query's Since, not UTC midnight. Step is the query's duration;
// calendar-day grouping (including daylight-saving changes) belongs to the UI.
type TrafficSample struct {
	At        time.Time `json:"at"`
	Interface string    `json:"interface"`
	Sent      uint64    `json:"sent"`
	Received  uint64    `json:"received"`
}

// ---- Checking rules ---------------------------------------------------------

// CheckInput describes a connection to check against a saved scene.
type CheckInput struct {
	SceneID string `json:"sceneId,omitempty"` // empty: the selected scene
	App     AppRef `json:"app"`               // zero: an app no rule names
	// Target is a domain, an IP address, or a URL whose host counts.
	Target string `json:"target"`
	// Address is the IP the connection goes to; empty: Target's, or what
	// Target resolves to now.
	Address  string   `json:"address,omitempty"`
	Protocol Protocol `json:"protocol"` // TCP or UDP; AnyProtocol checks TCP
}

// CheckResult tells what would happen to the connection now.
type CheckResult struct {
	Addresses []string    `json:"addresses,omitempty"` // what Target resolved to, when it is a domain
	Rules     []RuleCheck `json:"rules"`               // every rule of the scene, in order
	RuleID    string      `json:"ruleId,omitempty"`    // the rule that decides; empty: none matches
	Outcome   Outcome     `json:"outcome"`
	Interface string      `json:"interface,omitempty"` // where it would go out now
}

// RuleCheck is how one rule compares with the connection checked.
type RuleCheck struct {
	RuleID string      `json:"ruleId"`
	Result CheckReason `json:"result"`
}

// CheckReason is why a rule matches the connection checked, or why not.
type CheckReason string

const (
	CheckMatched  CheckReason = "matched"  // it decides
	CheckShadowed CheckReason = "shadowed" // it matches, but another rule decides
	CheckDisabled CheckReason = "disabled"
	CheckApp      CheckReason = "app"      // the app differs
	CheckTarget   CheckReason = "target"   // the destination differs
	CheckProtocol CheckReason = "protocol" // the protocol differs
)
