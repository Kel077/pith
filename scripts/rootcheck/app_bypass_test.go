package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/netip"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Kel077/pith/internal/api"
	"github.com/Kel077/pith/internal/rules"
)

type proofService struct {
	api.Service
	session *session
	cfg     api.Config
	since   time.Time
	logs    []api.Connection
	saved   []api.Config
	mutate  func(*api.Connection, *requestProof)
	closed  int
	capture *api.CaptureIdentity
}

func (m *proofService) SaveConfig(_ context.Context, cfg api.Config) error {
	if err := cfg.Validate(); err != nil {
		return err
	}
	// Keep each rule set independent of later edits to the scene slice.
	data, _ := json.Marshal(cfg)
	var copy api.Config
	_ = json.Unmarshal(data, &copy)
	m.cfg = copy
	m.saved = append(m.saved, copy)
	return nil
}

func (m *proofService) Connections(context.Context, api.ConnectionQuery) ([]api.Connection, error) {
	return append([]api.Connection(nil), m.logs...), nil
}

func (m *proofService) Status(context.Context) (api.Status, error) {
	return api.Status{State: api.Running, Since: m.since, LastCapture: m.capture}, nil
}

func (m *proofService) requester(path string, pid int) proofRequest {
	return m.requesterWithApp(path, pid, api.AppRef{Path: path})
}

func (m *proofService) requesterWithApp(path string, pid int, app api.AppRef) proofRequest {
	return func(context.Context) (requestProof, func() error, error) {
		set, err := rules.New(m.cfg.Scenes[0])
		if err != nil {
			return requestProof{}, nil, err
		}
		address, _ := netip.ParseAddrPort(m.session.Target)
		process := api.Process{PID: pid, Path: path}
		decision := set.Match(rules.Flow{App: app, Process: process, Address: address.Addr(), Domain: m.session.ServerName, Protocol: api.TCP})
		outcome, iface := decision.Predict(m.session.Original.Interfaces)
		publicIP := m.session.baselineIP(iface)
		if outcome == api.System {
			iface, publicIP = "utun42", m.session.baselineIP("")
		}
		proof := requestProof{PID: pid, Executable: path, Target: m.session.Target, TLS: tlsEvidence{Local: "192.0.2.10:50000", StatusCode: 200, BodyBytes: len(publicIP), PublicIP: publicIP}}
		conn := api.Connection{ID: uint64(len(m.logs) + 1), Started: time.Now(), Ended: time.Now(), App: app, Process: process, Protocol: api.TCP, Local: proof.TLS.Local, Remote: m.session.Target, Domain: m.session.ServerName, RuleID: decision.RuleID, Outcome: outcome, Interface: iface, Sent: 300, Received: 500}
		conn.OutboundLocal = "198.51.100.10:50001"
		if outcome == api.Blocked {
			conn.OutboundLocal = ""
			proof.Problem, proof.TLS = "blocked test request", tlsEvidence{Local: proof.TLS.Local}
			conn.Sent, conn.Received = 0, 0
		}
		if m.mutate != nil {
			m.mutate(&conn, &proof)
		}
		m.logs = append(m.logs, conn)
		return proof, func() error { m.closed++; return nil }, nil
	}
}

func appFixture() (*session, *proofService, api.Config) {
	s := &session{Installed: true, CancelPassed: true, SmokePassed: true, Target: "192.0.2.20:443", ServerName: "example.com", Interfaces: [2]api.InterfaceRef{{Name: "example0"}, {Name: "example1"}}}
	s.Original.Interfaces = []api.Interface{{Name: "example0", Kind: api.KindEthernet, Up: true}, {Name: "example1", Kind: api.KindEthernet, Up: true}}
	s.Preflight = []preflight{{Interface: "", TLS: tlsEvidence{PublicIP: "203.0.113.20"}}, {Interface: "example0", TLS: tlsEvidence{PublicIP: "203.0.113.30"}}}
	cfg := api.DefaultConfig()
	cfg.Settings.SafeStart, cfg.Settings.IPv6 = true, false
	m := &proofService{session: s, since: time.Now(), cfg: cfg}
	return s, m, cfg
}

