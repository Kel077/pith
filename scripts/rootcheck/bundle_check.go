package main

import (
	"context"
	"errors"
	"fmt"
	"net/netip"
	"os"
	"os/exec"
	"path/filepath"

	"github.com/Kel077/pith/internal/api"
)

func runBundleChecks(ctx context.Context, s *session, c api.Service, cfg api.Config, running api.Status, system string, report *checkReport, batch bool) (resultErr error) {
	fixture, cleanup, err := createBundleFixture(s)
	if err != nil {
		return err
	}
	defer func() { resultErr = errors.Join(resultErr, cleanup()) }()
	report.Notes = append(report.Notes, "Temporary bundle root: "+fixture.Directory+"; executable SHA-256: "+s.Bundle.SHA256+"; removes after every ordinary return.", "This verifies executable containment in the outermost bundle, not XPC service launch or delegated sockets. Background URLSession is skipped this round.")
	request := func(path string) proofRequest {
		return func(ctx context.Context) (requestProof, func() error, error) {
			if _, err := verifyBundleFixture(s, fixture.Directory); err != nil {
				return requestProof{}, nil, err
			}
			return requestFromChild(ctx, s, path, func(ctx context.Context, executable, dir string) *exec.Cmd {
				cmd := exec.CommandContext(ctx, executable, "_request", "--session", dir, "--fixture", fixture.Directory)
				cmd.Env = []string{"PATH=/usr/bin:/bin:/usr/sbin:/sbin", "LC_ALL=C", "TMPDIR=" + os.TempDir()}
				return cmd
			})
		}
	}
	return runBundleProofs(ctx, s, c, cfg, running, system, report, batch, fixture, request)
}

func runBundleProofs(ctx context.Context, s *session, c api.Service, cfg api.Config, running api.Status, system string, report *checkReport, batch bool, f bundleFixture, request func(string) proofRequest) error {
	endpoint, err := netip.ParseAddrPort(s.Target)
	if err != nil {
		return err
	}
	// Path-only and identifier-only matches are exercised independently. The
	// synthetic relocated path cannot contain any of the fixture executables.
	ip := api.Rule{ID: "bundle-ip", Enabled: true, Apps: []api.AppRef{{Path: f.App.Path}}, Targets: []string{endpoint.Addr().String()}, Protocol: api.TCP, Action: api.Route, Via: []api.InterfaceRef{s.Interfaces[0]}}
	domain := ip
	domain.ID, domain.Apps, domain.Targets = "bundle-domain", []api.AppRef{{Path: filepath.Join(f.Directory, "Relocated.app"), BundleID: bundleIdentifier}}, []string{s.ServerName}
	block := domain
	block.ID, block.Action, block.Via = "bundle-block", api.Block, nil
	control := api.AppRef{Path: f.Control}
	makeCase := func(name, executable string, app *api.AppRef, rules []api.Rule, outcome api.Outcome, ruleID, iface string) appCase {
		key := iface
		if outcome == api.System {
			key = ""
		}
		return appCase{Name: name, Executable: executable, ExpectedApp: app, Rules: rules, Outcome: outcome, RuleID: ruleID, Interface: iface, ExpectedIP: s.baselineIP(key), RequireDomain: true, Request: request(executable)}
	}
	cases := []appCase{makeCase("bundle main IP smoke through A", f.Main, &f.App, []api.Rule{ip}, api.Routed, ip.ID, s.Interfaces[0].Name)}
	if batch {
		cases = []appCase{
			makeCase("nested helper inherits parent IP rule", f.Helper, &f.App, []api.Rule{ip}, api.Routed, ip.ID, s.Interfaces[0].Name),
			makeCase("outside executable keeps system routing under bundle IP rule", f.Control, &control, []api.Rule{ip}, api.System, "", system),
			makeCase("bundle main matches identifier and TLS domain", f.Main, &f.App, []api.Rule{domain}, api.Routed, domain.ID, s.Interfaces[0].Name),
			makeCase("nested helper uses outermost identifier and TLS domain", f.Helper, &f.App, []api.Rule{domain}, api.Routed, domain.ID, s.Interfaces[0].Name),
			makeCase("parent bundle block wins for nested helper", f.Helper, &f.App, []api.Rule{domain, block}, api.Blocked, block.ID, ""),
			makeCase("outside executable stays unmatched under parent bundle block", f.Control, &control, []api.Rule{domain, block}, api.System, "", system),
		}
	}
	for _, test := range cases {
		entry, err := runAppCase(ctx, s, c, cfg, running.Since, test)
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
		fmt.Println("PASS:", test.Name)
	}
	return nil
}
