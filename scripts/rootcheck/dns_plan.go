package main

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"time"

	"github.com/Kel077/pith/internal/api"
)

type dnsPlan struct {
	Targets           []string      `json:"targets"`
	Name              string        `json:"name"`
	Types             []string      `json:"types"`
	SmokeQueries      int           `json:"smokeQueries"`
	BatchQueries      int           `json:"batchQueries"`
	MaxQueriesPerCase int           `json:"maxQueriesPerCase"`
	ResponseTimeout   time.Duration `json:"responseTimeout"`
	ResponseLimit     int           `json:"responseLimit"`
	IdleWaitLimit     time.Duration `json:"idleWaitLimit"`
	Notes             []string      `json:"notes"`
}

func (s *session) dnsTargets() []string {
	if s.DNSTarget == "" {
		return nil
	}
	return append([]string{s.DNSTarget}, s.ExtraDNSTargets...)
}

func (s *session) validateDNS() error {
	targets := s.dnsTargets()
	if len(targets) > 2 || s.DNSTarget == "" && (len(s.ExtraDNSTargets) != 0 || s.DNSName != "") {
		return errors.New("DNS acceptance requires one or two explicitly reviewed resolver endpoints")
	}
	if s.TestProfile != nil && len(targets) != 0 {
		return errors.New("additional DNS targets are accepted only in host-ipv4 mode")
	}
	for i, raw := range targets {
		endpoint, err := testAddress(raw)
		if err != nil || endpoint.Port() != 53 || endpoint.String() != raw || slices.Contains(targets[:i], raw) {
			return errors.New("DNS targets must be distinct canonical public IPv4:53 endpoints")
		}
	}
	if s.DNSName != "" {
		name, err := api.ParseTarget(s.DNSName)
		if err != nil || !name.IsDomain() || name.Subdomains || name.String() != s.DNSName {
			return errors.New("dns-name must be a plain lowercase ASCII DNS question name")
		}
	}
	return nil
}

func (s *session) dnsPlanBytes() ([]byte, error) {
	if err := s.validateDNS(); err != nil {
		return nil, err
	}
	if s.DNSName == "" {
		return nil, errors.New("prepare a new session with dns-name before automatic DNS checks")
	}
	plan := dnsPlan{
		Targets: s.dnsTargets(), Name: s.DNSName, Types: []string{"A", "AAAA"},
		SmokeQueries: 1, BatchQueries: 9*len(s.dnsTargets()) + 2, MaxQueriesPerCase: 3,
		ResponseTimeout: dnsResponseTimeout, ResponseLimit: 512, IdleWaitLimit: 14 * time.Second,
		Notes: []string{
			"One query per exchange, no retransmission, EDNS, TCP fallback or automatic resolver selection.",
			"The separate smoke must pass before the batch; every stage stops capture and verifies cleanup.",
			"Source tuples are application-side and outbound pre-NAT only; public NAT ports are not observed.",
			"Answer TTLs are recorded without dialing answer addresses or expanding frozen capture.",
			"No general UDP echo, unconnected listener, pre-existing UDP or loss-driven failover claim.",
		},
	}
	data, err := json.MarshalIndent(plan, "", "  ")
	return append(data, '\n'), err
}

func (s *session) writeDNSPlan() error {
	if s.DNSName == "" {
		return nil
	}
	data, err := s.dnsPlanBytes()
	if err != nil {
		return err
	}
	if err := writePrivate(filepath.Join(s.Dir, "dns-plan.json"), data); err != nil {
		return err
	}
	s.DNSPlanSHA256 = hashBytes(data)
	return s.save()
}

func (s *session) reviewedDNSPlan() error {
	data, err := s.dnsPlanBytes()
	if err != nil {
		return err
	}
	if s.DNSPlanSHA256 == "" || hashBytes(data) != s.DNSPlanSHA256 {
		return errors.New("DNS endpoints, question or limits changed after preparation; prepare and review a new session")
	}
	path := filepath.Join(s.Dir, "dns-plan.json")
	info, err := os.Lstat(path)
	if err != nil || !info.Mode().IsRegular() || info.Mode().Perm() != 0600 {
		return errors.New("DNS review plan is missing or is not a private regular file")
	}
	actual, err := os.ReadFile(path)
	if err != nil || hashBytes(actual) != s.DNSPlanSHA256 {
		return errors.New("DNS review plan changed after preparation")
	}
	return nil
}
