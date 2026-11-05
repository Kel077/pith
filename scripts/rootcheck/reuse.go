package main

import (
	"errors"
	"net/netip"
	"reflect"
	"slices"
	"strings"

	"github.com/Kel077/pith/internal/api"
	"github.com/Kel077/pith/internal/engine/capture"
)

type interfaceKey struct {
	Name  string
	Index int
}

// Only independently matched whole-VPN migrations justify a new reuse. Full
// snapshots and these normalized matches remain in the private cleanup report.
type vpnMigration struct {
	From      interfaceKey    `json:"from"`
	To        interfaceKey    `json:"to"`
	Addresses []netip.Prefix  `json:"matchedAddresses"`
	Routes    []capture.Route `json:"matchedRoutes"`
}

type vpnShape struct {
	Interface api.Interface
	Addresses []netip.Prefix
	Routes    []capture.Route
}

func (s snapshot) completeAddresses() bool {
	if len(s.InterfaceAddresses) != len(s.InterfaceIndices) {
		return false
	}
	for name := range s.InterfaceIndices {
		addresses, ok := s.InterfaceAddresses[name]
		if !ok {
			return false
		}
		for _, p := range addresses {
			if !p.IsValid() || p.Addr().Is4In6() {
				return false
			}
		}
	}
	return true
}

func captureSubnets(id api.CaptureIdentity) ([]netip.Prefix, error) {
	var result []netip.Prefix
	if len(id.TunnelSubnets) < 1 || len(id.TunnelSubnets) > 2 {
		return nil, errors.New("capture has no complete tunnel-subnet evidence")
	}
	seen4, seen6 := false, false
	for _, raw := range id.TunnelSubnets {
		p, err := netip.ParsePrefix(raw)
		if err != nil || p != p.Masked() || p.Addr().Is4In6() {
			return nil, errors.New("invalid capture subnet")
		}
		if p.Addr().Is4() && p.Bits() == 30 && !seen4 {
			seen4 = true
		} else if p.Addr().Is6() && p.Bits() == 126 && !seen6 {
			seen6 = true
		} else {
			return nil, errors.New("invalid capture subnet family or size")
		}
		result = append(result, p)
	}
	if !seen4 {
		return nil, errors.New("capture IPv4 subnet is missing")
	}
	return result, nil
}

func (s snapshot) noCaptureAddresses(ids []api.CaptureIdentity) error {
	if !s.completeAddresses() {
		return errors.New("interface reuse requires all assigned interface addresses")
	}
	for _, id := range ids {
		subnets, err := captureSubnets(id)
		if err != nil {
			return err
		}
		for _, subnet := range subnets {
			for _, addresses := range s.InterfaceAddresses {
				for _, p := range addresses {
					if subnet.Contains(p.Addr()) {
						return errors.New("a Pith tunnel address remains")
					}
				}
			}
			for _, route := range s.Routes {
				if subnet.Contains(route.Gateway) || subnet.Contains(route.Prefix.Addr()) && route.Prefix.Bits() >= subnet.Bits() {
					return errors.New("a Pith tunnel-subnet route remains")
				}
			}
		}
	}
	return nil
}

// A migration matches every global assigned prefix and route, not merely a
// shared default or label. Link-local address changes are retained in snapshots
// but are not stable VPN identity. Any Pith subnet there still rejects reuse.
func (s snapshot) vpn(key interfaceKey) (vpnShape, bool) {
	if s.InterfaceIndices[key.Name] != key.Index || !strings.HasPrefix(key.Name, "utun") || !s.completeAddresses() {
		return vpnShape{}, false
	}
	var shape vpnShape
	for _, iface := range s.Interfaces {
		if iface.Name != key.Name {
			continue
		}
		if iface.Kind != api.KindVPN || !iface.Up {
			return vpnShape{}, false
		}
		shape.Interface = iface
		shape.Interface.Name, shape.Interface.Label = "", ""
		shape.Interface.IPv4 = slices.Clone(iface.IPv4)
		slices.Sort(shape.Interface.IPv4)
		shape.Interface.IPv6 = slices.Clone(iface.IPv6)
		slices.Sort(shape.Interface.IPv6)
	}
	if !shape.Interface.Up {
		return vpnShape{}, false
	}
	for _, p := range s.InterfaceAddresses[key.Name] {
		if p.Addr().IsGlobalUnicast() {
			shape.Addresses = append(shape.Addresses, p)
		}
	}
	slices.SortFunc(shape.Addresses, func(a, b netip.Prefix) int { return strings.Compare(a.String(), b.String()) })
	if len(shape.Addresses) == 0 {
		return vpnShape{}, false
	}
	// Cross-check the API projection against the complete independent inventory.
	var projected []string
	for _, p := range shape.Addresses {
		projected = append(projected, p.Addr().String())
	}
	slices.Sort(projected)
	apiAddresses := append(slices.Clone(shape.Interface.IPv4), shape.Interface.IPv6...)
	slices.Sort(apiAddresses)
	if !slices.Equal(projected, apiAddresses) {
		return vpnShape{}, false
	}
	for _, r := range s.Routes {
		if r.Interface == key.Name && r.Index == key.Index {
			r.Interface = ""
			r.Index = 0
			shape.Routes = append(shape.Routes, r)
		}
	}
	shape.Routes = routeDifference(shape.Routes, nil)
	return shape, len(shape.Routes) > 0
}

