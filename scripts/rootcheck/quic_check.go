package main

import (
	"context"
	"errors"
	"fmt"
	"net/netip"
	"os"
	"time"

	"github.com/Kel077/pith/internal/api"
)

type quicExchange func(context.Context, bool) (quicEvidence, error)

func runQUICChecks(ctx context.Context, s *session, c api.Service, cfg api.Config, running api.Status, report *checkReport, batch bool, exchange quicExchange) error {
	path, err := executablePath()
	if err != nil {
		return err
	}
	endpoint, _ := netip.ParseAddrPort(s.QUICTarget)
	type testCase struct {
		name, target string
		adapter      int
		fragmented   bool
	}
	cases := []testCase{{"HTTP/3 IP smoke through A", endpoint.Addr().String(), 0, false}}
	if batch {
		cases = []testCase{
			{"HTTP/3 domain rule from observable Initial SNI", s.QUICServerName, 0, false},
			{"HTTP/3 IP rule through B", endpoint.Addr().String(), 1, false},
			{"HTTP/3 IP rule with fragmented Initial ClientHello", endpoint.Addr().String(), 0, true},
		}
	}
	report.Notes = append(report.Notes, "QUIC v1 / HTTP/3 only, verified TLS, one request per case; no proxy, redirect, HTTP retry, TCP fallback, migration or 0-RTT.", "The fragmented ClientHello control proves IP routing without recoverable first-datagram SNI, not ECH support or arbitrary multi-packet name recovery.", "Packet counters cover sent/read UDP payload only; public NAT ports, network headers and queued/dropped kernel packets are not measured.")
	for i, tc := range cases {
		entry := checkCase{Name: tc.name, RequestPID: os.Getpid(), RequestPath: path, RequestTarget: s.QUICTarget}
		rule := api.Rule{ID: fmt.Sprintf("quic-%d", i), Enabled: true, Targets: []string{tc.target}, Protocol: api.UDP, Action: api.Route, Via: []api.InterfaceRef{s.Interfaces[tc.adapter]}}
		cfg.Scenes[0].Rules = []api.Rule{rule}
		cfg.Settings.SniffDomains = true
		if err := c.SaveConfig(ctx, cfg); err != nil {
			return err
		}
		maxID, err := maxConnectionID(ctx, c)
		if err != nil {
			return err
		}
		evidence, err := exchange(ctx, tc.fragmented)
		entry.QUIC = &evidence
		if err == nil {
			err = validateQUICEvidence(evidence, s.QUICTarget, s.QUICServerName, tc.fragmented)
		}
		if err == nil {
			pollCtx, cancel := context.WithTimeout(ctx, 3*time.Second)
			err = waitUntil(pollCtx, func() (bool, error) {
				rows, err := c.Connections(pollCtx, api.ConnectionQuery{Limit: 500})
				if err != nil {
					return false, err
				}
				var ok bool
				entry.Connection, ok, err = checkQUICRecord(rows, maxID, path, rule.ID, s.Interfaces[tc.adapter].Name, evidence)
				return ok, err
			})
			cancel()
		}
		if err == nil {
			err = sameRunningCapture(ctx, c, running)
		}
		if err != nil {
			entry.Error = err.Error()
		}
		report.Cases = append(report.Cases, entry)
		if err != nil {
			return err
		}
		fmt.Println("PASS:", tc.name)
	}
	return nil
}

func validateQUICEvidence(e quicEvidence, target, name string, fragmented bool) error {
	local, err := netip.ParseAddrPort(e.Local)
	if err != nil || !local.Addr().Is4() || local.Addr().IsUnspecified() || local.Port() == 0 || e.Remote != target {
		return errors.New("HTTP/3 evidence does not name the exact IPv4 socket tuple")
	}
	if !e.Verified || e.ALPN != "h3" || e.Protocol != "HTTP/3.0" || e.StatusCode < 200 || e.StatusCode >= 400 || len(e.Nonce) != 32 || len(e.BodyPrefixSHA256) != 64 {
		return errors.New("missing verified HTTP/3 server response evidence")
	}
	if e.Sent == 0 || e.Received == 0 || e.Sent > quicSendLimit || e.Received > quicReceiveLimit || e.SentDatagrams < 1 || e.SentDatagrams > 128 || e.ReceivedDatagrams < 1 || e.ReceivedDatagrams > 256 || e.BodyBytes < 0 || e.BodyBytes > quicBodyLimit || e.Error != "" {
		return errors.New("HTTP/3 exchange exceeds its budget or lacks bidirectional traffic")
	}
	if e.FragmentedHello != fragmented || fragmented && e.FirstDatagramSNI != "" || !fragmented && e.FirstDatagramSNI != name {
		return errors.New("QUIC Initial does not establish the intended observable or fragmented SNI control")
	}
	return nil
}

func checkQUICRecord(rows []api.Connection, maxID uint64, path, rule, iface string, e quicEvidence) (api.Connection, bool, error) {
	var row api.Connection
	for _, candidate := range rows {
		if candidate.ID <= maxID || candidate.Process.PID != os.Getpid() || candidate.Protocol != api.UDP || candidate.Local != e.Local || candidate.Remote != e.Remote {
			continue
		}
		if row.ID != 0 {
			return candidate, false, errors.New("more than one engine UDP session matches one QUIC exchange")
		}
		row = candidate
	}
	if row.ID == 0 {
		return row, false, nil
	}
	if row.Process.Path != path || row.RuleID != rule || row.Interface != iface || row.Outcome != api.Routed || row.Domain != e.FirstDatagramSNI {
		return row, false, errors.New("QUIC process, rule, interface, outcome or observed name differs from the intended control")
	}
	local, err := netip.ParseAddrPort(row.OutboundLocal)
	if err != nil || !local.Addr().Is4() || local.Addr().IsUnspecified() || local.Port() == 0 {
		return row, false, errors.New("QUIC has no outbound pre-NAT socket tuple")
	}
	if row.Sent < e.Sent || row.Received < e.Received {
		return row, false, nil
	}
	// A peer may send datagrams after the client closes. The helper can count
	// these while they wait in the native receive queue; the client cannot.
	return row, true, nil
}
