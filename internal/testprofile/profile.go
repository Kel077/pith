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

const (
	FullIPv4 = "full-ipv4"
	HostIPv6 = "host-ipv6"
)

type Profile struct {
	Mode       string        `json:"mode"`
	Lease      time.Duration `json:"lease"`
	Target     string        `json:"target"`
	ServerName string        `json:"serverName"`
	DNSHosts   []netip.Addr  `json:"dnsHosts,omitempty"`
}

func (p Profile) Validate() error {
	if p.Mode != FullIPv4 && p.Mode != HostIPv6 {
		return errors.New("test profile must be full-ipv4 or host-ipv6")
	}
	maximum := 120 * time.Second
	if p.Mode == HostIPv6 {
		maximum = 60 * time.Second
	}
	if p.Lease < time.Second || p.Lease > maximum {
		return errors.New("test profile lease exceeds its mode's limits (full-ipv4: 120s, host-ipv6: 60s)")
	}
	target, err := netip.ParseAddrPort(p.Target)
	if err != nil || target.String() != p.Target || target.Port() == 0 || target.Addr().Is4In6() || target.Addr().Zone() != "" || !target.Addr().IsGlobalUnicast() || target.Addr().IsPrivate() || netip.MustParsePrefix("100.64.0.0/10").Contains(target.Addr()) || target.Addr().Is6() != (p.Mode == HostIPv6) {
		return errors.New("test profile requires a canonical literal public TLS endpoint in its selected address family")
	}
	name, err := api.ParseTarget(p.ServerName)
	if err != nil || !name.IsDomain() || name.Subdomains || name.String() != p.ServerName {
		return errors.New("test profile requires a plain lowercase TLS certificate name")
	}
	if len(p.DNSHosts) > 16 {
		return errors.New("test profile allows at most sixteen reviewed LAN DNS hosts")
	}
	if p.Mode == HostIPv6 && len(p.DNSHosts) != 0 {
		return errors.New("host-ipv6 cannot add DNS capture routes")
	}
	for i, addr := range p.DNSHosts {
		if !addr.Is4() || !addr.IsGlobalUnicast() || netip.MustParsePrefix("100.64.0.0/10").Contains(addr) || slices.Contains(p.DNSHosts[:i], addr) {
			return errors.New("test DNS hosts must be distinct unicast IPv4 addresses outside shared address space")
		}
	}
	return nil
}

func (p Profile) Prefixes() []netip.Prefix {
	if p.Mode == HostIPv6 {
		target, err := netip.ParseAddrPort(p.Target)
		if err != nil {
			return nil
		}
		return []netip.Prefix{netip.PrefixFrom(target.Addr(), 128)}
	}
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
	if settings.IPv6 != (p.Mode == HostIPv6) || !settings.SafeStart {
		return &api.Error{Code: api.ErrInvalidConfig, Detail: "test helper requires SafeStart and the capture profile's fixed IP family"}
	}
	return nil
}
