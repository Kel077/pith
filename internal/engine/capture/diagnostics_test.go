package capture

import (
	"context"
	"net/netip"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/Kel077/pith/internal/api"
)

type diagnosticDriver struct {
	fakeDriver
	details []RouteDetail
	reads   int
}

func (d *diagnosticDriver) SnapshotDetails() ([]Route, []RouteDetail, error) {
	d.reads++
	return slices.Clone(d.routes), slices.Clone(d.details), nil
}

func TestOwnershipFailureSummarizesSameSnapshotWithoutAddresses(t *testing.T) {
	for _, problem := range []string{"missing", "duplicated", "scoped", "IP gateway", "foreign index"} {
		t.Run(problem, func(t *testing.T) {
			d := &diagnosticDriver{}
			p := prefix("2001:db8::9/128")
			var diagnostics []string
			c, err := Prepare(context.Background(), Options{Driver: d, IPv6: true, Scopes: []netip.Prefix{p}, StatePath: filepath.Join(t.TempDir(), "capture.json"), Diagnostic: func(s string) { diagnostics = append(diagnostics, s) }})
			if err != nil {
				t.Fatal(err)
			}
			defer c.Close()
			g, err := NewGuard(c, nil)
			if err != nil {
				t.Fatal(err)
			}
			if err := c.Activate(context.Background()); err != nil {
				t.Fatal(err)
			}
			d.details = []RouteDetail{{Prefix: p, Flags: 0xc05, GatewayFamily: 18, Index: 42, Raw: true, Policy: true}}
			switch problem {
			case "missing":
				d.routes = nil
				d.details[0].Policy = false
			case "duplicated":
				d.routes = append(d.routes, d.routes[0])
				d.details = append(d.details, d.details[0])
			case "scoped":
				d.routes[0].Scoped, d.details[0].Scoped = true, true
			case "IP gateway":
				d.routes[0].Gateway = addr("2001:db8:1::1")
				d.details[0].GatewayFamily = 30
			case "foreign index":
				d.routes[0].Index, d.details[0].Index = 50, 50
			}
			// Raw cache entries from the same read must be reported too, but must
			// not count as policy, duplicate ownership, or deletion authority.
			d.details = append(d.details, RouteDetail{Prefix: p, Flags: 0x10405, GatewayFamily: 30, Index: 51, Scoped: true, Raw: true})
			if api.Code(g.Validate(true)) != api.ErrNetworkLost || d.reads != 1 || len(diagnostics) != 1 {
				t.Fatal("missing fault evidence or resampled the failure", d.reads, diagnostics)
			}
			for _, want := range []string{"family=IPv6 bits=128 expected=1 owned_index=42", "flags=0xc05", "flags=0x10405 gateway_family=30 scoped=true index=51 policy=false"} {
				if !strings.Contains(diagnostics[0], want) {
					t.Fatal("lost raw candidate metadata", want, diagnostics)
				}
			}
			if problem == "duplicated" && (!strings.Contains(diagnostics[0], "candidate_count=3") || !strings.Contains(diagnostics[0], "owned_count=2 policy_count=2") || !strings.Contains(diagnostics[0], "count=2 flags=0xc05")) {
				t.Fatal("incorrect duplicate counts", diagnostics)
			}
			for _, secret := range []string{"2001:db8", "utun42", "::9", "::1"} {
				if strings.Contains(diagnostics[0], secret) {
					t.Fatal("route address or interface name leaked")
				}
			}
		})
	}
}

func TestOwnershipDiagnosticGroupsAreBounded(t *testing.T) {
	p := prefix("2001:db8::9/128")
	g := &Guard{c: &Capture{Plan: Plan{Prefixes: []netip.Prefix{p}}}}
	var details []RouteDetail
	for index := range 100 {
		details = append(details, RouteDetail{Prefix: p, Index: index, Raw: true})
	}
	got := g.ownershipDiagnostics(nil, details, true)
	if len(got) != 1 || len(got[0]) > 5000 || !strings.Contains(got[0], "candidate_count=100") || !strings.Contains(got[0], "omitted_count=68") {
		t.Fatal("ownership diagnostics are unbounded or hide omitted counts", got)
	}
}
