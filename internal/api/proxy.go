package api

import (
	"net"
	"net/netip"
	"strconv"
	"strings"
	"time"
)

// MaxProxies bounds configured local listeners, including disabled entries.
const MaxProxies = 32

// MaxDevices bounds the devices that may use shared proxies.
const MaxDevices = 64

// Proxy exposes SOCKS5 no-auth CONNECT on 127.0.0.1:Port in the App process,
// and HTTP on the same port: CONNECT, and requests for http:// URLs. It
// goes out through Via, one interface, unless Upstream or Scene is set: to
// literal IPv4/IPv6 destinations and to names resolved through Via's own
// DNS, never through another interface as fallback. It does not depend on
// TUN. Whatever it goes out through, it refuses a loopback address and the
// names reserved for loopback; a name that resolves to a loopback address
// is refused through an interface and by a scene's rules, while what a name
// reaches through an Upstream is that proxy's to answer for. An HTTP request
// made to the port rather than through it asks Pith itself: see ProxyInfo.
type Proxy struct {
	Port    int          `json:"port"`         // 1024..65535; unique even when disabled
	Via     InterfaceRef `json:"via,omitzero"` // one supported physical exit, resolved by Resolve
	Enabled bool         `json:"enabled"`
	// Upstream replaces Via: the "host:port" of a SOCKS5 proxy without
	// authentication that every connection goes out through, as a port
	// another Mac's Pith shares. It is reached as macOS routes, and resolves
	// the names it is asked for itself. It is never asked for a loopback
	// address or name.
	Upstream string `json:"upstream,omitempty"`
	// Scene replaces Via: the ID of a scene whose rules choose what each
	// connection goes out through, whether or not that scene routes this
	// Mac. Its enabled rules are matched against the destination alone, a
	// name as the client asks for it or an address; rules that name apps
	// never match, since a proxy can't tell the app. A block rule refuses
	// the connection. A route rule's interfaces are tried in order, each as
	// a proxy through it would; when none connects, the connection goes
	// where macOS sends it if the rule falls back, and fails if not. A
	// connection no rule matches goes where macOS sends it. When a rule
	// names an address, a name is looked up once, as macOS does: the rules
	// are matched against the name with each of its addresses, and the
	// connection goes to an address they were matched against, one a rule
	// sends somewhere first, never one a rule blocks; a name that can't be
	// looked up fails. Changed rules apply to new connections. Exactly one
	// of Via, Upstream and Scene is set.
	Scene string `json:"scene,omitempty"`
	// Shared also takes connections from Config.Devices, on every IPv4
	// address of this Mac rather than on 127.0.0.1 alone. A shared proxy's
	// Upstream is on another machine: one on this Mac is refused, when saved
	// if its host is a loopback address, else when it is reached.
	Shared bool `json:"shared,omitempty"`
	// ChangeIP lets what connects to the port have Pith change the public
	// address of what the port goes out through, by asking the port itself:
	// see ProxyInfo. Off, only this Mac's user changes it, in Pith. A proxy
	// by a scene's rules has no one address to change, and can't have it on.
	ChangeIP bool `json:"changeIP,omitempty"`
}

// Device is another device, or a range of them, that may use the proxies
// that are Shared. A device not listed is refused, and reported in
// Status.Devices.
type Device struct {
	// Address is its IPv4 address as a shared proxy sees it connect, or a
	// range of them in CIDR of at least 8 bits: 192.0.2.10, 192.0.2.0/24.
	Address string `json:"address"`
	Name    string `json:"name,omitempty"` // the user's name for it
}

// Prefix is the addresses d covers: one for an address, a range's for a
// range, canonical. It fails for what Validate refuses: anything but IPv4, a
// loopback or unspecified address, or a range of less than 8 bits.
func (d Device) Prefix() (netip.Prefix, error) {
	s := strings.TrimSpace(d.Address)
	prefix, err := netip.ParsePrefix(s)
	if err != nil {
		addr, addrErr := netip.ParseAddr(s)
		if addrErr != nil {
			return netip.Prefix{}, &Error{Code: ErrInvalidConfig, Detail: "not an IP address or a range of them"}
		}
		prefix = netip.PrefixFrom(addr, addr.BitLen())
	}
	addr := prefix.Addr()
	if !addr.Is4() || addr.IsLoopback() || addr.IsUnspecified() || prefix.Bits() < 8 {
		return netip.Prefix{}, &Error{Code: ErrInvalidConfig, Detail: "not an IPv4 address of another device, or a range of at least 8 bits"}
	}
	return prefix.Masked(), nil
}

