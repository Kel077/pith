package capture

import (
	"fmt"
	"net/netip"
	"slices"
	"strings"
)

// RouteDetail is observation metadata, never route selection or deletion
// authority. Prefix is used only to group candidates before producing an
// address-free diagnostic. Keep raw flags out of Route equality: reference flags
// can change without changing forwarding policy.
type RouteDetail struct {
	Prefix        netip.Prefix
	Flags         int
	GatewayFamily int
	Index         int
	Scoped        bool
	Policy        bool
	Raw           bool
}

type detailedSnapshotReader interface {
	SnapshotDetails() ([]Route, []RouteDetail, error)
}

func snapshotDetails(driver Driver) ([]Route, []RouteDetail, error) {
	if reader, ok := driver.(detailedSnapshotReader); ok {
		return reader.SnapshotDetails()
	}
	routes, err := driver.Snapshot()
	details := make([]RouteDetail, 0, len(routes))
	for _, r := range routes {
		details = append(details, RouteDetail{Prefix: r.Prefix, Index: r.Index, Scoped: r.Scoped, Policy: true})
	}
	return routes, details, err
}

// Summaries use ordinal/family/width instead of the destination or interface
// name. Identical metadata is counted, and distinct groups are bounded. The
// candidate count includes filtered cache/down entries from this exact RIB read.
func (g *Guard) ownershipDiagnostics(routes []Route, details []RouteDetail, active bool) []string {
	result := make([]string, 0, len(g.c.Plan.Prefixes))
	for ordinal, p := range g.c.Plan.Prefixes {
		expected := 0
		if active {
			expected = 1
		}
		policy, owned := 0, 0
		for _, r := range routes {
			if r.Prefix == p {
				policy++
				if r.Interface == g.c.Name && r.Index == g.c.state.Index && !r.Scoped && !r.Local && !r.Reject && !r.Gateway.IsValid() {
					owned++
				}
			}
		}
		counts := map[string]int{}
		total := 0
		for _, d := range details {
			if d.Prefix != p {
				continue
			}
			total++
			flags, gateway := "unknown", "unknown"
			if d.Raw {
				flags, gateway = fmt.Sprintf("0x%x", d.Flags), fmt.Sprint(d.GatewayFamily)
			}
			key := fmt.Sprintf("flags=%s gateway_family=%s scoped=%t index=%d policy=%t", flags, gateway, d.Scoped, d.Index, d.Policy)
			counts[key]++
		}
		keys := make([]string, 0, len(counts))
		for key := range counts {
			keys = append(keys, key)
		}
		slices.Sort(keys)
		var groups []string
		omitted := 0
		for i, key := range keys {
			if i >= 32 {
				omitted += counts[key]
				continue
			}
			groups = append(groups, fmt.Sprintf("count=%d %s", counts[key], key))
		}
		if omitted != 0 {
			groups = append(groups, fmt.Sprintf("omitted_count=%d", omitted))
		}
		family := "IPv4"
		if p.Addr().Is6() {
			family = "IPv6"
		}
		result = append(result, fmt.Sprintf("capture prefix=%d family=%s bits=%d expected=%d owned_index=%d owned_count=%d policy_count=%d candidate_count=%d routes=[%s]", ordinal+1, family, p.Bits(), expected, g.c.state.Index, owned, policy, total, strings.Join(groups, "; ")))
	}
	return result
}
