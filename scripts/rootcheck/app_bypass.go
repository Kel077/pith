package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/netip"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/Kel077/pith/internal/api"
	"github.com/Kel077/pith/internal/rules"
)

type requestProof struct {
	PID        int         `json:"pid"`
	Executable string      `json:"executable"`
	Target     string      `json:"target"`
	TLS        tlsEvidence `json:"tls"`
	Problem    string      `json:"problem,omitempty"`
}

type proofRequest func(context.Context) (requestProof, func() error, error)

type appCase struct {
	Name          string
	Rules         []api.Rule
	Outcome       api.Outcome
	Interface     string
	RuleID        string
	Executable    string
	ExpectedIP    string
	RequireDomain bool
	Request       proofRequest
}

func appBypassPreflight(s *session) error {
	iface := s.Interfaces[0].Resolve(s.Original.Interfaces)
	if iface == nil || !iface.Up || iface.Kind == api.KindVPN || strings.HasPrefix(iface.Name, "utun") || strings.HasPrefix(iface.Name, "ppp") || strings.HasPrefix(iface.Name, "ipsec") {
		return errors.New("app-bypass requires interface A to be a reviewed, up physical interface")
	}
	for _, key := range []string{"", s.Interfaces[0].Name} {
		addr, err := netip.ParseAddr(s.baselineIP(key))
		if err != nil || !addr.IsGlobalUnicast() || addr.IsPrivate() {
			return errors.New("app-bypass is inconclusive without public-IP echoes from both system and interface-A preflights; prepare a suitable endpoint")
		}
	}
	return nil
}

func runAppBypass(ctx context.Context, s *session, c api.Service, cfg api.Config, since time.Time, system string, report *checkReport) error {
	self, err := executablePath()
	if err != nil {
		return err
	}
	child, cleanup, err := copyRequestTool(s.Dir, self)
	if err != nil {
		return err
	}
	defer cleanup()
	checksum, err := fileHash(child)
	if err != nil {
		return err
	}
	report.Notes = append(report.Notes, "Unmatched request executable: "+child+"; SHA-256: "+checksum)
	selfRequest := func(ctx context.Context) (requestProof, func() error, error) {
		proof := requestProof{PID: os.Getpid(), Executable: self, Target: s.Target}
		var err error
		proof.TLS, err = exchangeTLS(ctx, s, "", nil)
		if err != nil {
			proof.Problem = err.Error()
		}
		return proof, nil, nil
	}
	childRequest := func(ctx context.Context) (requestProof, func() error, error) {
		return requestFromChild(ctx, s, child, nil)
	}
	return runAppProofs(ctx, s, c, cfg, since, system, self, child, selfRequest, childRequest, report)
}

// runAppProofs separates routing assertions from process creation and network
// I/O. Tests inject request proofs and a Service without opening any socket.
func runAppProofs(ctx context.Context, s *session, c api.Service, cfg api.Config, since time.Time, system, self, child string, selfRequest, childRequest proofRequest, report *checkReport) error {
	if self == child || !filepath.IsAbs(self) || !filepath.IsAbs(child) || selfRequest == nil || childRequest == nil {
		return errors.New("app-bypass requires two distinct executable paths and controlled request sources")
	}
	if err := appBypassPreflight(s); err != nil {
		return err
	}
	address, err := netip.ParseAddrPort(s.Target)
	if err != nil {
		return err
	}
	app := api.AppRef{Path: self, Name: filepath.Base(self)}
	ip := api.Rule{ID: "app-ip", Enabled: true, Apps: []api.AppRef{app}, Targets: []string{address.Addr().String()}, Protocol: api.TCP, Action: api.Route, Via: []api.InterfaceRef{s.Interfaces[0]}}
	domain := ip
	domain.ID, domain.Targets = "app-domain", []string{s.ServerName}
	block := api.Rule{ID: "app-block", Enabled: true, Apps: []api.AppRef{app}, Targets: []string{s.ServerName}, Protocol: api.TCP, Action: api.Block}
	physicalIP, systemIP := s.baselineIP(s.Interfaces[0].Name), s.baselineIP("")
	if physicalIP == systemIP {
		note := "The system and physical-interface preflights have the same public-IP echo. App routing can be verified, but this evidence cannot establish a distinct exit-node bypass."
		report.Notes = append(report.Notes, note)
		fmt.Println("EVIDENCE LIMIT:", note)
	}
	cases := []appCase{
		{Name: "app and IP route through interface A", Rules: []api.Rule{ip}, Outcome: api.Routed, Interface: s.Interfaces[0].Name, RuleID: ip.ID, Executable: self, ExpectedIP: physicalIP, Request: selfRequest},
		{Name: "unmatched process keeps system routing under app IP rule", Rules: []api.Rule{ip}, Outcome: api.System, Interface: system, Executable: child, ExpectedIP: systemIP, Request: childRequest},
		{Name: "app and domain route through interface A", Rules: []api.Rule{domain}, Outcome: api.Routed, Interface: s.Interfaces[0].Name, RuleID: domain.ID, Executable: self, ExpectedIP: physicalIP, RequireDomain: true, Request: selfRequest},
		{Name: "unmatched process keeps system routing under app domain rule", Rules: []api.Rule{domain}, Outcome: api.System, Interface: system, Executable: child, ExpectedIP: systemIP, RequireDomain: true, Request: childRequest},
		{Name: "app block wins over an earlier app route", Rules: []api.Rule{domain, block}, Outcome: api.Blocked, RuleID: block.ID, Executable: self, RequireDomain: true, Request: selfRequest},
	}
	for _, test := range cases {
		entry, err := runAppCase(ctx, s, c, cfg, since, test)
		report.Cases = append(report.Cases, entry)
		if err != nil {
			return fmt.Errorf("%s: %w", test.Name, err)
		}
		fmt.Println("PASS:", test.Name)
	}
	return nil
}