// ParseUpstream splits a Proxy.Upstream into its host, an IP address or a
// host name, and its port.
func ParseUpstream(s string) (host string, port int, err error) {
	bad := &Error{Code: ErrInvalidConfig, Detail: "not the host:port of a proxy"}
	host, portText, splitErr := net.SplitHostPort(s)
	if splitErr != nil {
		return "", 0, bad
	}
	port, convErr := strconv.Atoi(portText)
	if convErr != nil || port < 1 || port > 65535 || strconv.Itoa(port) != portText {
		return "", 0, bad
	}
	if addr, addrErr := netip.ParseAddr(host); addrErr == nil {
		if addr.IsUnspecified() || addr.IsMulticast() {
			return "", 0, bad
		}
		return host, port, nil
	}
	if !validHostName(host) {
		return "", 0, bad
	}
	return host, port, nil
}

// validHostName accepts the names DNS, mDNS and a VPN's name service take:
// letters, digits, hyphens and underscores in labels of at most 63 bytes.
func validHostName(host string) bool {
	host = strings.TrimSuffix(host, ".")
	if len(host) == 0 || len(host) > 253 {
		return false
	}
	for label := range strings.SplitSeq(host, ".") {
		if len(label) == 0 || len(label) > 63 || label[0] == '-' || label[len(label)-1] == '-' {
			return false
		}
		for _, c := range []byte(label) {
			if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '-' || c == '_') {
				return false
			}
		}
	}
	return true
}

// ProxyStatus describes one configured local listener, independently of the
// helper's state. Removed entries disappear; disabled entries have zero counters,
// no error and no listening time. A failed bind can be retried by SaveConfig.
type ProxyStatus struct {
	Port      int       `json:"port"`
	Listening bool      `json:"listening"` // local bind only, not proof of internet access
	Since     time.Time `json:"since,omitzero"`
	// Error reports listener or current interface/DNS availability problems,
	// or an Upstream that doesn't answer or refuses this Mac.
	// A target's connection refusal or DNS answer error is not a listener error.
	// Listening may remain true while the selected interface is unavailable.
	Error *Error `json:"error,omitempty"`
	// Open counts CONNECT-established streams, excluding pending handshakes.
	Open int `json:"open"`
	// Sent/Received are successfully relayed bytes since Since, excluding SOCKS
	// negotiation and DNS. Restarting/rebinding resets them. They are not part
	// of the helper's connection history or TUN traffic statistics.
	Sent     uint64 `json:"sent"`
	Received uint64 `json:"received"`
	// Upstream is what the Upstream says of itself when it is a port of
	// another Pith: nil for another kind of proxy, and until it has answered.
	Upstream *ProxyInfo `json:"upstream,omitempty"`
	// Changing: the public address of what the proxy goes out through is
	// being changed, whoever asked.
	Changing bool `json:"changing,omitempty"`
}

// DeviceStatus describes another device that connected to a shared proxy
// since Pith opened: one of Config.Devices with what it moved, or one that
// was refused, for the user to allow.
type DeviceStatus struct {
	Address string    `json:"address"` // its IP address, as the proxy saw it
	Allowed bool      `json:"allowed"` // one of Config.Devices covers it
	Seen    time.Time `json:"seen"`    // when it last connected
	Open    int       `json:"open"`    // its CONNECT-established streams
	// Sent/Received are the bytes relayed for it through every shared proxy
	// since it first connected.
	Sent     uint64 `json:"sent"`
	Received uint64 `json:"received"`
}

// ProxyInfoVersion is the version of what a proxy's port says of itself:
// ProxyInfo.Pith.
const ProxyInfoVersion = 1

// MaxProxyHops bounds the ports a request to a port is passed along: a port
// whose upstream is another Pith's port asks it in turn.
const MaxProxyHops = 4

// ProxyExit is the kind of thing a proxy goes out through.
type ProxyExit string

const (
	ExitInterface ProxyExit = "interface" // Proxy.Via
	ExitUpstream  ProxyExit = "upstream"  // Proxy.Upstream
	ExitScene     ProxyExit = "scene"     // Proxy.Scene
)

