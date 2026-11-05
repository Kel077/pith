package main

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"time"

	"github.com/Kel077/pith/internal/api"
)

func (s *session) validateQUIC() error {
	if s.QUICTarget == "" && s.QUICServerName == "" {
		return nil
	}
	target, err := testAddress(s.QUICTarget)
	if err != nil || target.Port() != 443 || target.String() != s.QUICTarget || s.TestProfile != nil {
		return errors.New("QUIC acceptance requires a canonical public IPv4:443 in host-ipv4 mode")
	}
	name, err := api.ParseTarget(s.QUICServerName)
	if err != nil || !name.IsDomain() || name.Subdomains || name.String() != s.QUICServerName {
		return errors.New("quic-server-name must be a plain lowercase ASCII TLS certificate name")
	}
	return nil
}

func (s *session) quicPlanBytes() ([]byte, error) {
	if err := s.validateQUIC(); err != nil {
		return nil, err
	}
	if s.QUICTarget == "" {
		return nil, errors.New("prepare a new session with quic-target and quic-server-name")
	}
	plan := struct {
		Target        string        `json:"target"`
		ServerName    string        `json:"serverName"`
		Method        string        `json:"method"`
		Path          string        `json:"path"`
		Protocol      string        `json:"protocol"`
		SmokeRequests int           `json:"smokeRequests"`
		BatchRequests int           `json:"batchRequests"`
		Timeout       time.Duration `json:"timeoutPerRequest"`
		SentLimit     int           `json:"sentPayloadLimitPerRequest"`
		ReceiveLimit  int           `json:"readPayloadLimitPerRequest"`
		BodyLimit     int           `json:"bodyPrefixLimit"`
		Notes         []string      `json:"notes"`
	}{s.QUICTarget, s.QUICServerName, "GET", "/", "QUIC v1 / HTTP/3", 1, 3, quicRequestTimeout, quicSendLimit, quicReceiveLimit, quicBodyLimit, []string{
		"One fresh connected UDP socket and one HTTP/3 request per case; no HTTP retry, redirect, name resolution, proxy or TCP fallback.",
		"TLS certificate and ALPN verification remain enabled, with no 0-RTT or saved resumption state.",
		"Transport retransmissions count toward the send/read-payload budgets, bounded by 128 writes and 256 reads; kernel queues and network headers are not metered.",
		"Smoke: IP rule through A. Batch: observable SNI domain rule through A, IP rule through B, then IP rule through A with a fragmented Initial ClientHello.",
		"A fragmented ClientHello control is not ECH acceptance; the recorded first datagram must actually lack a recoverable SNI.",
		"Only a bounded response prefix hash is stored, never application plaintext, TLS secrets or a packet capture.",
	}}
	data, err := json.MarshalIndent(plan, "", "  ")
	return append(data, '\n'), err
}

func (s *session) writeQUICPlan() error {
	if s.QUICTarget == "" {
		return nil
	}
	data, err := s.quicPlanBytes()
	if err != nil {
		return err
	}
	if err := writePrivate(filepath.Join(s.Dir, "quic-plan.json"), data); err != nil {
		return err
	}
	s.QUICPlanSHA256 = hashBytes(data)
	return s.save()
}

func (s *session) reviewedQUICPlan() error {
	data, err := s.quicPlanBytes()
	if err != nil {
		return err
	}
	if s.QUICPlanSHA256 == "" || hashBytes(data) != s.QUICPlanSHA256 {
		return errors.New("QUIC endpoint or limits changed after preparation")
	}
	path := filepath.Join(s.Dir, "quic-plan.json")
	info, err := os.Lstat(path)
	if err != nil || !info.Mode().IsRegular() || info.Mode().Perm() != 0600 {
		return errors.New("QUIC review plan is missing or is not a private regular file")
	}
	actual, err := os.ReadFile(path)
	if err != nil || hashBytes(actual) != s.QUICPlanSHA256 {
		return errors.New("QUIC review plan changed after preparation")
	}
	return nil
}
