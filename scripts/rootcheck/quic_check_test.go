package main

import (
	"context"
	"os"
	"strings"
	"testing"

	"github.com/Kel077/pith/internal/api"
	"github.com/Kel077/pith/internal/testprofile"
)

func quicEvidenceFixture(fragmented bool) quicEvidence {
	e := quicEvidence{Local: "192.0.2.10:50000", Remote: "203.0.113.20:443", Nonce: strings.Repeat("a", 32), StatusCode: 200, Protocol: "HTTP/3.0", ALPN: "h3", Verified: true, BodyBytes: 20, BodyPrefixSHA256: strings.Repeat("b", 64), FirstDatagramSNI: "example.com", FragmentedHello: fragmented, Sent: 2400, Received: 3600, SentDatagrams: 2, ReceivedDatagrams: 3}
	if fragmented {
		e.FirstDatagramSNI = ""
	}
	return e
}

func TestQUICChecksFiniteRequestsAndFailFast(t *testing.T) {
	for _, mode := range []string{"smoke", "batch", "wrong-path", "wrong-sni", "unverified"} {
		t.Run(mode, func(t *testing.T) {
			s, c, _, cfg := dnsFixture(t)
			s.QUICTarget, s.QUICServerName = "203.0.113.20:443", "example.com"
			requests := 0
			report := checkReport{Before: s.Original}
			err := runQUICChecks(context.Background(), s, c, cfg, c.status, &report, mode != "smoke", func(ctx context.Context, fragmented bool) (quicEvidence, error) {
				requests++
				e := quicEvidenceFixture(fragmented)
				rule := c.config.Scenes[0].Rules[0]
				row := api.Connection{ID: uint64(requests), Process: api.Process{PID: os.Getpid(), Path: c.path}, Protocol: api.UDP, Local: e.Local, Remote: e.Remote, OutboundLocal: "198.51.100.10:50001", Domain: e.FirstDatagramSNI, RuleID: rule.ID, Interface: rule.Via[0].Name, Outcome: api.Routed, Sent: e.Sent, Received: e.Received}
				switch mode {
				case "wrong-path":
					row.Interface = "unexpected0"
				case "wrong-sni":
					e.FirstDatagramSNI = "other.example.com"
				case "unverified":
					e.Verified = false
				}
				c.logRows = append(c.logRows, row)
				return e, nil
			})
			positive := mode == "smoke" || mode == "batch"
			if (err == nil) != positive {
				t.Fatal(err, report)
			}
			want := 1
			if mode == "batch" {
				want = 3
			}
			if requests != want || len(report.Cases) != want {
				t.Fatal("incorrect request/report count", requests, report)
			}
			if mode == "batch" && (!report.Cases[2].QUIC.FragmentedHello || report.Cases[2].Connection.Domain != "") {
				t.Fatal("missing negative SNI control")
			}
		})
	}
}

func TestQUICEvidenceRejectsWrongProtocolAndLimits(t *testing.T) {
	for _, name := range []string{"remote", "local", "http2", "status", "alpn", "trust", "nonce", "body", "send-budget", "receive-budget", "zero-bytes", "packets", "sni", "fragment", "error"} {
		t.Run(name, func(t *testing.T) {
			e := quicEvidenceFixture(false)
			switch name {
			case "remote":
				e.Remote = "198.51.100.20:443"
			case "local":
				e.Local = "0.0.0.0:50000"
			case "http2":
				e.Protocol = "HTTP/2.0"
			case "status":
				e.StatusCode = 500
			case "alpn":
				e.ALPN = "h2"
			case "trust":
				e.Verified = false
			case "nonce":
				e.Nonce = ""
			case "body":
				e.BodyBytes = quicBodyLimit + 1
			case "send-budget":
				e.Sent = quicSendLimit + 1
			case "receive-budget":
				e.Received = quicReceiveLimit + 1
			case "zero-bytes":
				e.Received = 0
			case "packets":
				e.SentDatagrams = 129
			case "sni":
				e.FirstDatagramSNI = ""
			case "fragment":
				e.FragmentedHello = true
			case "error":
				e.Error = "failed"
			}
			if err := validateQUICEvidence(e, "203.0.113.20:443", "example.com", false); err == nil {
				t.Fatal("accepted invalid QUIC evidence")
			}
		})
	}
}