// ProxyInfo is what a proxy's port says of itself, as JSON, to an HTTP
// request made to the port rather than through it: one whose target is a
// path, as a server is sent, not a URL or the authority of a CONNECT.
//
//	GET /            answers with a ProxyInfo
//	POST /change-ip  changes the public address of what the port goes out
//	                 through, and answers with a ProxyIPChange once it has
//
// Whatever may connect to the port may GET; a POST also needs the port's
// ChangeIP. What fails answers with its HTTP status and {"error": Error}:
// 403 and ErrNotAllowed without ChangeIP, 501 and ErrUnsupported for an
// exit Pith can't change the address of, 502 and what went wrong for a
// change that failed. A request a web page made, which has an Origin field
// or a Sec-Fetch-Site other than "none", is refused with 403: the answers
// are for programs, and for a person who opens the address in a browser.
//
// A port whose Upstream is another Pith's port asks it the same way: it
// answers with what the upstream says, and passes a change on to it, through
// MaxProxyHops ports at most.
type ProxyInfo struct {
	Pith int       `json:"pith"` // ProxyInfoVersion: it tells Pith's answer from another server's
	Port int       `json:"port"`
	Exit ProxyExit `json:"exit"`
	// Interface is the interface the port goes out through, by the name its
	// Mac's user knows it by: empty unless Exit is ExitInterface.
	Interface string `json:"interface,omitempty"`
	// Online: the port listens, and what it goes out through is there. Its
	// interface is connected; or its upstream answers and, when that is
	// Pith's, is online itself. A port by a scene's rules is online while it
	// listens. It is not proof that the internet answers.
	Online bool `json:"online"`
	// PublicIPv4 and PublicIPv6 are the addresses the internet sees the
	// port's connections come from, as Pith's HTTPS test found them through
	// its interface at At, or as its upstream tells them. Pith looks them up
	// for a GET when it knows none, and again when they are older than the
	// request allows: "GET /?max-age=60", in seconds. Empty when they
	// couldn't be found, and for a port by a scene's rules, whose connections
	// leave from where each rule sends them.
	PublicIPv4 string    `json:"publicIPv4,omitempty"`
	PublicIPv6 string    `json:"publicIPv6,omitempty"`
	At         time.Time `json:"at,omitzero"`
	// CanChangeIP: a POST of /change-ip by whoever asked would be tried. The
	// port's ChangeIP is on, and its interface leads to a hotspot or a router
	// whose address Pith changes, or its upstream says the same of itself.
	CanChangeIP bool `json:"canChangeIP"`
	// Changing: the address is being changed now.
	Changing bool `json:"changing,omitempty"`
	// Upstream is the answer of the port's upstream, when Exit is
	// ExitUpstream and it is another Pith's port. Online, the addresses, At
	// and Changing above are then the upstream's.
	Upstream *ProxyInfo `json:"upstream,omitempty"`
}

// ProxyIPChange tells what changing the public address of what a proxy
// goes out through did: the addresses before, and the ones now. A router
// changes its IPv4 address alone.
type ProxyIPChange struct {
	Port       int       `json:"port"`
	BeforeIPv4 string    `json:"beforeIPv4,omitempty"`
	BeforeIPv6 string    `json:"beforeIPv6,omitempty"`
	PublicIPv4 string    `json:"publicIPv4,omitempty"`
	PublicIPv6 string    `json:"publicIPv6,omitempty"`
	At         time.Time `json:"at,omitzero"` // when the addresses now were found
	Seconds    float64   `json:"seconds"`     // it took, until the internet worked through it again
}

// CanProxy reports supported exit type, not current availability. Down physical
// interfaces remain selectable. VPN/TUN, loopback, bridges and peer transports
// cannot be proxy exits; rejecting them also excludes Pith's own capture utun.
// The client validates resolved interfaces with this predicate. An unresolved
// saved Via is allowed and reports ErrInterfaceUnavailable until it is present.
func (i Interface) CanProxy() bool {
	if i.Name == "" {
		return false
	}
	name := strings.ToLower(i.Name)
	for _, prefix := range []string{"lo", "utun", "ipsec", "ppp", "bridge", "gif", "stf", "awdl", "llw", "anpi", "tun", "tap", "vmenet"} {
		if strings.HasPrefix(name, prefix) {
			return false
		}
	}
	switch i.Kind {
	case KindWiFi, KindEthernet, KindCellular, KindThunderbolt:
		return true
	default:
		return false
	}
}