func TestAppBypassProofsKeepFrozenTargetAndUnmatchedSystemRoute(t *testing.T) {
	s, service, cfg := appFixture()
	self, child := "/tmp/pith-example/rootcheck", "/tmp/pith-example/request-tool/rootcheck-unmatched"
	var report checkReport
	err := runAppProofs(context.Background(), s, service, cfg, service.since, "utun42", self, child, service.requester(self, 9001), service.requester(child, 9002), &report)
	if err != nil {
		t.Fatal(err)
	}
	wantOutcomes := []api.Outcome{api.Routed, api.System, api.Routed, api.System, api.Blocked}
	wantRules := []string{"app-ip", "", "app-domain", "", "app-block"}
	if len(report.Cases) != len(wantOutcomes) || service.closed != len(wantOutcomes) {
		t.Fatal("missing cases or request cleanup", len(report.Cases), service.closed)
	}
	for i, entry := range report.Cases {
		if entry.Connection.Outcome != wantOutcomes[i] || entry.Connection.RuleID != wantRules[i] || entry.Connection.Remote != s.Target || entry.RequestTarget != s.Target || entry.RequestPID != entry.Connection.Process.PID || entry.RequestPath != entry.Connection.Process.Path {
			t.Fatalf("case %d lost process/target/rule evidence: %+v", i, entry)
		}
	}
	for _, saved := range service.saved {
		if !saved.Settings.SafeStart || saved.Settings.IPv6 || saved.Selected != cfg.Selected {
			t.Fatal("app mode changed the safety settings or selected scene")
		}
		for _, rule := range saved.Scenes[0].Rules {
			if len(rule.Apps) != 1 || rule.Apps[0].Path != self || len(rule.Targets) != 1 || rule.Targets[0] != "192.0.2.20" && rule.Targets[0] != s.ServerName {
				t.Fatal("app mode widened its identity or target scope", rule)
			}
		}
	}
}

func TestAppCaseRejectsIncorrectEvidenceAndAlwaysFinishesRequester(t *testing.T) {
	for _, scenario := range []struct {
		name   string
		mutate func(*api.Connection, *requestProof)
	}{
		{"wrong PID", func(c *api.Connection, _ *requestProof) { c.Process.PID++ }},
		{"wrong target", func(c *api.Connection, _ *requestProof) { c.Remote = "192.0.2.21:443" }},
		{"wrong rule", func(c *api.Connection, _ *requestProof) { c.RuleID = "unrelated" }},
		{"wrong interface", func(c *api.Connection, _ *requestProof) { c.Interface = "utun42" }},
		{"wrong process path", func(c *api.Connection, _ *requestProof) { c.Process.Path += "-other" }},
		{"wrong app path", func(c *api.Connection, _ *requestProof) { c.App.Path += "-other" }},
		{"wrong domain", func(c *api.Connection, _ *requestProof) { c.Domain = "other.example" }},
		{"missing bytes", func(c *api.Connection, _ *requestProof) { c.Received = 0 }},
		{"missing public echo", func(_ *api.Connection, p *requestProof) { p.TLS.PublicIP = "" }},
		{"wrong public echo", func(_ *api.Connection, p *requestProof) { p.TLS.PublicIP = "203.0.113.20" }},
		{"request changed target", func(_ *api.Connection, p *requestProof) { p.Target = "192.0.2.21:443" }},
		{"request changed path", func(_ *api.Connection, p *requestProof) { p.Executable += "-other" }},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			s, service, cfg := appFixture()
			self := "/tmp/pith-example/rootcheck"
			rule := api.Rule{ID: "app-domain", Enabled: true, Apps: []api.AppRef{{Path: self}}, Targets: []string{s.ServerName}, Protocol: api.TCP, Action: api.Route, Via: []api.InterfaceRef{s.Interfaces[0]}}
			service.mutate = scenario.mutate
			test := appCase{Name: scenario.name, Rules: []api.Rule{rule}, Outcome: api.Routed, RuleID: rule.ID, Interface: "example0", Executable: self, ExpectedIP: s.baselineIP("example0"), RequireDomain: true, Request: service.requester(self, 9001)}
			ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
			defer cancel()
			entry, err := runAppCase(ctx, s, service, cfg, service.since, test)
			if err == nil || entry.Error == "" || service.closed != 1 {
				t.Fatalf("bad evidence passed or request remained open: entry=%+v err=%v closed=%d", entry, err, service.closed)
			}
		})
	}
}

func TestAppBypassRequiresSmokeAndPublicEchoBeforeHelperAccess(t *testing.T) {
	s, _, _ := appFixture()
	s.SmokePassed = false
	if err := check(context.Background(), s, nil, checkOptions{appBypass: true}); err == nil || !strings.Contains(err.Error(), "smoke") {
		t.Fatal("app mode did not enforce the smoke prerequisite", err)
	}
	s.SmokePassed = true
	s.Preflight[0].TLS.PublicIP = ""
	if err := check(context.Background(), s, nil, checkOptions{appBypass: true}); err == nil || !strings.Contains(err.Error(), "public-IP") {
		t.Fatal("app mode tried helper access without public-IP evidence", err)
	}
}

