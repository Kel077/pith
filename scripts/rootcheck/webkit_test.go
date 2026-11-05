package main

import (
	"context"
	"errors"
	"net"
	"net/netip"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/Kel077/pith/internal/api"
	"github.com/Kel077/pith/internal/engine/attribution"
	"github.com/Kel077/pith/internal/testprofile"
	"golang.org/x/sys/unix"
)

func webkitFixture(t *testing.T) (*session, *proofService, api.Config, api.Connection, webkitPCB) {
	t.Helper()
	s, _, _ := baselineFixture(t)
	app := filepath.Join(s.Dir, "Reviewed Safari.app")
	main := filepath.Join(app, "Contents", "MacOS", "Safari")
	if err := os.MkdirAll(filepath.Dir(main), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(main, []byte("synthetic executable; never run"), 0700); err != nil {
		t.Fatal(err)
	}
	metadata := []byte(`<?xml version="1.0"?><plist version="1.0"><dict><key>CFBundleIdentifier</key><string>com.apple.Safari</string><key>CFBundleExecutable</key><string>Safari</string></dict></plist>`)
	if err := os.WriteFile(filepath.Join(app, "Contents", "Info.plist"), metadata, 0600); err != nil {
		t.Fatal(err)
	}
	if err := s.freezeWebKit(context.Background(), app, func(_ context.Context, network, _ string) ([]netip.Addr, error) {
		if network == "ip6" {
			return nil, nil
		}
		return []netip.Addr{netip.MustParseAddr("192.0.2.20")}, nil
	}); err != nil {
		t.Fatal(err)
	}
	if err := s.writeWebKitPlan(); err != nil {
		t.Fatal(err)
	}
	networking := filepath.Join(s.Dir, "com.apple.WebKit.Networking")
	if err := os.WriteFile(networking, []byte("synthetic networking owner; never run"), 0700); err != nil {
		t.Fatal(err)
	}
	cfg := api.DefaultConfig()
	c := &proofService{session: s, cfg: cfg, since: time.Now(), capture: &api.CaptureIdentity{Token: "example-capture"}}
	row := api.Connection{ID: 2, App: s.WebKit.App, Process: api.Process{PID: 700, Path: networking}, Protocol: api.TCP, Local: "192.0.2.10:50000", Remote: s.Target, OutboundLocal: "198.51.100.10:50001", RuleID: "webkit-app", Outcome: api.Routed, Interface: s.Interfaces[0].Name, Sent: 300, Received: 500}
	proof := webkitPCB{Owner: row.Process, App: row.App, EffectivePID: 701, EffectiveExecutable: main, Delegated: true, Socket: attribution.Socket{PID: 700, EffectivePID: 701, Local: netip.MustParseAddrPort(row.Local), Remote: netip.MustParseAddrPort(row.Remote), Generation: 3}}
	return s, c, cfg, row, proof
}

func TestWebKitFrozenPlanAndAdmission(t *testing.T) {
	s, _, _, _, _ := webkitFixture(t)
	before, err := s.scopes()
	if err != nil {
		t.Fatal(err)
	}
	if err := s.reviewedWebKitPlan(); err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(s.webkitURL(), "https://example.com/?pith-rootcheck=") {
		t.Fatal(s.webkitURL())
	}
	oldBinding := s.cleanupBinding()
	s.WebKitPlanSHA256 = strings.Repeat("a", 64)
	if s.reviewedWebKitPlan() == nil || oldBinding == s.cleanupBinding() {
		t.Fatal("plan not bound to review and cleanup")
	}
	if err := s.writeWebKitPlan(); err != nil {
		t.Fatal(err)
	}
	after, _ := s.scopes()
	if !reflect.DeepEqual(before, after) {
		t.Fatal("browser plan widened capture")
	}
	s.TestProfile = &testprofile.Profile{Mode: testprofile.FullIPv4}
	if s.reviewedWebKitPlan() == nil {
		t.Fatal("profile admitted browser stage")
	}
	s.TestProfile = nil
	if err := os.WriteFile(s.WebKit.Executable, []byte("updated"), 0700); err != nil {
		t.Fatal(err)
	}
	if s.reviewedWebKitPlan() == nil {
		t.Fatal("updated browser accepted")
	}

	for _, opts := range []checkOptions{{webkitOpen: true}, {webkit: -time.Second}, {webkit: 61 * time.Second}, {webkit: time.Second, batch: true}, {webkit: time.Second}} {
		s, c, ops := baselineFixture(t)
		if err := checkWithOps(context.Background(), s, c, opts, ops); err == nil {
			t.Fatal("invalid browser mode admitted", opts)
		}
		if c.starts != 0 || c.stops != 0 {
			t.Fatal("invalid browser mode touched helper")
		}
	}
}

func TestWebKitMetadataCannotBlockOnFIFO(t *testing.T) {
	s, _, _, _, _ := webkitFixture(t)
	path := filepath.Join(s.WebKit.App.Path, "Contents", "Info.plist")
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if err := unix.Mkfifo(path, 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := inspectWebKitApp(s.WebKit.App.Path); err == nil {
		t.Fatal("FIFO accepted")
	}
}

func TestWebKitLaunchServicesCommandIsOnlyOneNavigation(t *testing.T) {
	app, url := "/Applications/Reviewed Safari.app", "https://example.com/?pith-rootcheck=nonce"
	cmd := webkitOpenCommand(context.Background(), app, url)
	if cmd.Path != "/usr/bin/open" || !reflect.DeepEqual(cmd.Args, []string{"/usr/bin/open", "-a", app, url}) || cmd.Stdin != nil {
		t.Fatal("unexpected browser control", cmd)
	}
	// Deliberately inspect only. Tests must never launch a browser or TCC prompt.
}

func TestWebKitFreshDelegationManualAndSingleOpen(t *testing.T) {
	for _, launch := range []bool{false, true} {
		for _, protocol := range []api.Protocol{api.TCP, api.UDP} {
			s, c, cfg, row, proof := webkitFixture(t)
			row.Protocol = protocol
			var sequence []string
			deps := webkitCheckDeps{
				ready: func(url string) error {
					sequence = append(sequence, "ready")
					row.Started, proof.At = time.Now(), time.Now()
					c.logs = []api.Connection{row}
					return nil
				},
				open: func(_ context.Context, app, url string) error {
					sequence = append(sequence, "open")
					if app != s.WebKit.App.Path || url != s.webkitURL() {
						t.Fatal("unfrozen navigation")
					}
					return nil
				},
				lookup: func(_ context.Context, p api.Protocol, local, remote netip.AddrPort) (webkitPCB, error) {
					if p != protocol || local.String() != row.Local || remote.String() != row.Remote {
						t.Fatal("wrong PCB query")
					}
					return proof, nil
				},
			}
			status, _ := c.Status(context.Background())
			var report checkReport
			if err := runWebKitCheck(context.Background(), s, c, cfg, status, &report, time.Second, launch, deps); err != nil {
				t.Fatal(err)
			}
			want := []string{"ready"}
			attempts := 0
			if launch {
				want = append(want, "open")
				attempts = 1
			}
			if !reflect.DeepEqual(sequence, want) || report.WebKit.OpenAttempts != attempts || report.WebKit.Result != "passed" || report.WebKit.PageBodyVerified || report.WebKit.PCB.Socket.Generation != 3 {
				t.Fatal(sequence, report.WebKit)
			}
			if len(c.saved) != 1 || len(c.saved[0].Scenes[0].Rules) != 1 || c.saved[0].Scenes[0].Rules[0].Apps[0] != s.WebKit.App {
				t.Fatal("wrong app policy")
			}
		}
	}
}

func TestWebKitIncompleteNeverRetriesOrClaimsPage(t *testing.T) {
	for _, mode := range []string{"open-error", "old-flow", "no-pcb", "wrong-tuple", "no-delegation", "other-effective-app", "wrong-executable", "no-bytes"} {
		t.Run(mode, func(t *testing.T) {
			s, c, cfg, row, proof := webkitFixture(t)
			openCalls, lookups := 0, 0
			deps := webkitCheckDeps{
				ready: func(string) error {
					row.Started, proof.At = time.Now(), time.Now()
					switch mode {
					case "old-flow":
						row.Started = row.Started.Add(-time.Hour)
					case "wrong-tuple":
						proof.Socket.Remote = netip.MustParseAddrPort("192.0.2.21:443")
					case "no-delegation":
						proof.Delegated = false
					case "other-effective-app":
						proof.App.BundleID = "org.example.other"
					case "wrong-executable":
						proof.EffectiveExecutable = row.Process.Path
					case "no-bytes":
						row.Received = 0
					}
					c.logs = []api.Connection{row}
					return nil
				},
				open: func(context.Context, string, string) error {
					openCalls++
					if mode == "open-error" {
						return errors.New("synthetic LaunchServices failure")
					}
					return nil
				},
				lookup: func(context.Context, api.Protocol, netip.AddrPort, netip.AddrPort) (webkitPCB, error) {
					lookups++
					if mode == "no-pcb" {
						return proof, attribution.ErrNotFound
					}
					return proof, nil
				},
			}
			status, _ := c.Status(context.Background())
			var report checkReport
			err := runWebKitCheck(context.Background(), s, c, cfg, status, &report, 15*time.Millisecond, true, deps)
			if err == nil || !strings.Contains(err.Error(), "incomplete") || openCalls != 1 || report.WebKit.Result != "incomplete" || report.WebKit.PageBodyVerified {
				t.Fatal(err, report.WebKit, openCalls)
			}
			if mode == "old-flow" && lookups != 0 {
				t.Fatal("old socket used")
			}
		})
	}
}

func TestWebKitIndependentProofRejectsWrongHelperRecord(t *testing.T) {
	for _, mode := range []string{"owner", "app", "rule", "interface", "outbound", "capture"} {
		t.Run(mode, func(t *testing.T) {
			s, c, cfg, row, proof := webkitFixture(t)
			status, _ := c.Status(context.Background())
			deps := webkitCheckDeps{ready: func(string) error {
				row.Started, proof.At = time.Now(), time.Now()
				switch mode {
				case "owner":
					row.Process.PID++
				case "app":
					row.App.BundleID = "org.example.other"
				case "rule":
					row.RuleID = ""
				case "interface":
					row.Interface = s.Interfaces[1].Name
				case "outbound":
					row.OutboundLocal = ""
				case "capture":
					c.since = c.since.Add(time.Second)
				}
				c.logs = []api.Connection{row}
				return nil
			}, lookup: func(context.Context, api.Protocol, netip.AddrPort, netip.AddrPort) (webkitPCB, error) {
				return proof, nil
			}}
			var report checkReport
			if err := runWebKitCheck(context.Background(), s, c, cfg, status, &report, time.Second, false, deps); err == nil || report.WebKit.Result != "failed" {
				t.Fatal(err, report.WebKit)
			}
		})
	}
}

type webkitLateCounters struct {
	*proofService
	reads int
}

func (c *webkitLateCounters) Connections(ctx context.Context, query api.ConnectionQuery) ([]api.Connection, error) {
	rows, err := c.proofService.Connections(ctx, query)
	if len(rows) != 0 {
		c.reads++
		if c.reads == 1 {
			rows[0].Received = 0
		}
	}
	return rows, err
}

func TestWebKitKeepsLivePCBProofWhileWaitingForCounters(t *testing.T) {
	s, base, cfg, row, proof := webkitFixture(t)
	c := &webkitLateCounters{proofService: base}
	lookups := 0
	deps := webkitCheckDeps{
		ready: func(string) error {
			row.Started, proof.At = time.Now(), time.Now()
			c.logs = []api.Connection{row}
			return nil
		},
		lookup: func(context.Context, api.Protocol, netip.AddrPort, netip.AddrPort) (webkitPCB, error) {
			lookups++
			if lookups > 1 {
				return webkitPCB{}, attribution.ErrNotFound
			}
			return proof, nil
		},
	}
	status, _ := c.Status(context.Background())
	var report checkReport
	if err := runWebKitCheck(context.Background(), s, c, cfg, status, &report, time.Second, false, deps); err != nil {
		t.Fatal(err)
	}
	if lookups != 1 || c.reads != 2 || report.WebKit.Result != "passed" {
		t.Fatal(lookups, c.reads, report.WebKit)
	}
}

func TestWebKitFreezesEveryARecordAndObservesItsOrdinal(t *testing.T) {
	s, c, cfg, row, proof := webkitFixture(t)
	calls := 0
	if err := s.freezeWebKit(context.Background(), s.WebKit.App.Path, func(ctx context.Context, network, host string) ([]netip.Addr, error) {
		calls++
		deadline, ok := ctx.Deadline()
		if (network != "ip4" && network != "ip6") || host != "example.com" || !ok || time.Until(deadline) > 5*time.Second {
			t.Fatal("unbounded or incorrect lookup")
		}
		if network == "ip6" {
			return nil, nil
		}
		return []netip.Addr{netip.MustParseAddr("192.0.2.21"), netip.MustParseAddr("192.0.2.20"), netip.MustParseAddr("192.0.2.21")}, nil
	}); err != nil {
		t.Fatal(err)
	}
	if err := s.writeWebKitPlan(); err != nil {
		t.Fatal(err)
	}
	scopes, err := s.scopes()
	if err != nil || len(scopes) != 2 || !reflect.DeepEqual(s.WebKit.Addresses, []string{"192.0.2.20", "192.0.2.21"}) {
		t.Fatal(scopes, err)
	}
	row.Remote, proof.Socket.Remote = "192.0.2.21:443", netip.MustParseAddrPort("192.0.2.21:443")
	deps := webkitCheckDeps{ready: func(string) error {
		row.Started, proof.At = time.Now(), time.Now()
		c.logs = []api.Connection{row}
		return nil
	}, lookup: func(_ context.Context, _ api.Protocol, _, remote netip.AddrPort) (webkitPCB, error) {
		if remote.String() != row.Remote {
			t.Fatal("queried original target instead of observed target")
		}
		return proof, nil
	}}
	status, _ := c.Status(context.Background())
	var report checkReport
	if err := runWebKitCheck(context.Background(), s, c, cfg, status, &report, time.Second, false, deps); err != nil {
		t.Fatal(err)
	}
	if calls != 4 || report.WebKit.TargetIndex != 2 || report.WebKit.Connection.Remote != "" || report.WebKit.PCB.Socket.Remote.IsValid() || len(c.saved[0].Scenes[0].Rules[0].Targets) != 2 {
		t.Fatal("ordinal or frozen policy mismatch", calls, report.WebKit)
	}
	if s.webkitTargetIndex("192.0.2.21:8443") != 0 || s.webkitTargetIndex("192.0.2.22:443") != 0 {
		t.Fatal("unfrozen destination admitted")
	}
}

func TestWebKitDNSPreflightWarnsWithoutExpandingFrozenScope(t *testing.T) {
	for _, mode := range []string{"stable", "rotation", "lookup-error", "ipv6", "ipv6-unknown", "no-aaaa"} {
		t.Run(mode, func(t *testing.T) {
			s, _, _, _, _ := webkitFixture(t)
			calls := 0
			err := s.freezeWebKit(context.Background(), s.WebKit.App.Path, func(_ context.Context, network, _ string) ([]netip.Addr, error) {
				calls++
				if network == "ip6" {
					switch mode {
					case "ipv6":
						return []netip.Addr{netip.MustParseAddr("2001:db8::1")}, nil
					case "ipv6-unknown":
						return nil, &net.DNSError{IsTimeout: true}
					case "no-aaaa":
						return nil, &net.DNSError{IsNotFound: true}
					}
					return nil, nil
				}
				if calls > 1 && mode == "lookup-error" {
					return nil, errors.New("synthetic DNS failure")
				}
				if calls > 1 && mode == "rotation" {
					return []netip.Addr{netip.MustParseAddr("192.0.2.21")}, nil
				}
				return []netip.Addr{netip.MustParseAddr("192.0.2.20")}, nil
			})
			if err != nil || calls != 4 || len(s.WebKit.Addresses) != 1 || s.WebKit.Addresses[0] != "192.0.2.20" {
				t.Fatal(err, calls, s.WebKit)
			}
			warn := mode != "stable" && mode != "no-aaaa"
			if (len(s.WebKit.DNSReview.Warnings) > 0) != warn {
				t.Fatal(mode, s.WebKit.DNSReview)
			}
			if err := s.writeWebKitPlan(); err != nil {
				t.Fatal(err)
			}
			if err := s.reviewedWebKitPlan(); err != nil {
				t.Fatal("DNS evidence broke immutable app review", err)
			}
		})
	}
}

func TestWebKitRejectsUnreviewableDNSAndCombinedScope(t *testing.T) {
	for _, mode := range []string{"empty", "too-many", "private", "ipv6", "missing-tls", "lookup-error"} {
		t.Run(mode, func(t *testing.T) {
			s, _, _, _, _ := webkitFixture(t)
			err := s.freezeWebKit(context.Background(), s.WebKit.App.Path, func(context.Context, string, string) ([]netip.Addr, error) {
				addresses := []netip.Addr{netip.MustParseAddr("192.0.2.20")}
				switch mode {
				case "empty":
					return nil, nil
				case "too-many":
					for i := byte(21); i <= 36; i++ {
						addresses = append(addresses, netip.AddrFrom4([4]byte{192, 0, 2, i}))
					}
				case "private":
					addresses = append(addresses, netip.MustParseAddr("10.0.0.1"))
				case "ipv6":
					addresses = append(addresses, netip.MustParseAddr("2001:db8::20"))
				case "missing-tls":
					addresses = []netip.Addr{netip.MustParseAddr("192.0.2.21")}
				case "lookup-error":
					return nil, errors.New("synthetic DNS failure")
				}
				return addresses, nil
			})
			if err == nil {
				t.Fatal("unsafe DNS set accepted")
			}
		})
	}
	s, _, _, _, _ := webkitFixture(t)
	s.WebKit.Addresses = nil
	for i := byte(20); i < 36; i++ {
		s.WebKit.Addresses = append(s.WebKit.Addresses, netip.AddrFrom4([4]byte{192, 0, 2, i}).String())
	}
	if scopes, err := s.scopes(); err != nil || len(scopes) != 16 {
		t.Fatal(scopes, err)
	}
	s.QUICTarget, s.QUICServerName = "198.51.100.20:443", "example.com"
	if _, err := s.scopes(); err == nil {
		t.Fatal("combined scope exceeded sixteen hosts")
	}
}
