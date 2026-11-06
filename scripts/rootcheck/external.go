package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/netip"
	"reflect"
	"slices"
	"strings"
	"time"

	"github.com/Kel077/pith/internal/api"
	"github.com/Kel077/pith/internal/engine/capture"
)

const externalProof = "external-change"

var errExternalChange = errors.New("Pith capture is absent, but external routes or interfaces changed; review the private differences and use rebaseline --accept-external-change explicitly")

// Complete is false for legacy sessions or any capture attempt whose identity
// could not be recorded. Pending survives controller death. Neither condition
// can be repaired by accepting external changes or by editing a baseline.
type captureHistory struct {
	Complete bool                  `json:"complete"`
	Pending  bool                  `json:"pending"`
	Captures []api.CaptureIdentity `json:"captures,omitempty"`
}

type externalChange struct {
	RoutesAdded       []capture.Route           `json:"routesAdded,omitempty"`
	RoutesRemoved     []capture.Route           `json:"routesRemoved,omitempty"`
	InterfacesAdded   []api.Interface           `json:"interfacesAdded,omitempty"`
	InterfacesRemoved []api.Interface           `json:"interfacesRemoved,omitempty"`
	IndicesAdded      map[string]int            `json:"indicesAdded,omitempty"`
	IndicesRemoved    map[string]int            `json:"indicesRemoved,omitempty"`
	Migrations        []vpnMigration            `json:"verifiedVPNMigrations,omitempty"`
	AddressesAdded    map[string][]netip.Prefix `json:"addressesAdded,omitempty"`
	AddressesRemoved  map[string][]netip.Prefix `json:"addressesRemoved,omitempty"`
}

func (s *session) validCapture(id *api.CaptureIdentity) bool {
	if id == nil || id.Token == "" || id.BootSession == "" || id.Index <= 0 || !strings.HasPrefix(id.Interface, "utun") || len(id.Interface) == 4 || strings.IndexFunc(id.Interface[4:], func(r rune) bool { return r < '0' || r > '9' }) >= 0 {
		return false
	}
	scopes, err := s.scopes()
	if err != nil || len(scopes) != len(id.Prefixes) {
		return false
	}
	var actual []netip.Prefix
	for _, raw := range id.Prefixes {
		p, err := netip.ParsePrefix(raw)
		if err != nil || !slices.Contains(scopes, p) || slices.Contains(actual, p) {
			return false
		}
		actual = append(actual, p)
	}
	return true
}

func (s *session) knownCapture(id *api.CaptureIdentity) bool {
	if id == nil {
		return true
	} // A restarted helper is idle with no in-memory history.
	return slices.ContainsFunc(s.Ownership.Captures, func(old api.CaptureIdentity) bool { return sameCapture(old, *id) })
}

func sameCapture(a, b api.CaptureIdentity) bool {
	a.EndedAt, b.EndedAt = time.Time{}, time.Time{}
	return reflect.DeepEqual(a, b)
}

// Retain confirmed closure across later captures. Only an authenticated idle
// status with the exact immutable receipt may advance this historical fact.
func (s *session) rememberEnded(st api.Status) {
	if idleReviewedStatus(s, st) != nil || st.LastCapture == nil || st.LastCapture.EndedAt.IsZero() {
		return
	}
	for i, old := range s.Ownership.Captures {
		if sameCapture(old, *st.LastCapture) && old.EndedAt.IsZero() {
			s.Ownership.Captures[i].EndedAt = st.LastCapture.EndedAt
		}
	}
}

func (s *session) startWithIdentity(ctx context.Context, c api.Service, scene string, previous *api.CaptureIdentity, ops rootcheckOps) error {
	if s.Ownership.Pending || !s.knownCapture(previous) {
		s.Ownership.Complete = false
	}
	s.Ownership.Pending = true
	if err := ops.save(s); err != nil {
		return err
	}
	startErr := c.Start(ctx, scene)
	// Even a canceled Start may have prepared and removed a utun. Read its
	// receipt with an independent bound before publishing any cleanup proof.
	receiptCtx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	st, receiptErr := c.Status(receiptCtx)
	if receiptErr == nil {
		receiptErr = ops.verifyInstalled(s)
	}
	if receiptErr == nil && (!st.Helper.Installed || !st.Helper.Running || st.Helper.Outdated || !s.validCapture(st.LastCapture) || previous != nil && st.LastCapture.Token == previous.Token || s.knownCapture(st.LastCapture)) {
		receiptErr = errors.New("Start did not provide a new, reviewed capture identity")
	}
	if receiptErr != nil {
		s.Ownership.Complete = false
	} else {
		id := *st.LastCapture
		id.Prefixes = slices.Clone(id.Prefixes)
		id.TunnelSubnets = slices.Clone(id.TunnelSubnets)
		s.Ownership.Captures = append(s.Ownership.Captures, id)
	}
	s.Ownership.Pending = false
	saveErr := ops.save(s)
	if startErr != nil {
		return errors.Join(startErr, saveErr)
	}
	return errors.Join(receiptErr, saveErr)
}

