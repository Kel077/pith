package main

import (
	"context"
	"errors"
	"fmt"
	"net/netip"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Kel077/pith/internal/api"
	"github.com/Kel077/pith/internal/engine/capture"
	"github.com/Kel077/pith/internal/testprofile"
)

func vpnFixture(t *testing.T) *session {
	t.Helper()
	s, _, _ := baselineFixture(t)
	s.TestProfile = &testprofile.Profile{Mode: testprofile.FullIPv4, Lease: 120 * time.Second, Target: s.Target, ServerName: s.ServerName}
	s.Original.InterfaceIndices["utun50"] = 50
	s.Original.Interfaces = append(s.Original.Interfaces, api.Interface{Name: "utun50", Kind: api.KindVPN, Up: true, IPv4: []string{"203.0.113.2"}})
	s.Original.Routes = append(s.Original.Routes, capture.Route{Prefix: netip.MustParsePrefix("203.0.113.0/24"), Interface: "utun50", Index: 50}, capture.Route{Prefix: netip.MustParsePrefix("0.0.0.0/0"), Interface: "utun50", Index: 50, Scoped: true})
	s.VPNRestart = &vpnRestartPlan{DNS: "203.0.113.53:53", OwnName: "this-host.example.com"}
	deps := liveVPNChecks(nil)
	deps.snapshot = func(context.Context) (snapshot, error) { return s.Original, nil }
	deps.dns = func(context.Context, *vpnRestartPlan) (dnsEvidence, error) { return vpnAnswer(s), nil }
	if err := s.freezeVPN(context.Background(), deps); err != nil {
		t.Fatal(err)
	}
	if err := s.writeVPNPlan(); err != nil {
		t.Fatal(err)
	}
	return s
}

func vpnAnswer(s *session) dnsEvidence {
	return dnsEvidence{ID: 7, Name: s.VPNRestart.OwnName, Type: "TypeA", Local: "192.0.2.1:51000", Remote: s.VPNRestart.DNS, QueryBytes: 40, ResponseBytes: 70, Answers: []dnsAnswer{{Name: s.VPNRestart.OwnName + ".", Type: "TypeA", Value: "203.0.113.2", TTL: 60}}}
}

func TestVPNPlanOnlyFullModeAndLocalOwnName(t *testing.T) {
	s := vpnFixture(t)
	if err := s.reviewedVPNPlan(); err != nil {
		t.Fatal(err)
	}
	if err := claimVPN(s); err != nil {
		t.Fatal(err)
	}
	if claimVPN(s) == nil {
		t.Fatal("repeated restart admitted")
	}
	if info, err := os.Stat(filepath.Join(s.Dir, "vpn-restart-attempt.json")); err != nil || info.Mode().Perm() != 0600 {
		t.Fatal(info, err)
	}
	binding := s.cleanupBinding()
	s.VPNPlanSHA256 = strings.Repeat("0", 64)
	if s.reviewedVPNPlan() == nil || binding == s.cleanupBinding() {
		t.Fatal("changed plan accepted")
	}
	s.TestProfile = nil
	if s.validateVPN() == nil {
		t.Fatal("scoped mode accepted")
	}
	s.TestProfile = &testprofile.Profile{Mode: testprofile.FullIPv4, Lease: 60 * time.Second}
	if s.validateVPN() == nil {
		t.Fatal("short lease admitted for human stage")
	}
	p := vpnAnswer(s)
	p.Answers[0].Value = "203.0.113.3"
	if _, err := vpnOwnAnswers(s.VPNRestart, s.VPNRestart.Tunnel, p); err == nil {
		t.Fatal("peer answer accepted as self")
	}
	view := cloneSnapshot(s.Original)
	view.Routes = append(view.Routes, capture.Route{Prefix: netip.MustParsePrefix("0.0.0.0/0"), Interface: "utun50", Index: 50})
	if _, _, err := vpnTunnelAt(view, s.VPNRestart.DNS); err == nil {
		t.Fatal("exit node accepted")
	}
}

func TestVPNFrozenPlanPreservesScopeAndDetectsStaleRoutes(t *testing.T) {
	s := vpnFixture(t)
	scopes, err := s.scopes()
	if err != nil || !slices.Equal(scopes, s.TestProfile.Prefixes()) {
		t.Fatal(scopes, err)
	}
	view := cloneSnapshot(s.Original)
	view.Interfaces[len(view.Interfaces)-1].Up = false
	_, up, err := vpnTunnelAt(view, s.VPNRestart.DNS)
	if err != nil || up || !vpnDNSProtected(view, s.VPNRestart.DNS) {
		t.Fatal("down flag was mistaken for withdrawn route", up, err)
	}
	view.Routes[len(view.Routes)-2].Index++
	view.Interfaces[len(view.Interfaces)-1].Up = true
	if _, _, err := vpnTunnelAt(view, s.VPNRestart.DNS); err == nil {
		t.Fatal("stale VPN route generation accepted")
	}
	changed := cloneSnapshot(s.Original)
	changed.Interfaces[len(changed.Interfaces)-1].IPv4 = []string{"203.0.113.9"}
	deps := liveVPNChecks(func(context.Context) (snapshot, error) { return changed, nil })
	deps.dns = func(context.Context, *vpnRestartPlan) (dnsEvidence, error) { return vpnAnswer(s), nil }
	if s.freezeVPN(context.Background(), deps) == nil {
		t.Fatal("changing preflight accepted")
	}
}

