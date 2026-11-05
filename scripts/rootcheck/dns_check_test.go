package main

import (
	"context"
	"errors"
	"net"
	"net/netip"
	"os"
	"slices"
	"testing"
	"time"

	"github.com/Kel077/pith/internal/api"
	"github.com/Kel077/pith/internal/rules"
	"golang.org/x/net/dns/dnsmessage"
)

type dnsService struct {
	baselineService
	session *session
	config  api.Config
	path    string
	sockets []*dnsConn
	queries []dnsmessage.Message
	corrupt func(*api.Connection)
}

func (s *dnsService) SaveConfig(_ context.Context, cfg api.Config) error {
	s.config = cfg
	return cfg.Validate()
}

type dnsConn struct {
	s              *dnsService
	local, remote  *net.UDPAddr
	row            int
	response       []byte
	denied, closed bool
}

func (c *dnsConn) Read(p []byte) (int, error) {
	if c.closed {
		return 0, net.ErrClosed
	}
	if c.denied {
		return 0, os.ErrDeadlineExceeded
	}
	n := copy(p, c.response)
	c.response = nil
	c.s.logRows[c.row].Received += uint64(n)
	return n, nil
}
func (c *dnsConn) Write(p []byte) (int, error) {
	if c.closed {
		return 0, net.ErrClosed
	}
	var query dnsmessage.Message
	if err := query.Unpack(p); err != nil {
		return 0, err
	}
	c.s.queries = append(c.s.queries, query)
	if c.row < 0 || !c.s.logRows[c.row].Ended.IsZero() {
		policy, err := rules.New(c.s.config.Scenes[0])
		if err != nil {
			return 0, err
		}
		decision := policy.Match(rules.Flow{Protocol: api.UDP, Address: c.remote.AddrPort().Addr()})
		outcome, iface := decision.Predict(c.s.session.Original.Interfaces)
		if outcome == api.System {
			iface = "example0"
		}
		row := api.Connection{ID: uint64(len(c.s.logRows) + 1), Started: time.Now(), Process: api.Process{PID: os.Getpid(), Path: c.s.path},
			Protocol: api.UDP, Local: c.local.String(), Remote: c.remote.String(), Outcome: outcome, Interface: iface, RuleID: decision.RuleID}
		c.denied = outcome == api.Blocked || outcome == api.Unreachable
		if c.denied {
			row.Ended = time.Now()
		} else {
			row.OutboundLocal = "198.51.100.10:50001"
		}
		if c.s.corrupt != nil {
			c.s.corrupt(&row)
		}
		c.s.logRows = append(c.s.logRows, row)
		c.row = len(c.s.logRows) - 1
	}
	if !c.denied {
		c.s.logRows[c.row].Sent += uint64(len(p))
		response := dnsResponseFixture(query.ID, query.Questions[0].Type)
		c.response, _ = response.Pack()
	}
	return len(p), nil
}
func (c *dnsConn) Close() error                     { c.closed = true; return nil }
func (c *dnsConn) LocalAddr() net.Addr              { return c.local }
func (c *dnsConn) RemoteAddr() net.Addr             { return c.remote }
func (c *dnsConn) SetDeadline(time.Time) error      { return nil }
func (c *dnsConn) SetReadDeadline(time.Time) error  { return nil }
func (c *dnsConn) SetWriteDeadline(time.Time) error { return nil }

