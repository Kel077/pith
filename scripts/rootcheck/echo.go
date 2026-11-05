package main

import (
	"errors"
	"net/netip"
)

// Connection attribution, target, rule, outcome and interface must already have
// been verified. Public echoes supplement that evidence; carrier NAT exits can
// change between requests without any adapter or routing change.
func (s *session) checkEcho(entry *checkCase, iface string) error {
	entry.ExpectedIP = s.baselineIP(iface)
	if entry.ExpectedIP == "" {
		return nil // This endpoint's preflight did not offer an IP echo.
	}
	actual, err := netip.ParseAddr(entry.TLS.PublicIP)
	expected, expectedErr := netip.ParseAddr(entry.ExpectedIP)
	if err != nil || expectedErr != nil || !actual.IsGlobalUnicast() || actual.IsPrivate() || actual.BitLen() != expected.BitLen() {
		return errors.New("public-IP echo is missing, invalid, or from a different address family")
	}
	if actual == expected {
		return nil
	}
	for _, probe := range s.Preflight {
		if probe.Interface != iface && probe.TLS.PublicIP == actual.String() {
			return errors.New("public-IP echo matches another exit's preflight instead of the verified interface")
		}
	}
	entry.ExitIPDrift = true
	entry.EvidenceNote = "Exit IP drift: the verified connection used the expected interface; its public echo changed and matches no other preflight exit."
	return nil
}