type vpnService struct {
	api.Service
	mu              sync.Mutex
	s               *session
	state           api.Status
	cfg             api.Config
	rows            []api.Connection
	stage           string
	pidpath         string
	leak, wrongExit bool
}

func newVPNService(t *testing.T, s *session) *vpnService {
	path, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	id := &api.CaptureIdentity{BootSession: s.Original.BootSession, Token: "test-vpn-capture", Interface: "utun90", Index: 90}
	for _, p := range s.TestProfile.Prefixes() {
		id.Prefixes = append(id.Prefixes, p.String())
	}
	return &vpnService{s: s, state: api.Status{State: api.Running, Since: time.Now(), LastCapture: id}, cfg: api.DefaultConfig(), stage: "before", pidpath: path}
}
func (c *vpnService) Status(context.Context) (api.Status, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	result := c.state
	if result.LastCapture != nil {
		id := *result.LastCapture
		id.Prefixes = slices.Clone(id.Prefixes)
		result.LastCapture = &id
	}
	return result, nil
}
func (c *vpnService) SaveConfig(_ context.Context, cfg api.Config) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.cfg = cfg
	return nil
}
func (c *vpnService) Connections(context.Context, api.ConnectionQuery) ([]api.Connection, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return slices.Clone(c.rows), nil
}
func (c *vpnService) Traffic(_ context.Context, q api.TrafficQuery) (api.Traffic, error) {
	return api.Traffic{Series: []api.TrafficSample{{At: q.Since, Sent: 100, Received: 200}}, Apps: []api.AppTraffic{{}}}, nil
}
func (c *vpnService) set(stage string) { c.mu.Lock(); defer c.mu.Unlock(); c.stage = stage }
func (c *vpnService) snapshot(context.Context) (snapshot, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	view := cloneSnapshot(c.s.Original)
	if c.stage == "down" {
		view.Routes = slices.DeleteFunc(view.Routes, func(r capture.Route) bool { return r.Interface == "utun50" })
		view.Interfaces = slices.DeleteFunc(view.Interfaces, func(i api.Interface) bool { return i.Name == "utun50" })
		delete(view.InterfaceIndices, "utun50")
	}
	if c.stage == "rebuilt" {
		for i := range view.Routes {
			if view.Routes[i].Interface == "utun50" {
				view.Routes[i].Interface = "utun51"
				view.Routes[i].Index = 51
			}
		}
		for i := range view.Interfaces {
			if view.Interfaces[i].Name == "utun50" {
				view.Interfaces[i].Name = "utun51"
			}
		}
		delete(view.InterfaceIndices, "utun50")
		view.InterfaceIndices["utun51"] = 51
	}
	if c.state.State == api.Running {
		id := c.state.LastCapture
		view.InterfaceIndices[id.Interface] = id.Index
		view.Interfaces = append(view.Interfaces, api.Interface{Name: id.Interface, Kind: api.KindVPN, Up: true})
		for _, raw := range id.Prefixes {
			view.Routes = append(view.Routes, capture.Route{Prefix: netip.MustParsePrefix(raw), Interface: id.Interface, Index: id.Index})
		}
	}
	return view, nil
}
func (c *vpnService) dns(_ context.Context, _ *vpnRestartPlan) (dnsEvidence, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	answer := vpnAnswer(c.s)
	if c.stage == "down" {
		answer.ResponseBytes = 0
		answer.Answers = nil
		answer.Error = context.DeadlineExceeded.Error()
		if c.leak {
			c.rows = append(c.rows, api.Connection{ID: uint64(len(c.rows) + 1), Protocol: api.UDP, Remote: answer.Remote, Local: answer.Local, Process: api.Process{PID: os.Getpid()}, Interface: c.s.Interfaces[0].Name, Outcome: api.System, Sent: 40})
		}
		return answer, context.DeadlineExceeded
	}
	return answer, nil
}
func (c *vpnService) public(_ context.Context, _ *session) (tlsEvidence, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	evidence := tlsEvidence{Local: fmt.Sprintf("192.0.2.1:%d", 52000+len(c.rows)), StatusCode: 200, BodyBytes: 20}
	row := api.Connection{ID: uint64(len(c.rows) + 1), Local: evidence.Local, Remote: c.s.Target, Protocol: api.TCP, Process: api.Process{PID: os.Getpid(), Path: c.pidpath}, Outcome: api.System, Interface: c.s.Interfaces[0].Name, OutboundLocal: "198.51.100.1:53000", Sent: 100, Received: 200}
	if len(c.cfg.Scenes[0].Rules) > 0 {
		row.Outcome = api.Routed
		row.RuleID = "vpn-public"
	}
	if c.wrongExit {
		row.Interface = c.s.Interfaces[1].Name
	}
	c.rows = append(c.rows, row)
	return evidence, nil
}

