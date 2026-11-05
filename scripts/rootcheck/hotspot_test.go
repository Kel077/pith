package main

import (
	"context"
	"errors"
	"net/netip"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Kel077/pith/internal/api"
	"github.com/Kel077/pith/internal/engine/capture"
)

type hotspotService struct {
	*baselineService
	renew func(context.Context) (api.HotspotRenewal, error)
}

func (c *hotspotService) Hotspot(context.Context, string) (api.Hotspot, error) {
	return api.Hotspot{Interface: "example0", Model: "M2000", Connected: true}, nil
}
func (c *hotspotService) RenewHotspot(ctx context.Context, iface string) (api.HotspotRenewal, error) {
	if iface != "example0" {
		return api.HotspotRenewal{}, errors.New("unexpected interface")
	}
	return c.renew(ctx)
}

func TestHotspotObservationVerifiesRenewalAndFreshCapture(t *testing.T) {
	s, base, ops := baselineFixture(t)
	if err := base.Start(context.Background(), "test"); err != nil {
		t.Fatal(err)
	}
	base.status.Since = time.Now()
	service := &hotspotService{baselineService: base, renew: func(ctx context.Context) (api.HotspotRenewal, error) {
		if _, ok := ctx.Deadline(); !ok {
			t.Error("renewal has no deadline")
		}
		return api.HotspotRenewal{After: api.Probe{Interface: "example0", OK: true, PublicIPv4: "203.0.113.99"}}, nil
	}}
	probes := 0
	ops.probe = func(_ context.Context, got *session, iface string) (tlsEvidence, error) {
		probes++
		if got.Target != s.Target || iface != "example0" {
			t.Fatal("post-renewal probe widened target or interface")
		}
		return tlsEvidence{StatusCode: 200, PublicIP: "203.0.113.99"}, nil
	}
	var report checkReport
	verified := false
	if err := observeHotspot(context.Background(), s, service, base.status, time.Second, &report, ops, func(_ context.Context, echo tlsEvidence) error {
		verified = echo.PublicIP == "203.0.113.99"
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if !verified || probes != 1 || report.Hotspot == nil || len(report.Hotspot.Statuses) < 4 || report.Hotspot.Finished.IsZero() || base.starts != 1 || base.stops != 0 {
		t.Fatal("renewal lost continuity, bound or captured evidence")
	}
}

func TestHotspotObservationCancelsAndJoinsOnCaptureLoss(t *testing.T) {
	s, base, ops := baselineFixture(t)
	_ = base.Start(context.Background(), "test")
	initial := base.status
	var renewing atomic.Bool
	joined := make(chan struct{})
	base.onStatus = func(_ int, st *api.Status) error {
		if renewing.Load() {
			st.State = api.Failed
		}
		return nil
	}
	service := &hotspotService{baselineService: base, renew: func(ctx context.Context) (api.HotspotRenewal, error) {
		renewing.Store(true)
		<-ctx.Done()
		close(joined)
		return api.HotspotRenewal{}, ctx.Err()
	}}
	var report checkReport
	err := observeHotspot(context.Background(), s, service, initial, time.Second, &report, ops, func(context.Context, tlsEvidence) error {
		t.Fatal("verified new flow after lost capture")
		return nil
	})
	if err == nil || report.Hotspot.Error == "" {
		t.Fatal("capture loss passed")
	}
	select {
	case <-joined:
	default:
		t.Fatal("renewal outlived observation")
	}
}

func TestHotspotOptInAndAuditPrecedeDeviceMutation(t *testing.T) {
	s, base, ops := baselineFixture(t)
	if err := checkWithOps(context.Background(), s, nil, checkOptions{renewHotspot: true}, ops); err == nil {
		t.Fatal("implicit observation allowed")
	}
	_ = base.Start(context.Background(), "test")
	ops.archive = func(*session, string, any) (string, error) { return "", errors.New("cannot archive") }
	service := &hotspotService{baselineService: base, renew: func(context.Context) (api.HotspotRenewal, error) {
		t.Fatal("mutated device without intent record")
		return api.HotspotRenewal{}, nil
	}}
	if err := observeHotspot(context.Background(), s, service, base.status, time.Second, &checkReport{}, ops, nil); err == nil {
		t.Fatal("missing audit passed")
	}
}

func TestHotspotPrefixChangesProduceExternalCleanupEvidence(t *testing.T) {
	for _, routes := range []bool{false, true} {
		s, c, ops, _ := externalFixture(t)
		s.Original.InterfaceAddresses = map[string][]netip.Prefix{
			"example0": {netip.MustParsePrefix("2001:db8:1::1/64")}, "example1": {}, "utun50": {},
		}
		if routes {
			s.Original.Routes = append(s.Original.Routes, capture.Route{Interface: "example0", Index: 42, Prefix: netip.MustParsePrefix("2001:db8:1::/64")})
		}
		after := cloneSnapshot(s.Original)
		after.InterfaceAddresses["example0"] = []netip.Prefix{netip.MustParsePrefix("2001:db8:2::1/64")}
		if routes {
			after.Routes[len(after.Routes)-1].Prefix = netip.MustParsePrefix("2001:db8:2::/64")
		}
		ops.snapshot = func(context.Context) (snapshot, error) { return after, nil }
		if err := stopWithOps(context.Background(), s, c, ops); !errors.Is(err, errExternalChange) || s.Cleanup == nil || s.Cleanup.Kind != externalProof {
			t.Fatal("external prefix change confused with capture residue", err)
		}
		if err := s.requireCleanup(true); err != nil {
			t.Fatal("external proof is not explicitly reviewable", err)
		}
		after.InterfaceAddresses["example0"] = []netip.Prefix{netip.MustParsePrefix("2001:db8:1::99/64"), netip.MustParsePrefix("2001:db8:1::88/64")}
		after.Routes = s.Original.Routes
		if !sameCleanupBaseline(s.Original, after) {
			t.Fatal("privacy-address rotation became a topology change")
		}
	}
}