func TestQUICRecordRequiresFreshExactFlowAndAttributedName(t *testing.T) {
	e := quicEvidenceFixture(false)
	row := api.Connection{ID: 11, Process: api.Process{PID: os.Getpid(), Path: "/example/rootcheck"}, Protocol: api.UDP, Local: e.Local, Remote: e.Remote, OutboundLocal: "198.51.100.10:50001", Domain: e.FirstDatagramSNI, RuleID: "quic", Interface: "example0", Outcome: api.Routed, Sent: e.Sent, Received: e.Received}
	for _, name := range []string{"good", "stale", "pid", "path", "tcp", "local", "remote", "rule", "interface", "outcome", "domain", "no-outbound", "sent", "received", "duplicate"} {
		t.Run(name, func(t *testing.T) {
			bad := row
			switch name {
			case "stale":
				bad.ID = 10
			case "pid":
				bad.Process.PID++
			case "path":
				bad.Process.Path = "/another/tool"
			case "tcp":
				bad.Protocol = api.TCP
			case "local":
				bad.Local = "192.0.2.10:50002"
			case "remote":
				bad.Remote = "198.51.100.20:443"
			case "rule":
				bad.RuleID = "different"
			case "interface":
				bad.Interface = "example1"
			case "outcome":
				bad.Outcome = api.System
			case "domain":
				bad.Domain = ""
			case "no-outbound":
				bad.OutboundLocal = ""
			case "sent":
				bad.Sent--
			case "received":
				bad.Received--
			}
			rows := []api.Connection{bad}
			if name == "duplicate" {
				rows = append(rows, bad)
			}
			_, ok, err := checkQUICRecord(rows, 10, row.Process.Path, "quic", "example0", e)
			if ok != (name == "good") {
				t.Fatal("incorrect record acceptance", ok, err)
			}
		})
	}
}

func TestQUICPlanFreezesEndpointAndRequiresFreshSmoke(t *testing.T) {
	s, c, ops := baselineFixture(t)
	s.QUICTarget, s.QUICServerName = "203.0.113.20:443", "example.com"
	if err := s.writeQUICPlan(); err != nil {
		t.Fatal(err)
	}
	if err := s.reviewedQUICPlan(); err != nil {
		t.Fatal(err)
	}
	if scopes, err := s.scopes(); err != nil || len(scopes) != 2 || scopes[1].String() != "203.0.113.20/32" {
		t.Fatal(scopes, err)
	}
	if err := checkWithOps(context.Background(), s, c, checkOptions{quicBatch: true}, ops); err == nil {
		t.Fatal("batch did not require QUIC smoke")
	}
	if c.starts != 0 || c.stops != 0 {
		t.Fatal("invalid admission touched routing")
	}
	s.QUICServerName = "other.example.com"
	if err := s.reviewedQUICPlan(); err == nil {
		t.Fatal("accepted changed certificate name")
	}
	s.QUICServerName = "example.com"
	for _, target := range []string{"198.51.100.20:443", "203.0.113.20:8443", "127.0.0.1:443", "[2001:db8::20]:443", "example.com:443"} {
		s.QUICTarget = target
		if err := s.reviewedQUICPlan(); err == nil {
			t.Fatal("accepted changed/invalid target", target)
		}
	}
	s.QUICTarget = "203.0.113.20:443"
	s.TestProfile = &testprofile.Profile{Mode: testprofile.FullIPv4}
	if _, err := s.scopes(); err == nil {
		t.Fatal("enabled QUIC stage in unreviewed mode")
	}
}