func dnsFixture(t *testing.T) (*session, *dnsService, dnsCheckDeps, api.Config) {
	t.Helper()
	s, baseline, _ := baselineFixture(t)
	s.DNSTarget, s.ExtraDNSTargets, s.DNSName = "192.0.2.53:53", []string{"198.51.100.53:53"}, "example.com"
	path, err := executablePath()
	if err != nil {
		t.Fatal(err)
	}
	baseline.status.State = api.Running
	baseline.status.Since = time.Now()
	baseline.status.LastCapture = &api.CaptureIdentity{Token: "dns-capture", Interface: "utun90", Index: 90, Prefixes: []string{"192.0.2.53/32", "198.51.100.53/32"}}
	c := &dnsService{baselineService: *baseline, session: s, path: path}
	deps := dnsCheckDeps{
		dial: func(ctx context.Context, target string) (net.Conn, error) {
			endpoint, err := netip.ParseAddrPort(target)
			if err != nil {
				return nil, err
			}
			if !slices.Contains(s.dnsTargets(), target) {
				t.Fatal("unreviewed DNS target", target)
			}
			conn := &dnsConn{s: c, local: &net.UDPAddr{IP: net.ParseIP("192.0.2.10"), Port: 51000 + len(c.sockets)}, remote: net.UDPAddrFromAddrPort(endpoint), row: -1}
			c.sockets = append(c.sockets, conn)
			return conn, nil
		},
		idle: func(_ context.Context, _ api.Service, _ api.Status, row api.Connection, _ time.Time) (api.Connection, error) {
			for i := range c.logRows {
				if c.logRows[i].ID == row.ID {
					c.logRows[i].Ended = time.Now()
					return c.logRows[i], nil
				}
			}
			return api.Connection{}, errors.New("unknown idle session")
		},
	}
	cfg := api.Config{Version: api.ConfigVersion, Selected: "test", Scenes: []api.Scene{{ID: "test", Name: "DNS acceptance"}}, Settings: api.DefaultSettings()}
	return s, c, deps, cfg
}

func TestDNSChecksSmokeThenFiniteBatch(t *testing.T) {
	for _, batch := range []bool{false, true} {
		t.Run(map[bool]string{false: "smoke", true: "batch"}[batch], func(t *testing.T) {
			s, c, deps, cfg := dnsFixture(t)
			report := checkReport{Before: s.Original}
			if err := runDNSChecks(context.Background(), s, c, cfg, c.status, &report, batch, deps); err != nil {
				t.Fatal(err)
			}
			want := 1
			if batch {
				want = 20
			}
			if len(c.queries) != want {
				t.Fatalf("queries=%d want=%d", len(c.queries), want)
			}
			ids := map[uint16]bool{}
			for _, query := range c.queries {
				if ids[query.ID] {
					t.Fatal("reused query nonce")
				}
				ids[query.ID] = true
				if len(query.Questions) != 1 || query.Questions[0].Name.String() != "example.com." {
					t.Fatal(query)
				}
			}
			for _, conn := range c.sockets {
				if !conn.closed {
					t.Fatal("left fixture socket open")
				}
			}
			if batch {
				var reuse, hot, beforeIdle, afterIdle *checkCase
				for i := range report.Cases {
					entry := &report.Cases[i]
					switch entry.Name {
					case "DNS resolver 1: A and AAAA reuse one session on A":
						reuse = entry
					case "DNS resolver 1: open UDP session keeps A after hot reload":
						hot = entry
					case "DNS resolver 1: open DNS session before idle expiry":
						beforeIdle = entry
					case "DNS resolver 1: same source port creates a new session on B after idle":
						afterIdle = entry
					}
				}
				if reuse == nil || hot == nil || beforeIdle == nil || afterIdle == nil {
					t.Fatal("missing lifecycle evidence")
				}
				if reuse.Connection.ID != hot.Connection.ID || reuse.Connection.OutboundLocal != hot.Connection.OutboundLocal {
					t.Fatal("hot reload migrated an open UDP flow")
				}
				if beforeIdle.Connection.ID == afterIdle.Connection.ID || beforeIdle.Connection.Local != afterIdle.Connection.Local || afterIdle.Connection.Interface != "example1" {
					t.Fatal("idle replacement did not retain app port and take new policy")
				}
			}
		})
	}
}

func TestDNSChecksStopAtFirstIncorrectPathAndCloseSockets(t *testing.T) {
	s, c, deps, cfg := dnsFixture(t)
	c.corrupt = func(row *api.Connection) { row.Interface = "unexpected0" }
	report := checkReport{Before: s.Original}
	if err := runDNSChecks(context.Background(), s, c, cfg, c.status, &report, true, deps); err == nil {
		t.Fatal("accepted wrong interface")
	}
	if len(c.queries) != 1 || len(report.Cases) != 1 || report.Cases[0].Error == "" {
		t.Fatal("failure did not stop the batch")
	}
	for _, conn := range c.sockets {
		if !conn.closed {
			t.Fatal("failure left a socket open")
		}
	}
}

