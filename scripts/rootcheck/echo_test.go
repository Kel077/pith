package main

import (
	"context"
	"testing"

	"github.com/Kel077/pith/internal/api"
)

func TestEchoAllowsDriftButRejectsOtherKnownExits(t *testing.T) {
	for _, tt := range []struct {
		name, iface, actual string
		fail, drift         bool
	}{
		{"unchanged", "example0", "203.0.113.30", false, false},
		{"drift", "example0", "203.0.113.99", false, true},
		{"wrong-system", "example0", "203.0.113.20", true, false},
		{"wrong-other-adapter", "example0", "203.0.113.40", true, false},
		{"system-drift", "", "203.0.113.99", false, true},
		{"system-wrong-adapter", "", "203.0.113.30", true, false},
		{"missing", "example0", "", true, false},
		{"private", "example0", "10.0.0.1", true, false},
		{"wrong-family", "example0", "2001:db8::1", true, false},
	} {
		t.Run(tt.name, func(t *testing.T) {
			s, _, _ := appFixture()
			s.Preflight = append(s.Preflight, preflight{Interface: "example1", TLS: tlsEvidence{PublicIP: "203.0.113.40"}})
			entry := checkCase{TLS: tlsEvidence{PublicIP: tt.actual}}
			err := s.checkEcho(&entry, tt.iface)
			if (err != nil) != tt.fail || entry.ExitIPDrift != tt.drift || (entry.EvidenceNote != "") != tt.drift {
				t.Fatal("incorrect drift evidence", entry, err)
			}
		})
	}
}

func TestAppProofsAllowIndependentPhysicalAndSystemDrift(t *testing.T) {
	s, service, cfg := appFixture()
	service.mutate = func(conn *api.Connection, proof *requestProof) {
		if conn.Outcome == api.Routed {
			proof.TLS.PublicIP = "203.0.113.90"
		} else if conn.Outcome == api.System {
			proof.TLS.PublicIP = "203.0.113.91"
		}
	}
	self, child := "/tmp/pith-example/rootcheck", "/tmp/pith-example/request-tool/rootcheck-unmatched"
	var report checkReport
	if err := runAppProofs(context.Background(), s, service, cfg, service.since, "utun42", self, child, service.requester(self, 9001), service.requester(child, 9002), &report); err != nil {
		t.Fatal(err)
	}
	for _, entry := range report.Cases[:4] {
		if !entry.ExitIPDrift || entry.Connection.Process.PID != entry.RequestPID {
			t.Fatal("drift lost primary attribution evidence")
		}
	}
}