func TestAppBypassReportsIndistinguishableExitEvidence(t *testing.T) {
	s, service, cfg := appFixture()
	s.Preflight[0].TLS.PublicIP = s.Preflight[1].TLS.PublicIP
	self, child := "/tmp/pith-example/rootcheck", "/tmp/pith-example/request-tool/rootcheck-unmatched"
	var report checkReport
	if err := runAppProofs(context.Background(), s, service, cfg, service.since, "utun42", self, child, service.requester(self, 9001), service.requester(child, 9002), &report); err != nil {
		t.Fatal(err)
	}
	if len(report.Notes) != 1 || !strings.Contains(report.Notes[0], "cannot establish") {
		t.Fatal("identical exit evidence was presented as proof of bypass")
	}
}

func TestAppBypassRejectsTunnelNamesWithoutKindMetadata(t *testing.T) {
	for _, name := range []string{"utun42", "ppp0", "ipsec0"} {
		t.Run(name, func(t *testing.T) {
			s, _, _ := appFixture()
			s.Interfaces[0].Name = name
			s.Original.Interfaces[0].Name, s.Original.Interfaces[0].Kind = name, api.KindOther
			s.Preflight[1].Interface = name
			if err := appBypassPreflight(s); err == nil {
				t.Fatal("tunnel was accepted as a physical bypass interface")
			}
		})
	}
}

func TestServeRequestUsesFrozenTargetOnceWithoutHelperAccess(t *testing.T) {
	s, _, _ := appFixture()
	var output bytes.Buffer
	calls := 0
	err := serveRequest(context.Background(), s, io.NopCloser(strings.NewReader("done")), &output, func(ctx context.Context) (tlsEvidence, error) {
		calls++
		if deadline, ok := ctx.Deadline(); !ok || time.Until(deadline) > requestLifetime {
			t.Error("request did not get its own deadline")
		}
		return tlsEvidence{StatusCode: 200, PublicIP: "203.0.113.20"}, nil
	})
	if err != nil || calls != 1 {
		t.Fatal("single mocked request failed", calls, err)
	}
	var proof requestProof
	if err := json.Unmarshal(output.Bytes(), &proof); err != nil || proof.Target != s.Target || proof.PID != os.Getpid() || proof.Problem != "" {
		t.Fatal("invalid child request evidence", proof, err)
	}
	s.SmokePassed = false
	if err := serveRequest(context.Background(), s, io.NopCloser(strings.NewReader("done")), io.Discard, func(context.Context) (tlsEvidence, error) {
		t.Fatal("unapproved child request reached networking")
		return tlsEvidence{}, nil
	}); err == nil {
		t.Fatal("child request accepted an unprepared session")
	}
}

func TestServeRequestCancellationReleasesAcknowledgmentWait(t *testing.T) {
	s, _, _ := appFixture()
	input, writer := io.Pipe()
	defer writer.Close()
	ctx, cancel := context.WithCancel(context.Background())
	err := serveRequest(ctx, s, input, io.Discard, func(context.Context) (tlsEvidence, error) {
		cancel()
		return tlsEvidence{}, context.Canceled
	})
	if !errors.Is(err, context.Canceled) {
		t.Fatal("canceled child waited for an absent parent", err)
	}
}

func TestRequestToolCopyIsPrivateDistinctAndRemovable(t *testing.T) {
	dir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(dir, 0700); err != nil {
		t.Fatal(err)
	}
	source := filepath.Join(dir, "rootcheck")
	if err := os.WriteFile(source, []byte("synthetic executable bytes"), 0700); err != nil {
		t.Fatal(err)
	}
	copy, cleanup, err := copyRequestTool(dir, source)
	if err != nil {
		t.Fatal(err)
	}
	defer cleanup()
	if copy == source || filepath.Dir(copy) == dir || !strings.HasPrefix(copy, dir+string(filepath.Separator)) {
		t.Fatal("copy does not have a distinct path under the session")
	}
	a, _ := fileHash(source)
	b, _ := fileHash(copy)
	if a != b {
		t.Fatal("copy changed executable bytes")
	}
	cleanup()
	if _, err := os.Stat(copy); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("temporary request executable remained after cleanup", err)
	}
	if _, err := os.Stat(source); err != nil {
		t.Fatal("copy cleanup removed the original executable", err)
	}
}