func TestVPNRestartManualTransitionsAndCleanupProof(t *testing.T) {
	s := vpnFixture(t)
	c := newVPNService(t, s)
	deps := liveVPNChecks(c.snapshot)
	deps.dns = c.dns
	deps.public = c.public
	deps.poll = time.Millisecond
	deps.minimumDown = 20 * time.Millisecond
	var prompts []string
	deps.prompt = func(line string) {
		prompts = append(prompts, line)
		if strings.HasPrefix(line, "READY:") {
			c.set("down")
		}
		if strings.HasPrefix(line, "RECONNECT:") {
			c.set("rebuilt")
		}
	}
	report := checkReport{Started: time.Now()}
	if err := beginVPNCheck(context.Background(), s, s.Original, &report, deps); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Second)
	defer cancel()
	running, _ := c.Status(ctx)
	if err := runVPNRestart(ctx, s, c, api.DefaultConfig(), running, &report, deps); err != nil {
		t.Fatal(err)
	}
	e := report.VPNRestart
	if !e.ControlsPassed || e.PostStopPassed || len(prompts) != 3 || len(e.Queries) != 4 || len(report.Cases) != 4 || e.ReconnectAt.Sub(e.WithdrewAt) < deps.minimumDown {
		t.Fatal(e, prompts, len(report.Cases))
	}
	if e.Polls[len(e.Polls)-1].Interface != "utun51" || e.Polls[len(e.Polls)-1].Index != 51 {
		t.Fatal(e.Polls)
	}
	c.mu.Lock()
	c.state.State = api.Stopped
	c.mu.Unlock()
	view, _ := c.snapshot(ctx)
	if err := verifyVPNAfterStop(ctx, s, view, e, deps); err != nil || !e.PostStopPassed || len(e.Queries) != 5 {
		t.Fatal(err, e)
	}
	if claimVPN(s) == nil {
		t.Fatal("restart can repeat")
	}
}

func TestVPNIncompleteAndForwardingFailures(t *testing.T) {
	for _, kind := range []string{"early-reconnect", "leak", "wrong-exit", "capture-stopped", "capture-route-missing"} {
		t.Run(kind, func(t *testing.T) {
			s := vpnFixture(t)
			c := newVPNService(t, s)
			deps := liveVPNChecks(c.snapshot)
			deps.dns = c.dns
			deps.public = c.public
			deps.poll = time.Millisecond
			deps.minimumDown = 20 * time.Millisecond
			deps.prompt = func(line string) {
				if strings.HasPrefix(line, "READY:") {
					c.set("down")
				}
			}
			if kind == "leak" {
				c.leak = true
			}
			if kind == "wrong-exit" {
				c.wrongExit = true
			}
			if kind == "early-reconnect" {
				deps.dns = func(ctx context.Context, p *vpnRestartPlan) (dnsEvidence, error) {
					e, err := c.dns(ctx, p)
					if err != nil {
						c.set("rebuilt")
					}
					return e, err
				}
			}
			if kind == "capture-stopped" {
				deps.prompt = func(line string) { c.mu.Lock(); defer c.mu.Unlock(); c.state.State = api.Stopped }
			}
			if kind == "capture-route-missing" {
				deps.snapshot = func(ctx context.Context) (snapshot, error) {
					v, err := c.snapshot(ctx)
					v.Routes = slices.DeleteFunc(v.Routes, func(r capture.Route) bool { return r.Interface == "utun90" })
					return v, err
				}
			}
			report := checkReport{Started: time.Now()}
			if err := beginVPNCheck(context.Background(), s, s.Original, &report, deps); err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithTimeout(context.Background(), 100*time.Second)
			defer cancel()
			running, _ := c.Status(ctx)
			err := runVPNRestart(ctx, s, c, api.DefaultConfig(), running, &report, deps)
			if err == nil || report.VPNRestart.ControlsPassed {
				t.Fatal("failure accepted", err)
			}
			if kind == "early-reconnect" && (!errors.Is(err, errVPNInconclusive) || report.VPNRestart.Incomplete == "") {
				t.Fatal(err)
			}
		})
	}
}