func snapshotInventory(s snapshot) error {
	if s.BootSession == "" || len(s.InterfaceIndices) == 0 {
		return errors.New("incomplete boot or interface-index evidence")
	}
	names, indices := map[string]bool{}, map[int]bool{}
	for name, index := range s.InterfaceIndices {
		if name == "" || index <= 0 || indices[index] {
			return errors.New("ambiguous complete interface inventory")
		}
		indices[index] = true
	}
	for _, iface := range s.Interfaces {
		index := s.InterfaceIndices[iface.Name]
		if iface.Name == "" || index <= 0 || names[iface.Name] {
			return errors.New("ambiguous interface inventory")
		}
		names[iface.Name] = true
	}
	for _, route := range s.Routes {
		if !route.Prefix.IsValid() || route.Index <= 0 || s.InterfaceIndices[route.Interface] != route.Index {
			return errors.New("route/interface identity changed during snapshot")
		}
	}
	return nil
}

// externalDifferences never removes or adopts anything. It classifies only
// changes outside capture ownership, with independent evidence for any reuse.
func (s *session) externalDifferences(after snapshot, status api.Status) (*externalChange, error) {
	if err := idleReviewedStatus(s, status); err != nil {
		return nil, err
	}
	if !s.knownCapture(status.LastCapture) {
		return nil, errors.New("complete capture ownership history is required for external-change evidence")
	}
	if err := s.externalSnapshotHistory(after); err != nil {
		return nil, err
	}
	beforeOK, afterOK, migrations, err := s.permittedReuse(after, status)
	if err != nil {
		return nil, err
	}
	return s.classifyExternalDifferences(after, beforeOK, afterOK, migrations)
}

func (s *session) externalSnapshotHistory(after snapshot) error {
	if !s.Ownership.Complete || s.Ownership.Pending || len(s.Ownership.Captures) == 0 {
		return errors.New("complete capture ownership history is required for external-change evidence")
	}
	if err := snapshotInventory(s.Original); err != nil {
		return err
	}
	if err := snapshotInventory(after); err != nil {
		return err
	}
	if s.Original.BootSession != after.BootSession {
		return errors.New("boot session changed")
	}
	for _, id := range s.Ownership.Captures {
		if !s.validCapture(&id) || id.BootSession != after.BootSession {
			return errors.New("invalid capture ownership history")
		}
	}
	return nil
}

// Both callers validate history and snapshots first. Only the installed-helper
// path may supply independently proven interface-reuse exceptions.
func (s *session) classifyExternalDifferences(after snapshot, beforeOK, afterOK map[interfaceKey]bool, migrations []vpnMigration) (*externalChange, error) {
	ownedNames, ownedIndices := map[string]bool{}, map[int]bool{}
	for _, id := range s.Ownership.Captures {
		ownedNames[id.Interface], ownedIndices[id.Index] = true, true
	}
	for name, index := range after.InterfaceIndices {
		if (ownedNames[name] || ownedIndices[index]) && !afterOK[interfaceKey{name, index}] {
			return nil, errors.New("a Pith capture interface or reused name/index remains")
		}
	}
	scopes, _ := s.scopes()
	for _, route := range after.Routes {
		if (ownedNames[route.Interface] || ownedIndices[route.Index]) && !afterOK[interfaceKey{route.Interface, route.Index}] || slices.Contains(scopes, route.Prefix) {
			return nil, errors.New("a Pith interface route or exact capture prefix remains")
		}
	}
	diff := &externalChange{RoutesAdded: routeDifference(after.Routes, s.Original.Routes), RoutesRemoved: routeDifference(s.Original.Routes, after.Routes), IndicesAdded: map[string]int{}, IndicesRemoved: map[string]int{}}
	diff.Migrations = migrations
	if s.Original.completeAddresses() && after.completeAddresses() {
		diff.AddressesAdded = addressDifference(after.InterfaceAddresses, s.Original.InterfaceAddresses)
		diff.AddressesRemoved = addressDifference(s.Original.InterfaceAddresses, after.InterfaceAddresses)
	}
	for _, part := range []struct {
		routes  []capture.Route
		allowed map[interfaceKey]bool
	}{{diff.RoutesAdded, afterOK}, {diff.RoutesRemoved, beforeOK}} {
		for _, route := range part.routes {
			if (ownedNames[route.Interface] || ownedIndices[route.Index]) && !part.allowed[interfaceKey{route.Interface, route.Index}] || slices.Contains(scopes, route.Prefix) {
				return nil, errors.New("route differences touch an unproven capture identity or prefix")
			}
		}
	}
	for _, pair := range []struct {
		from, to snapshot
		out      *[]api.Interface
		indices  map[string]int
		allowed  map[interfaceKey]bool
	}{
		{after, s.Original, &diff.InterfacesAdded, diff.IndicesAdded, afterOK}, {s.Original, after, &diff.InterfacesRemoved, diff.IndicesRemoved, beforeOK},
	} {
		for _, iface := range pair.from.Interfaces {
			unchanged := slices.ContainsFunc(pair.to.Interfaces, func(other api.Interface) bool {
				return other.Name == iface.Name && pair.from.InterfaceIndices[iface.Name] == pair.to.InterfaceIndices[other.Name] && sameInterfaces([]api.Interface{iface}, []api.Interface{other})
			})
			if unchanged {
				continue
			}
			if (ownedNames[iface.Name] || ownedIndices[pair.from.InterfaceIndices[iface.Name]]) && !pair.allowed[interfaceKey{iface.Name, pair.from.InterfaceIndices[iface.Name]}] {
				return nil, errors.New("interface differences touch a past Pith identity")
			}
			*pair.out = append(*pair.out, iface)
			pair.indices[iface.Name] = pair.from.InterfaceIndices[iface.Name]
		}
		slices.SortFunc(*pair.out, func(a, b api.Interface) int { return strings.Compare(a.Name, b.Name) })
	}
	if len(diff.RoutesAdded)+len(diff.RoutesRemoved)+len(diff.InterfacesAdded)+len(diff.InterfacesRemoved)+len(diff.AddressesAdded)+len(diff.AddressesRemoved) == 0 {
		return nil, errors.New("no external differences to accept")
	}
	if len(diff.IndicesAdded) == 0 {
		diff.IndicesAdded = nil
	}
	if len(diff.IndicesRemoved) == 0 {
		diff.IndicesRemoved = nil
	}
	return diff, nil
}

