package main

import (
	"errors"
	"net/netip"
	"slices"
	"time"

	"github.com/Kel077/pith/internal/api"
	"github.com/Kel077/pith/internal/engine/capture"
	"github.com/Kel077/pith/internal/testprofile"
)

func (s *session) captureMode() string {
	if s.TestProfile == nil {
		return "host-ipv4"
	}
	return s.TestProfile.Mode
}

func (s *session) ipv6() bool {
	return s.TestProfile != nil && s.TestProfile.Mode == testprofile.HostIPv6
}

func (s *session) tcpNetwork() string {
	if s.ipv6() {
		return "tcp6"
	}
	return "tcp4"
}

func (s *session) usableInterface(iface *api.Interface) bool {
	if iface == nil || !iface.Up {
		return false
	}
	if !s.ipv6() {
		return len(iface.IPv4) > 0
	}
	for _, raw := range iface.IPv6 {
		if addr, err := netip.ParseAddr(raw); err == nil && addr.Is6() && !addr.Is4In6() && addr.Zone() == "" && addr.IsGlobalUnicast() && !addr.IsPrivate() {
			return true
		}
	}
	return false
}

func snapshotDNS(before snapshot) []netip.Addr {
	var result []netip.Addr
	for _, iface := range before.Interfaces {
		for _, raw := range iface.DNS {
			if addr, err := netip.ParseAddr(raw); err == nil {
				result = append(result, addr)
			}
		}
	}
	return result
}

// Only prepare may freeze the full capture set. Rebaseline checks this set and
// cannot silently adopt extra DNS routes or a different installation mode.
func (s *session) freezeFullScope(before snapshot) error {
	plan, err := capture.Build(before.Routes, snapshotDNS(before), false, nil)
	if err != nil {
		return err
	}
	s.TestProfile.DNSHosts = nil
	for _, prefix := range plan.Prefixes {
		if prefix.Bits() == 32 {
			s.TestProfile.DNSHosts = append(s.TestProfile.DNSHosts, prefix.Addr())
		}
	}
	slices.SortFunc(s.TestProfile.DNSHosts, func(a, b netip.Addr) int { return a.Compare(b) })
	s.SkippedDNS = slices.Clone(plan.SkippedDNS)
	return s.TestProfile.Validate()
}

func (s *session) validateSnapshotScope(before snapshot) error {
	_, err := s.snapshotPlan(before)
	return err
}

func (s *session) snapshotPlan(before snapshot) (capture.Plan, error) {
	scopes, err := s.scopes()
	if err != nil {
		return capture.Plan{}, err
	}
	if s.TestProfile == nil || s.ipv6() {
		return capture.Build(before.Routes, nil, s.ipv6(), scopes)
	}
	plan, err := capture.Build(before.Routes, snapshotDNS(before), false, nil)
	if err != nil {
		return capture.Plan{}, err
	}
	if len(plan.Prefixes) != len(scopes) {
		return capture.Plan{}, errors.New("full capture DNS scope changed; prepare and review a new installation")
	}
	for _, prefix := range scopes {
		if !slices.Contains(plan.Prefixes, prefix) {
			return capture.Plan{}, errors.New("full capture DNS scope changed; prepare and review a new installation")
		}
	}
	return plan, nil
}

func (s *session) checkTimeout() time.Duration {
	if s.TestProfile != nil {
		// Leave time for the controller's normal Stop before the helper watchdog.
		return s.TestProfile.Lease - 10*time.Second
	}
	return 3 * time.Minute
}
