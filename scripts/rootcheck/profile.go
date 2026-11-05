package main

import (
	"errors"
	"net/netip"
	"slices"
	"time"

	"github.com/Kel077/pith/internal/engine/capture"
)

func (s *session) captureMode() string {
	if s.TestProfile == nil {
		return "host-ipv4"
	}
	return s.TestProfile.Mode
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
	return s.TestProfile.Validate()
}

func (s *session) validateSnapshotScope(before snapshot) error {
	scopes, err := s.scopes()
	if err != nil {
		return err
	}
	if s.TestProfile == nil {
		_, err := capture.Build(before.Routes, nil, false, scopes)
		return err
	}
	plan, err := capture.Build(before.Routes, snapshotDNS(before), false, nil)
	if err != nil {
		return err
	}
	if len(plan.Prefixes) != len(scopes) {
		return errors.New("full capture DNS scope changed; prepare and review a new installation")
	}
	for _, prefix := range scopes {
		if !slices.Contains(plan.Prefixes, prefix) {
			return errors.New("full capture DNS scope changed; prepare and review a new installation")
		}
	}
	return nil
}

func (s *session) checkTimeout() time.Duration {
	if s.TestProfile != nil {
		// Leave time for the controller's normal Stop before the helper watchdog.
		return s.TestProfile.Lease - 10*time.Second
	}
	return 3 * time.Minute
}