func touched(key interfaceKey, ids []api.CaptureIdentity) []api.CaptureIdentity {
	var matches []api.CaptureIdentity
	for _, id := range ids {
		if id.Interface == key.Name || id.Index == key.Index {
			matches = append(matches, id)
		}
	}
	return matches
}

// A later accepted baseline can already contain an independently proven foreign
// reuse. Older completed captures cannot taint that baseline forever. A capture
// after this baseline (or an unknown end time) cannot use this exception.
func (s *session) baselineForeign(key interfaceKey) bool {
	ids := touched(key, s.Ownership.Captures)
	if len(ids) == 0 {
		return true
	}
	if s.Original.At.IsZero() {
		return false
	}
	for _, id := range ids {
		if id.EndedAt.IsZero() || !id.EndedAt.Before(s.Original.At) {
			return false
		}
	}
	_, ok := s.Original.vpn(key)
	return ok && s.Original.noCaptureAddresses(ids) == nil
}

func (s *session) permittedReuse(after snapshot, st api.Status) (map[interfaceKey]bool, map[interfaceKey]bool, []vpnMigration, error) {
	beforeOK, afterOK := map[interfaceKey]bool{}, map[interfaceKey]bool{}
	for name, index := range s.Original.InterfaceIndices {
		key := interfaceKey{name, index}
		if !s.baselineForeign(key) {
			continue
		}
		beforeOK[key] = true
		if len(touched(key, s.Ownership.Captures)) > 0 {
			a, aok := s.Original.vpn(key)
			b, bok := after.vpn(key)
			if aok && bok && reflect.DeepEqual(a, b) && after.noCaptureAddresses(s.Ownership.Captures) == nil {
				afterOK[key] = true
			}
		}
	}
	var migrations []vpnMigration
	used := map[interfaceKey]bool{}
	for name, index := range after.InterfaceIndices {
		key := interfaceKey{name, index}
		ids := touched(key, s.Ownership.Captures)
		if len(ids) == 0 || afterOK[key] {
			continue
		}
		if st.LastCapture == nil || st.LastCapture.EndedAt.IsZero() || st.LastCapture.EndedAt.After(after.At) || !s.knownCapture(st.LastCapture) || st.LastCapture.Token != s.Ownership.Captures[len(s.Ownership.Captures)-1].Token {
			return nil, nil, nil, errors.New("interface reuse requires the latest capture's confirmed closure")
		}
		for _, id := range ids {
			ended := id.EndedAt
			if sameCapture(id, *st.LastCapture) {
				ended = st.LastCapture.EndedAt
			}
			if ended.IsZero() || after.At.IsZero() || ended.After(after.At) {
				return nil, nil, nil, errors.New("a reused capture lifetime has no earlier confirmed end")
			}
		}
		if err := after.noCaptureAddresses(s.Ownership.Captures); err != nil {
			return nil, nil, nil, err
		}
		shape, ok := after.vpn(key)
		if !ok {
			return nil, nil, nil, errors.New("reused interface is not a fully observed active VPN")
		}
		var candidates []interfaceKey
		for oldName, oldIndex := range s.Original.InterfaceIndices {
			old := interfaceKey{oldName, oldIndex}
			if old == key || !beforeOK[old] || after.InterfaceIndices[oldName] != 0 {
				continue
			}
			prior, ok := s.Original.vpn(old)
			if ok && reflect.DeepEqual(prior, shape) {
				candidates = append(candidates, old)
			}
		}
		if len(candidates) != 1 || used[candidates[0]] {
			return nil, nil, nil, errors.New("reused interface lacks a unique whole-VPN migration")
		}
		// Cloning the old identity onto two interfaces is ambiguous, not migration.
		for otherName, otherIndex := range after.InterfaceIndices {
			other := interfaceKey{otherName, otherIndex}
			if other == key {
				continue
			}
			candidate, ok := after.vpn(other)
			if ok && reflect.DeepEqual(candidate, shape) {
				return nil, nil, nil, errors.New("VPN identity appears on multiple interfaces")
			}
		}
		used[candidates[0]], afterOK[key] = true, true
		migrations = append(migrations, vpnMigration{From: candidates[0], To: key, Addresses: shape.Addresses, Routes: shape.Routes})
	}
	slices.SortFunc(migrations, func(a, b vpnMigration) int { return strings.Compare(a.To.Name, b.To.Name) })
	return beforeOK, afterOK, migrations, nil
}