func TestVPNWindowBoundsAndTimeout(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	window := vpnWindow(ctx, 45*time.Second, 10*time.Second)
	if window > 20*time.Second || window < 19*time.Second {
		t.Fatal(window)
	}
	s := vpnFixture(t)
	c := newVPNService(t, s)
	deps := liveVPNChecks(c.snapshot)
	deps.poll = time.Millisecond
	running, _ := c.Status(ctx)
	evidence := &vpnRestartEvidence{}
	_, _, err := waitVPNState(ctx, s, c, running, evidence, deps, false, 5*time.Millisecond)
	if !errors.Is(err, errVPNInconclusive) {
		t.Fatal(err)
	}
	c.mu.Lock()
	c.state.LastCapture.Token = "different"
	c.mu.Unlock()
	if _, _, _, err := vpnCheckpoint(ctx, s, c, running, evidence, deps); err == nil {
		t.Fatal("capture token change accepted")
	}
}

func TestVPNNoPlanNoCaptureAndPrivateReview(t *testing.T) {
	s, c, ops := baselineFixture(t)
	if err := checkWithOps(context.Background(), s, c, checkOptions{vpnRestart: true}, ops); err == nil || c.starts != 0 || c.stops != 0 {
		t.Fatal(err, c.starts, c.stops)
	}
	s = vpnFixture(t)
	before := s.cleanupBinding()
	s.VPNRestart.OwnName = "another-host.example.com"
	if s.reviewedVPNPlan() == nil {
		t.Fatal("own name changed")
	}
	s.VPNRestart.OwnName = "this-host.example.com"
	if before != s.cleanupBinding() {
		t.Fatal("unexpected proof drift")
	}
	plan, err := s.vpnPlanBytes()
	if err != nil || !strings.Contains(string(plan), "no VPN CLI") {
		t.Fatal(err, string(plan))
	}
	if !reflect.DeepEqual(s.VPNRestart.OwnIPv4, []string{"203.0.113.2"}) {
		t.Fatal(s.VPNRestart.OwnIPv4)
	}
}

func TestVPNAdmissionFailureStillStopsAndRecordsCleanup(t *testing.T) {
	s := vpnFixture(t)
	_, c, ops := baselineFixture(t)
	ops.snapshot = func(context.Context) (snapshot, error) { return s.Original, nil }
	deps := liveVPNChecks(ops.snapshot)
	deps.dns = func(context.Context, *vpnRestartPlan) (dnsEvidence, error) {
		return dnsEvidence{}, context.DeadlineExceeded
	}
	ops.vpn = &deps
	err := checkWithOps(context.Background(), s, c, checkOptions{vpnRestart: true}, ops)
	if !errors.Is(err, errVPNInconclusive) || c.starts != 0 || c.stops != 1 || s.Cleanup == nil {
		t.Fatal(err, c.starts, c.stops, s.Cleanup)
	}
	if claimVPN(s) == nil {
		t.Fatal("failed attempt was refunded")
	}
}

func TestVPNWithdrawalCancelsAndJoinsInFlightRequest(t *testing.T) {
	s := vpnFixture(t)
	c := newVPNService(t, s)
	c.set("down")
	started, ended := make(chan struct{}), make(chan struct{})
	deps := liveVPNChecks(func(ctx context.Context) (snapshot, error) {
		select {
		case <-started:
			c.set("rebuilt")
		default:
		}
		return c.snapshot(ctx)
	})
	deps.dns = c.dns
	deps.poll = time.Millisecond
	deps.public = func(ctx context.Context, _ *session) (tlsEvidence, error) {
		close(started)
		<-ctx.Done()
		close(ended)
		return tlsEvidence{}, ctx.Err()
	}
	report := checkReport{VPNRestart: &vpnRestartEvidence{WithdrewAt: time.Now()}}
	running, _ := c.Status(context.Background())
	err := vpnDisconnectedControls(context.Background(), s, c, api.DefaultConfig(), running, &report, deps)
	if !errors.Is(err, errVPNInconclusive) {
		t.Fatal(err)
	}
	select {
	case <-ended:
	default:
		t.Fatal("request worker left running")
	}
}

func TestVPNLateDNSRecordInvalidatesControls(t *testing.T) {
	s := vpnFixture(t)
	query := vpnAnswer(s)
	e := &vpnRestartEvidence{ControlsPassed: true, Queries: []vpnDNSCase{{Phase: "withdrawn", Exchange: query, AfterConnectionID: 3, CheckNoForwarding: true}}}
	rows := []api.Connection{{ID: 4, Remote: query.Remote, Local: query.Local, Process: api.Process{PID: os.Getpid()}, Outcome: api.System}}
	if checkVPNFinalRows(e, rows) == nil || e.ControlsPassed {
		t.Fatal("late forwarding record accepted")
	}
}