func runAppCase(ctx context.Context, s *session, c api.Service, cfg api.Config, since time.Time, test appCase) (entry checkCase, resultErr error) {
	entry.Name, entry.ExpectedIP = test.Name, test.ExpectedIP
	defer func() {
		if resultErr != nil {
			entry.Error = resultErr.Error()
		}
	}()
	cfg.Scenes[0].Rules = test.Rules
	if err := c.SaveConfig(ctx, cfg); err != nil {
		return entry, err
	}
	previous, err := c.Connections(ctx, api.ConnectionQuery{Limit: 500})
	if err != nil {
		return entry, err
	}
	var maxID uint64
	for _, conn := range previous {
		if conn.ID > maxID {
			maxID = conn.ID
		}
	}
	proof, finish, err := test.Request(ctx)
	if finish != nil {
		// Keep the unmatched process alive through attribution verification, then
		// close its input and reap it on every success and failure path.
		defer func() { resultErr = errors.Join(resultErr, finish()) }()
	}
	if err != nil {
		return entry, err
	}
	entry.TLS = proof.TLS
	entry.RequestPID, entry.RequestPath, entry.RequestTarget = proof.PID, proof.Executable, proof.Target
	if proof.PID <= 0 || proof.Executable != test.Executable || proof.Target != s.Target {
		return entry, errors.New("request proof does not identify the expected process path and frozen target")
	}
	expectSuccess := test.Outcome != api.Blocked && test.Outcome != api.Unreachable
	if (proof.Problem == "") != expectSuccess {
		return entry, fmt.Errorf("request result does not match expected outcome %s: %s", test.Outcome, proof.Problem)
	}
	pollCtx, cancel := context.WithTimeout(ctx, 4*time.Second)
	defer cancel()
	if err := waitUntil(pollCtx, func() (bool, error) {
		connections, err := c.Connections(pollCtx, api.ConnectionQuery{Limit: 500})
		if err != nil {
			return false, err
		}
		for _, conn := range connections {
			if conn.ID <= maxID || conn.Process.PID != proof.PID || conn.Remote != s.Target || conn.Protocol != api.TCP || conn.Ended.IsZero() {
				continue
			}
			entry.Connection = conn
			if conn.Process.Path != test.Executable || conn.App.Path != test.Executable {
				return false, errors.New("connection attribution does not match the controlled executable path")
			}
			if conn.Outcome != test.Outcome || conn.RuleID != test.RuleID || conn.Interface != test.Interface {
				return false, fmt.Errorf("connection differs: outcome=%s rule=%s interface=%s", conn.Outcome, conn.RuleID, conn.Interface)
			}
			if test.RequireDomain && conn.Domain != s.ServerName {
				return false, errors.New("connection did not record the expected TLS domain")
			}
			if expectSuccess && (conn.Sent == 0 || conn.Received == 0) {
				return false, errors.New("successful connection has no bidirectional byte counts")
			}
			return true, nil
		}
		return false, nil
	}); err != nil {
		return entry, fmt.Errorf("new captured connection evidence: %w", err)
	}
	if expectSuccess && (test.ExpectedIP == "" || proof.TLS.PublicIP != test.ExpectedIP) {
		return entry, errors.New("public-IP echo differs from the expected physical or system preflight")
	}
	current, err := c.Status(ctx)
	if err != nil || current.State != api.Running || !current.Since.Equal(since) {
		return entry, fmt.Errorf("app-rule reload restarted or stopped capture: state=%s error=%v", current.State, err)
	}
	return entry, nil
}

func executablePath() (string, error) {
	path, err := os.Executable()
	if err != nil {
		return "", err
	}
	path, err = filepath.EvalSymlinks(path)
	if err != nil {
		return "", err
	}
	if !filepath.IsAbs(path) || rules.OuterBundle(path) != "" {
		return "", errors.New("run the standalone rootcheck tool from TMPDIR, outside any application bundle")
	}
	return path, nil
}

func copyRequestTool(sessionDir, source string) (string, func(), error) {
	if err := privateTempDirectory(sessionDir); err != nil {
		return "", nil, err
	}
	in, err := os.Open(source)
	if err != nil {
		return "", nil, err
	}
	defer in.Close()
	info, err := in.Stat()
	const maximum = 256 << 20
	if err != nil || !info.Mode().IsRegular() || info.Mode().Perm()&0111 == 0 || info.Size() > maximum {
		return "", nil, errors.New("request source must be a regular executable no larger than 256 MiB")
	}
	dir, err := os.MkdirTemp(sessionDir, "request-tool-")
	if err != nil {
		return "", nil, err
	}
	cleanup := func() { _ = os.RemoveAll(dir) }
	path := filepath.Join(dir, "rootcheck-unmatched")
	out, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0700)
	if err != nil {
		cleanup()
		return "", nil, err
	}
	n, copyErr := io.Copy(out, io.LimitReader(in, maximum+1))
	err = errors.Join(copyErr, out.Close())
	if err != nil || n > maximum {
		cleanup()
		return "", nil, fmt.Errorf("copy request executable: bytes=%d error=%v", n, err)
	}
	originalHash, err := fileHash(source)
	if err != nil {
		cleanup()
		return "", nil, err
	}
	copyHash, err := fileHash(path)
	if err != nil || originalHash != copyHash {
		cleanup()
		return "", nil, errors.New("request executable changed while being copied")
	}
	return path, cleanup, nil
}
