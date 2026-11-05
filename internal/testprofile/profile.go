// Package testprofile describes immutable, explicitly reviewed helper test modes.
// It has no system or network side effects and is separate from the public API.
package testprofile

import (
	"errors"
	"net/netip"
	"slices"
	"time"

	"github.com/Kel077/pith/internal/api"
)

const FullIPv4 = "full-ipv4"

type Profile struct {
	Mode       string        `json:"mode"`
	Lease      time.Duration `json:"lease"`
	Target     string        `json:"target"`
	ServerName string        `json:"serverName"`
	DNSHosts   []netip.Addr  `json:"dnsHosts,omitempty"`
}

func (p Profile) Validate() error {
	if p.Mode != FullIPv4 || p.Lease < time.Second || p.Lease > 120*time.Second {
		return errors.New("full-ipv4 test profile requires a lease from 1s through 120s")
	}
	target, err := netip.ParseAddrPort(p.Target)
	if err != nil || target.Port() == 0 || !target.Addr().Is4() || !target.Addr().IsGlobalUnicast() || target.Addr().IsPrivate() || netip.MustParsePrefix("100.64.0.0/10").Contains(target.Addr()) {
		return errors.New("full-ipv4 test profile requires a literal public IPv4 TLS endpoint")
	}
	name, err := api.ParseTarget(p.ServerName)
	if err != nil || !name.IsDomain() || name.Subdomains || name.String() != p.ServerName {
		return errors.New("test profile requires a plain lowercase TLS certificate name")
	}
	if len(p.DNSHosts) > 16 {
		return errors.New("test profile allows at most sixteen reviewed LAN DNS hosts")
	}
	for i, addr := range p.DNSHosts {
		if !addr.Is4() || !addr.IsGlobalUnicast() || netip.MustParsePrefix("100.64.0.0/10").Contains(addr) || slices.Contains(p.DNSHosts[:i], addr) {
			return errors.New("test DNS hosts must be distinct unicast IPv4 addresses outside shared address space")
		}
	}
	return nil
}

func (p Profile) Prefixes() []netip.Prefix {
	result := []netip.Prefix{netip.MustParsePrefix("0.0.0.0/1"), netip.MustParsePrefix("128.0.0.0/1")}
	for _, addr := range p.DNSHosts {
		result = append(result, netip.PrefixFrom(addr, 32))
	}
	return result
}

func (p Profile) Arguments() ([]string, error) {
	if err := p.Validate(); err != nil {
		return nil, err
	}
	args := []string{"--safe-target", p.Target, "--safe-server-name", p.ServerName, "--test-profile", p.Mode, "--test-lease", p.Lease.String()}
	for _, addr := range p.DNSHosts {
		args = append(args, "--test-dns-host", addr.String())
	}
	return args, nil
}

func (p Profile) ValidateSettings(settings api.Settings) error {
	if settings.IPv6 || !settings.SafeStart {
		return &api.Error{Code: api.ErrInvalidConfig, Detail: "full-ipv4 test helper requires IPv6 disabled and SafeStart enabled"}
	}
	return nil
}