func TestDNSRecordRejectsWrongIdentityReuseBytesAndPorts(t *testing.T) {
	want := dnsFlowExpectation{PID: 42, Path: "/example/rootcheck", Local: "192.0.2.10:50000", Remote: "192.0.2.53:53", AfterID: 10, ID: 11, OutboundLocal: "198.51.100.10:50001", Outcome: api.Routed, Interface: "example0", RuleID: "dns-a", Sent: 29, Received: 45}
	row := api.Connection{ID: 11, Process: api.Process{PID: want.PID, Path: want.Path}, Protocol: api.UDP, Local: want.Local, Remote: want.Remote, OutboundLocal: want.OutboundLocal, Outcome: want.Outcome, Interface: want.Interface, RuleID: want.RuleID, Sent: want.Sent, Received: want.Received}
	if _, ok, err := checkDNSRecord([]api.Connection{row}, want); !ok || err != nil {
		t.Fatal(ok, err)
	}
	for _, name := range []string{"stale", "pid", "local", "remote", "tcp", "path", "rule", "interface", "outcome", "new-id", "source-port", "missing-outbound", "sent", "received", "ended", "duplicate"} {
		t.Run(name, func(t *testing.T) {
			bad := row
			switch name {
			case "stale":
				bad.ID = 10
			case "pid":
				bad.Process.PID++
			case "local":
				bad.Local = "192.0.2.10:50002"
			case "remote":
				bad.Remote = "198.51.100.53:53"
			case "tcp":
				bad.Protocol = api.TCP
			case "path":
				bad.Process.Path = "/other/tool"
			case "rule":
				bad.RuleID = "other-rule"
			case "interface":
				bad.Interface = "example1"
			case "outcome":
				bad.Outcome = api.System
			case "new-id":
				bad.ID++
			case "source-port":
				bad.OutboundLocal = "198.51.100.10:50002"
			case "missing-outbound":
				bad.OutboundLocal = ""
			case "sent":
				bad.Sent++
			case "received":
				bad.Received++
			case "ended":
				bad.Ended = time.Now()
			}
			rows := []api.Connection{bad}
			if name == "duplicate" {
				rows = append(rows, row)
			}
			if _, ok, _ := checkDNSRecord(rows, want); ok {
				t.Fatal("accepted incorrect DNS flow")
			}
		})
	}
	want.Outcome, want.Interface, want.RuleID, want.OutboundLocal, want.Sent, want.Received = api.Blocked, "", "block", "", 0, 0
	row.Outcome, row.Interface, row.RuleID, row.OutboundLocal, row.Sent, row.Received, row.Ended = api.Blocked, "", "block", "", 0, 0, time.Now()
	if _, ok, err := checkDNSRecord([]api.Connection{row}, want); !ok || err != nil {
		t.Fatal(ok, err)
	}
	row.OutboundLocal = "198.51.100.10:50001"
	if _, _, err := checkDNSRecord([]api.Connection{row}, want); err == nil {
		t.Fatal("blocked flow has outbound evidence")
	}
}

func TestDNSIdleRequiresExpiryWithoutEarlyFailure(t *testing.T) {
	_, c, _, _ := dnsFixture(t)
	row := api.Connection{ID: 1, Local: "192.0.2.10:50000", OutboundLocal: "198.51.100.10:50001", Sent: 29, Received: 45}
	c.logRows = []api.Connection{row}
	c.logRows[0].Ended = time.Now()
	if _, err := waitDNSIdle(context.Background(), c, c.status, row, time.Now()); err == nil {
		t.Fatal("early failure claimed idle expiry")
	}
	if _, err := waitDNSIdle(context.Background(), c, c.status, row, time.Now().Add(-11*time.Second)); err != nil {
		t.Fatal(err)
	}
	c.logRows[0].Problem = "forwarding failed"
	if _, err := waitDNSIdle(context.Background(), c, c.status, row, time.Now().Add(-11*time.Second)); err == nil {
		t.Fatal("forwarding error claimed idle expiry")
	}
}
