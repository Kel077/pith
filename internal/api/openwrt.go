package api

import (
	"net/netip"
	"path"
	"strings"
	"unicode"
)

const DefaultOpenWrtCommand = "/usr/sbin/wan-rotate-ip"

// OpenWrtRouter names a WAN rotation service that is reachable over SSH from
// Via. Host can be an OpenSSH host alias. Empty User and IdentityFile use SSH
// configuration, and an empty Command uses DefaultOpenWrtCommand.
type OpenWrtRouter struct {
	Via          InterfaceRef `json:"via"`
	Host         string       `json:"host"`
	User         string       `json:"user,omitempty"`
	IdentityFile string       `json:"identityFile,omitempty"`
	Command      string       `json:"command,omitempty"`
}

func (r OpenWrtRouter) Validate() error {
	bad := func(detail string) error {
		return &Error{Code: ErrInvalidConfig, Detail: "OpenWrt: " + detail}
	}
	if r.Via.Name == "" {
		return bad("interface is required")
	}
	addr, err := netip.ParseAddr(r.Host)
	if err == nil {
		if addr.IsUnspecified() || addr.IsMulticast() || addr.Zone() != "" {
			return bad("host must be a unicast address or SSH alias")
		}
	} else if !validHostName(r.Host) {
		return bad("host must be an address, DNS name or SSH alias")
	}
	if r.User != "" && (len(r.User) > 64 || !validHostName(r.User)) {
		return bad("invalid SSH user")
	}
	if r.IdentityFile != "" && !absoluteSSHPath(strings.TrimPrefix(r.IdentityFile, "~")) {
		return bad("identity file must be an absolute path or begin with ~/")
	}
	if r.Command != "" && !absoluteSSHPath(r.Command) {
		return bad("command must be an absolute path on the router")
	}
	return nil
}

func absoluteSSHPath(s string) bool {
	return len(s) <= 1024 && path.IsAbs(s) && path.Clean(s) != "/" &&
		strings.IndexFunc(s, unicode.IsControl) == -1 && !strings.HasSuffix(s, "/")
}

// OpenWrtIndex finds an interface's router, or -1. A configured BSD name
// wins over a reference that followed a uniquely identified adapter.
func OpenWrtIndex(routers []OpenWrtRouter, links []Interface, name string) int {
	index, matches := -1, 0
	for i, r := range routers {
		if link := r.Via.Resolve(links); link != nil && link.Name == name {
			if r.Via.Name == name {
				return i
			}
			index, matches = i, matches+1
		}
	}
	if matches == 1 {
		return index
	}
	return -1
}

// OpenWrtStatus is the address delegation and failover state reported by the
// configured router. IPv6Prefix may be returned without a prefix length.
type OpenWrtStatus struct {
	IPv6Prefix  string `json:"ipv6Prefix,omitempty"`
	WANState    string `json:"wanState,omitempty"`
	BackupState string `json:"backupState,omitempty"`
}

// WANRotation records an OpenWrt router's leases and the result of changing
// its WAN MAC. The addresses in a public probe describe the Mac's actual exit.
type WANRotation struct {
	BeforeIPv4       string  `json:"beforeIPv4,omitempty"`
	IPv4             string  `json:"ipv4,omitempty"`
	BeforeIPv6Prefix string  `json:"beforeIPv6Prefix,omitempty"`
	IPv6Prefix       string  `json:"ipv6Prefix,omitempty"`
	MACChanged       bool    `json:"macChanged"`
	WANState         string  `json:"wanState,omitempty"`
	BackupState      string  `json:"backupState,omitempty"`
	Seconds          float64 `json:"seconds"`
}
