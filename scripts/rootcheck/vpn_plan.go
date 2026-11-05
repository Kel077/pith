package main

import (
	"context"
	"encoding/json"
	"errors"
	"net/netip"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/Kel077/pith/internal/api"
	"github.com/Kel077/pith/internal/captureprefix"
	"github.com/Kel077/pith/internal/engine/capture"
	"github.com/Kel077/pith/internal/testprofile"
)

type vpnRestartPlan struct {
	DNS       string      `json:"dnsTarget"`
	OwnName   string      `json:"ownName"`
	OwnIPv4   []string    `json:"ownIPv4,omitempty"`
	Tunnel    vpnTunnel   `json:"tunnel"`
	Preflight dnsEvidence `json:"preflight"`
}

type vpnTunnel struct {
	Name      string          `json:"name"`
	Index     int             `json:"index"`
	Addresses []string        `json:"ipv4"`
	DNS       []string        `json:"dns,omitempty"`
	Routes    []capture.Route `json:"routes"`
}

func (s *session) validateVPN() error {
	p := s.VPNRestart
	if p == nil {
		return nil
	}
	if s.TestProfile == nil || s.TestProfile.Mode != testprofile.FullIPv4 || s.TestProfile.Lease != 120*time.Second {
		return errors.New("VPN restart requires the separate full-ipv4 profile with a 120s helper lease")
	}
	target, err := netip.ParseAddrPort(p.DNS)
	if err != nil || target.String() != p.DNS || target.Port() != 53 || !target.Addr().Is4() || !target.Addr().IsGlobalUnicast() || target.Addr().IsPrivate() {
		return errors.New("VPN DNS must be a canonical reviewed device-local IPv4:53")
	}
	name, err := api.ParseTarget(p.OwnName)
	if err != nil || !name.IsDomain() || name.Subdomains || name.String() != p.OwnName || strings.Count(p.OwnName, ".") < 2 {
		return errors.New("vpn-own-name must be this Mac's full lowercase ASCII MagicDNS name")
	}
	return nil
}

// The resolver must bypass full capture on a more-specific unscoped VPN route.
// A scoped default alone is not evidence, and an exit-node layout is refused.
func vpnTunnelAt(view snapshot, dns string) (vpnTunnel, bool, error) {
	var result vpnTunnel
	dst, err := netip.ParseAddrPort(dns)
	if err != nil {
		return result, false, err
	}
	for _, r := range view.Routes {
		if strings.HasPrefix(r.Interface, "utun") && !r.Scoped && r.Prefix.Bits() == 0 {
			return result, false, errors.New("VPN acceptance does not permit an unscoped tunnel default or exit node")
		}
	}
	name := capture.SystemInterface(view.Routes, dst.Addr())
	if !strings.HasPrefix(name, "utun") {
		return result, false, nil
	}
	iface := (api.InterfaceRef{Name: name}).Resolve(view.Interfaces)
	if iface == nil || !iface.Up || iface.Kind != api.KindVPN || view.InterfaceIndices[name] <= 0 {
		return result, false, nil
	}
	result.Name, result.Index = name, view.InterfaceIndices[name]
	result.Addresses, result.DNS = slices.Clone(iface.IPv4), slices.Clone(iface.DNS)
	slices.Sort(result.Addresses)
	slices.Sort(result.DNS)
	protected := false
	for _, r := range view.Routes {
		if r.Interface != name {
			continue
		}
		if r.Index != result.Index {
			return vpnTunnel{}, false, errors.New("VPN route and interface generations disagree")
		}
		if vpnProtectedRoute(r, dst.Addr()) {
			protected = true
		}
		result.Routes = append(result.Routes, r)
	}
	result.Routes = routeDifference(result.Routes, nil)
	return result, protected && len(result.Addresses) > 0, nil
}

func vpnProtectedRoute(route capture.Route, destination netip.Addr) bool {
	width := -1
	for _, prefix := range captureprefix.Full(false) {
		if prefix.Contains(destination) {
			width = prefix.Bits()
		}
	}
	return width >= 0 && !route.Scoped && strings.HasPrefix(route.Interface, "utun") && route.Prefix.Contains(destination) && route.Prefix.Bits() > width
}

func vpnDNSProtected(view snapshot, target string) bool {
	addr, err := netip.ParseAddrPort(target)
	if err != nil {
		return true
	}
	return slices.ContainsFunc(view.Routes, func(r capture.Route) bool { return vpnProtectedRoute(r, addr.Addr()) })
}

