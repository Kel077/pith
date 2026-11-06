package api

import (
	"strings"
	"time"
)

// MaxProxies bounds configured local listeners, including disabled entries.
const MaxProxies = 32

// Proxy exposes SOCKS5 no-auth CONNECT on 127.0.0.1:Port in the App process.
// It supports literal IPv4/IPv6 and names resolved through Via's own DNS.
// It never uses another interface as fallback and does not depend on TUN.
type Proxy struct {
	Port    int          `json:"port"` // 1024..65535; unique even when disabled
	Via     InterfaceRef `json:"via"`  // one supported physical exit, resolved by Resolve
	Enabled bool         `json:"enabled"`
}

// ProxyStatus describes one configured local listener, independently of the
// helper's state. Removed entries disappear; disabled entries have zero counters,
// no error and no listening time. A failed bind can be retried by SaveConfig.
type ProxyStatus struct {
	Port      int       `json:"port"`
	Listening bool      `json:"listening"` // local bind only, not proof of internet access
	Since     time.Time `json:"since,omitzero"`
	// Error reports listener or current interface/DNS availability problems.
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
