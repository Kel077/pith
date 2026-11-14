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
// and HTTP on the same port: CONNECT, and requests for http:// URLs.
// It supports literal IPv4/IPv6 and names resolved through Via's own DNS.
// It never uses another interface as fallback and does not depend on TUN.
type Proxy struct {
	Port    int          `json:"port"`         // 1024..65535; unique even when disabled
	Via     InterfaceRef `json:"via,omitzero"` // one supported physical exit, resolved by Resolve
	Enabled bool         `json:"enabled"`
	// Upstream replaces Via: the "host:port" of a SOCKS5 proxy without
	// authentication that every connection goes out through, as a port
	// another Mac's Pith shares. It is reached as macOS routes, and resolves
	// the names it is asked for itself. Exactly one of Via and Upstream is set.
	Upstream string `json:"upstream,omitempty"`
	// Shared also takes connections from Config.Devices, on every IPv4
	// address of this Mac rather than on 127.0.0.1 alone.
	Shared bool `json:"shared,omitempty"`
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