func vpnOwnAnswers(p *vpnRestartPlan, tunnel vpnTunnel, evidence dnsEvidence) ([]string, error) {
	if evidence.Name != p.OwnName || evidence.Type != "TypeA" || evidence.Remote != p.DNS || evidence.QueryBytes == 0 || evidence.ResponseBytes == 0 {
		return nil, errors.New("missing exact own-name DNS exchange evidence")
	}
	var answers []string
	for _, answer := range evidence.Answers {
		if answer.Type != "TypeA" {
			continue
		}
		if !slices.Contains(tunnel.Addresses, answer.Value) {
			return nil, errors.New("MagicDNS answer is not an IPv4 address assigned to this Mac's VPN interface")
		}
		if !slices.Contains(answers, answer.Value) {
			answers = append(answers, answer.Value)
		}
	}
	slices.Sort(answers)
	if len(answers) == 0 || len(answers) > 4 || len(p.OwnIPv4) != 0 && !slices.Equal(p.OwnIPv4, answers) {
		return nil, errors.New("own MagicDNS addresses are absent or changed from the reviewed plan")
	}
	return answers, nil
}

func (s *session) freezeVPN(ctx context.Context, deps vpnRestartDeps) error {
	if err := s.validateVPN(); err != nil {
		return err
	}
	tunnel, connected, err := vpnTunnelAt(s.Original, s.VPNRestart.DNS)
	if err != nil {
		return err
	}
	if !connected {
		return errors.New("connect the reviewed VPN without an exit node before preparing")
	}
	public, _ := netip.ParseAddrPort(s.Target)
	if strings.HasPrefix(capture.SystemInterface(s.Original.Routes, public.Addr()), "utun") {
		return errors.New("the public control must use a physical system route without an exit node")
	}
	evidence, err := deps.dns(ctx, s.VPNRestart)
	if err != nil {
		return err
	}
	addresses, err := vpnOwnAnswers(s.VPNRestart, tunnel, evidence)
	if err != nil {
		return err
	}
	after, err := deps.snapshot(ctx)
	if err != nil {
		return err
	}
	if !stableSnapshots(s.Original, after) {
		return errors.New("VPN changed during own-name preflight")
	}
	s.VPNRestart.OwnIPv4, s.VPNRestart.Tunnel, s.VPNRestart.Preflight = addresses, tunnel, evidence
	return nil
}

func (s *session) vpnPlanBytes() ([]byte, error) {
	if s.VPNRestart == nil {
		return nil, errors.New("prepare a reviewed VPN restart plan first")
	}
	if err := s.validateVPN(); err != nil {
		return nil, err
	}
	p := s.VPNRestart
	if len(p.OwnIPv4) == 0 || p.Tunnel.Name == "" || p.Tunnel.Index <= 0 {
		return nil, errors.New("VPN plan lacks frozen local identity")
	}
	b, err := json.MarshalIndent(struct {
		Plan       *vpnRestartPlan     `json:"plan"`
		Public     string              `json:"publicTarget"`
		ServerName string              `json:"serverName"`
		Interfaces [2]api.InterfaceRef `json:"interfaces"`
		Notes      []string            `json:"notes"`
	}{p, s.Target, s.ServerName, s.Interfaces, []string{
		"One user-controlled disconnect/reconnect; no VPN CLI, toggle, restart, update, peer scan or account change.",
		"READY: disconnect within at most 45s, keep disconnected about 15s, then reconnect once. Require at least 10s observed withdrawal; RECONNECT follows gap controls.",
		"Rebuild window is at most 45s, clipped to leave 15s for final controls before the controller deadline. Every timeout or early reconnect is incomplete, not a Pith network failure.",
		"At most one own-name A query in prepare, and five in check including post-Stop; one datagram each, two-second deadline, no DNS fallback or answer-address dialing.",
		"Four extra frozen public HTTPS requests in check, three seconds each, in addition to common SafeStart/system control; no replacement endpoint or retry.",
		"Own-name/Quad100 proves local MagicDNS and route lifecycle, not remote peer reachability, wire-level absence of leaks, existing TCP continuity, daemon restart or egress generation internals.",
	}}, "", "  ")
	return append(b, '\n'), err
}

func (s *session) writeVPNPlan() error {
	if s.VPNRestart == nil {
		return nil
	}
	data, err := s.vpnPlanBytes()
	if err != nil {
		return err
	}
	if err := writePrivate(filepath.Join(s.Dir, "vpn-plan.json"), data); err != nil {
		return err
	}
	s.VPNPlanSHA256 = hashBytes(data)
	return s.save()
}

func (s *session) reviewedVPNPlan() error {
	data, err := s.vpnPlanBytes()
	if err != nil {
		return err
	}
	path := filepath.Join(s.Dir, "vpn-plan.json")
	info, err := os.Lstat(path)
	if err != nil || !info.Mode().IsRegular() || info.Mode().Perm() != 0600 {
		return errors.New("VPN plan is not a private regular file")
	}
	actual, err := os.ReadFile(path)
	if err != nil || hashBytes(data) != s.VPNPlanSHA256 || hashBytes(actual) != s.VPNPlanSHA256 {
		return errors.New("VPN plan changed after review")
	}
	return nil
}