// Preserve multiplicity; duplicate routes must not disappear in a set conversion.
func routeDifference(a, b []capture.Route) []capture.Route {
	counts := map[capture.Route]int{}
	for _, r := range b {
		counts[r]++
	}
	var result []capture.Route
	for _, r := range a {
		if counts[r] > 0 {
			counts[r]--
			continue
		}
		result = append(result, r)
	}
	slices.SortFunc(result, func(a, b capture.Route) int {
		x, _ := json.Marshal(a)
		y, _ := json.Marshal(b)
		return strings.Compare(string(x), string(y))
	})
	return result
}

func stableSnapshots(a, b snapshot) bool {
	return a.BootSession == b.BootSession && reflect.DeepEqual(a.InterfaceIndices, b.InterfaceIndices) && sameRoutes(a.Routes, b.Routes) && sameInterfaces(a.Interfaces, b.Interfaces) && sameAddressNetworks(a, b)
}

func sameCleanupBaseline(a, b snapshot) bool {
	tunnels := func(s snapshot) map[string]int {
		result := map[string]int{}
		for name, index := range s.InterfaceIndices {
			if strings.HasPrefix(name, "utun") {
				result[name] = index
			}
		}
		return result
	}
	return a.BootSession == b.BootSession && sameRoutes(a.Routes, b.Routes) && sameTunnels(a.Interfaces, b.Interfaces) && reflect.DeepEqual(tunnels(a), tunnels(b)) && sameAddressNetworks(a, b)
}

// IPv6 privacy rotation within one prefix is not a topology change. A changed
// assigned prefix or IPv4 address is external evidence, even if no corresponding
// configured route changed. Raw addresses remain in the private snapshots.
func sameAddressNetworks(a, b snapshot) bool {
	if !a.completeAddresses() || !b.completeAddresses() {
		return true // Legacy snapshots retain their existing route/tunnel checks.
	}
	networks := func(s snapshot) map[string][]netip.Prefix {
		out := map[string][]netip.Prefix{}
		for name, addresses := range s.InterfaceAddresses {
			for _, p := range addresses {
				if !p.Addr().IsGlobalUnicast() {
					continue
				}
				if p.Addr().Is6() {
					p = p.Masked()
				}
				if !slices.Contains(out[name], p) {
					out[name] = append(out[name], p)
				}
			}
			slices.SortFunc(out[name], func(x, y netip.Prefix) int { return strings.Compare(x.String(), y.String()) })
		}
		return out
	}
	return reflect.DeepEqual(networks(a), networks(b))
}

func addressDifference(a, b map[string][]netip.Prefix) map[string][]netip.Prefix {
	result := map[string][]netip.Prefix{}
	for name, addresses := range a {
		for _, p := range addresses {
			if !slices.Contains(b[name], p) {
				result[name] = append(result[name], p)
			}
		}
		slices.SortFunc(result[name], func(x, y netip.Prefix) int { return strings.Compare(x.String(), y.String()) })
	}
	if len(result) == 0 {
		return nil
	}
	return result
}

func (s *session) recheckExternal(after snapshot, st api.Status, recorded *externalChange) error {
	diff, err := s.externalDifferences(after, st)
	if err != nil {
		return err
	}
	if recorded != nil && !reflect.DeepEqual(diff, recorded) {
		return fmt.Errorf("external-change details do not match the recorded snapshots")
	}
	return nil
}
