package ui

import (
	"slices"
	"testing"
	"time"

	"github.com/Kel077/pith/internal/api"
	"github.com/Kel077/pith/internal/mock"
)

// Pith's own capture tunnel is never shown or offered as an interface; a
// tunnel that only reuses its name, or a capture that has ended, is.
func TestOwnTunnelHidden(t *testing.T) {
	a, _ := testApp(t, mock.Options{Sample: true, HelperInstalled: true, Running: true})
	own := api.Interface{Name: "utun9", Kind: api.KindVPN, Up: true, IPv4: []string{"192.0.2.253"}, IPv6: []string{"2001:db8:ffff::1"}}
	other := api.Interface{Name: "utun3", Kind: api.KindVPN, Up: true, IPv4: []string{"198.51.100.7"}}
	ifs := append(slices.Clone(a.ifaces), own, other)
	capture := &api.CaptureIdentity{Interface: "utun9", Index: 40, TunnelSubnets: []string{"192.0.2.252/30", "2001:db8:ffff::/126"}}

	st := a.status
	st.LastCapture = capture
	a.setStatus(st)
	a.setIfaces(ifs)
	names := func() []string {
		var out []string
		for _, i := range a.ifaces {
			out = append(out, i.Name)
		}
		return out
	}
	if slices.Contains(names(), "utun9") || !slices.Contains(names(), "utun3") {
		t.Fatalf("shown %v", names())
	}

	// Another tunnel later reusing the name, outside the capture's subnets.
	reused := own
	reused.IPv4, reused.IPv6 = []string{"203.0.113.9"}, nil
	a.setIfaces(append(slices.Clone(ifs[:len(ifs)-2]), reused, other))
	if !slices.Contains(names(), "utun9") {
		t.Fatalf("a tunnel that only shares the name is hidden: %v", names())
	}

	// The status arriving after the interfaces hides it too.
	a.setStatus(api.Status{State: api.Stopped})
	a.setIfaces(ifs)
	if !slices.Contains(names(), "utun9") {
		t.Fatalf("hidden without a capture: %v", names())
	}
	st.LastCapture = &api.CaptureIdentity{Interface: "utun9", TunnelSubnets: capture.TunnelSubnets, EndedAt: time.Time{}}
	a.setStatus(st)
	if slices.Contains(names(), "utun9") {
		t.Fatalf("still shown once the status names it: %v", names())
	}
}
