package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/netip"
	"os/exec"
	"path/filepath"
	"time"

	"github.com/Kel077/pith/internal/api"
	"github.com/Kel077/pith/internal/engine/attribution"
)

type webkitPCB struct {
	At                  time.Time          `json:"at"`
	Socket              attribution.Socket `json:"socket"`
	Owner               api.Process        `json:"owner"`
	App                 api.AppRef         `json:"effectiveApp"`
	EffectivePID        int                `json:"effectivePID"`
	EffectiveExecutable string             `json:"effectiveExecutable"`
	Delegated           bool               `json:"delegated"`
}

type webkitEvidence struct {
	URL              string          `json:"manualURL"`
	Ready            time.Time       `json:"ready"`
	OpenRequested    bool            `json:"openRequested"`
	OpenAttempts     int             `json:"openAttempts"`
	OpenError        string          `json:"openError,omitempty"`
	Result           string          `json:"result"`
	Reason           string          `json:"reason,omitempty"`
	PageBodyVerified bool            `json:"pageBodyVerified"`
	Connection       *api.Connection `json:"connection,omitempty"`
	PCB              *webkitPCB      `json:"pcb,omitempty"`
}

type webkitCheckDeps struct {
	lookup func(context.Context, api.Protocol, netip.AddrPort, netip.AddrPort) (webkitPCB, error)
	ready  func(string) error
	open   func(context.Context, string, string) error
}

func liveWebKitChecks() webkitCheckDeps {
	resolver := attribution.New()
	return webkitCheckDeps{
		ready: func(url string) error {
			_, err := fmt.Println("READY: one Safari navigation to", url, "\nOnly delegated socket ownership and exit routing are checked; page content is not automatically verified.")
			return err
		},
		open: openWebKitOnce,
		lookup: func(ctx context.Context, protocol api.Protocol, local, remote netip.AddrPort) (webkitPCB, error) {
			var proof webkitPCB
			table, err := attribution.Snapshot(protocol)
			if err != nil {
				return proof, err
			}
			proof.Socket, err = attribution.Find(table, local, remote)
			if err != nil {
				return proof, err
			}
			owner, err := resolver.Lookup(ctx, protocol, local, remote)
			if err != nil {
				return proof, err
			}
			proof.Owner, proof.App = owner.Process, owner.App
			proof.EffectivePID, proof.Delegated = owner.EffectivePID, owner.Delegated
			proof.EffectiveExecutable, err = attribution.ProcessPath(owner.EffectivePID)
			if err != nil {
				return proof, err
			}
			// A second fresh resolver verifies process identity around the path read;
			// cached helper events are not accepted as independent kernel evidence.
			after, err := attribution.New().Lookup(ctx, protocol, local, remote)
			if err != nil || after != owner {
				return proof, errors.New("PCB ownership changed while sampling")
			}
			proof.At = time.Now()
			return proof, nil
		},
	}
}

func webkitOpenCommand(ctx context.Context, app, url string) *exec.Cmd {
	// No shell, automation API, -W, application arguments or window manipulation.
	// The coordinator explicitly opts in to this single LaunchServices request.
	cmd := exec.CommandContext(ctx, "/usr/bin/open", "-a", app, url)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = nil, io.Discard, io.Discard
	cmd.WaitDelay = time.Second
	return cmd
}

func openWebKitOnce(ctx context.Context, app, url string) error {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	// Run waits for our short-lived open process, never for Safari. Cancellation
	// can terminate only that child; Safari and all of its services are untouched.
	return webkitOpenCommand(ctx, app, url).Run()
}

func canonicalSame(a, b string) bool {
	if !filepath.IsAbs(a) || !filepath.IsAbs(b) {
		return false
	}
	x, err := filepath.EvalSymlinks(a)
	if err != nil {
		return false
	}
	y, err := filepath.EvalSymlinks(b)
	return err == nil && x == y
}

// The basename selects the networking-process case, never app policy. Positive
// proof also requires an independently observed delegated PID and exact app.
func webkitDelegation(plan *webkitPlan, conn api.Connection, p webkitPCB) error {
	local, err := netip.ParseAddrPort(conn.Local)
	if err != nil || local.Port() == 0 || local.Addr().IsUnspecified() {
		return errors.New("application tuple unavailable")
	}
	remote, err := netip.ParseAddrPort(conn.Remote)
	if err != nil || p.Socket.Local != local || p.Socket.Remote != remote {
		return errors.New("no exact connected PCB tuple")
	}
	if !p.Delegated || p.EffectivePID <= 0 || p.EffectivePID == p.Owner.PID || p.Socket.PID != p.Owner.PID || p.Socket.EffectivePID != p.EffectivePID {
		return errors.New("independent delegated owner is incomplete")
	}
	if filepath.Base(p.Owner.Path) != "com.apple.WebKit.Networking" || p.Owner.PID <= 0 {
		return errors.New("socket is not owned by the WebKit networking process")
	}
	if p.App.BundleID != plan.App.BundleID || !canonicalSame(p.App.Path, plan.App.Path) || !canonicalSame(p.EffectiveExecutable, plan.Executable) {
		return errors.New("effective process is not the frozen Safari executable and bundle")
	}
	return nil
}

func webkitConnection(plan *webkitPlan, conn api.Connection, p webkitPCB, iface string) error {
	if conn.Process.PID != p.Owner.PID || !canonicalSame(conn.Process.Path, p.Owner.Path) || conn.App.BundleID != plan.App.BundleID || !canonicalSame(conn.App.Path, plan.App.Path) {
		return errors.New("helper app/socket owner differs from independent delegation evidence")
	}
	if conn.RuleID != "webkit-app" || conn.Outcome != api.Routed || conn.Interface != iface {
		return errors.New("delegated connection used the wrong rule, outcome or interface")
	}
	outbound, err := netip.ParseAddrPort(conn.OutboundLocal)
	if err != nil || outbound.Port() == 0 || outbound.Addr().IsUnspecified() {
		return errors.New("delegated connection has no outbound local tuple")
	}
	return nil
}

func runWebKitCheck(parent context.Context, s *session, c api.Service, cfg api.Config, running api.Status, report *checkReport, duration time.Duration, launch bool, deps webkitCheckDeps) (resultErr error) {
	if duration <= 0 || duration > time.Minute {
		return errors.New("webkit observation must be in (0,60s]")
	}
	if err := s.reviewedWebKitPlan(); err != nil {
		return err
	}
	endpoint, _ := netip.ParseAddrPort(s.Target)
	cfg.Scenes[0].Rules = []api.Rule{{ID: "webkit-app", Enabled: true, Apps: []api.AppRef{s.WebKit.App}, Targets: []string{endpoint.Addr().String()}, Protocol: api.AnyProtocol, Action: api.Route, Via: []api.InterfaceRef{s.Interfaces[0]}}}
	if err := c.SaveConfig(parent, cfg); err != nil {
		return err
	}
	maxID, err := maxConnectionID(parent, c)
	if err != nil {
		return err
	}
	evidence := &webkitEvidence{URL: s.webkitURL(), OpenRequested: launch, Result: "incomplete", Reason: "no fresh delegated socket observed"}
	report.WebKit = evidence
	report.Notes = append(report.Notes, "Automatic WebKit evidence proves delegated socket ownership and exit routing only. Page body, HTTP response and URL nonce are not automatically verified; keep any manual page evidence separately. Background URLSession is skipped.")
	defer func() {
		if err := s.reviewedWebKitPlan(); err != nil {
			resultErr = errors.Join(resultErr, err)
			evidence.Result = "failed"
		}
		if resultErr != nil {
			evidence.Reason = resultErr.Error()
		}
	}()
	ctx, cancel := context.WithTimeout(parent, duration)
	defer cancel()
	evidence.Ready = time.Now()
	if err := deps.ready(evidence.URL); err != nil {
		return err
	}
	if launch {
		evidence.OpenAttempts = 1
		if err := deps.open(ctx, s.WebKit.App.Path, evidence.URL); err != nil {
			evidence.OpenError = err.Error()
			return fmt.Errorf("incomplete: the single LaunchServices open failed: %w", err)
		}
	}
	proofs := make(map[uint64]webkitPCB)
	err = waitUntil(ctx, func() (bool, error) {
		if err := sameRunningCapture(ctx, c, running); err != nil {
			evidence.Result = "failed"
			return false, err
		}
		rows, err := c.Connections(ctx, api.ConnectionQuery{Since: evidence.Ready, Limit: 500})
		if err != nil {
			return false, err
		}
		for _, conn := range rows {
			if conn.ID <= maxID || conn.Started.Before(evidence.Ready) || conn.Remote != s.Target || (conn.Protocol != api.TCP && conn.Protocol != api.UDP) {
				continue
			}
			proof, ok := proofs[conn.ID]
			if !ok {
				local, err := netip.ParseAddrPort(conn.Local)
				if err != nil {
					evidence.Reason = "fresh flow lacks its application tuple"
					continue
				}
				proof, err = deps.lookup(ctx, conn.Protocol, local, endpoint)
				if err != nil {
					evidence.Reason = "independent PCB sample unavailable: " + err.Error()
					continue
				}
				if proof.At.Before(evidence.Ready) {
					evidence.Reason = "PCB sample predates READY"
					continue
				}
				if err := webkitDelegation(s.WebKit, conn, proof); err != nil {
					evidence.Reason = err.Error()
					continue
				}
				if len(proofs) < 500 {
					proofs[conn.ID] = proof
				}
			}
			// Recheck the exact tuple even when bytes arrive after its PCB expires.
			if err := webkitDelegation(s.WebKit, conn, proof); err != nil {
				evidence.Result = "failed"
				return false, err
			}
			evidence.Connection, evidence.PCB = &conn, &proof
			if err := webkitConnection(s.WebKit, conn, proof, s.Interfaces[0].Name); err != nil {
				evidence.Result = "failed"
				return false, err
			}
			if conn.Sent == 0 || conn.Received == 0 {
				evidence.Reason = "delegated connection has not reported bidirectional bytes"
				continue
			}
			evidence.Result, evidence.Reason = "passed", "delegated socket ownership and exit routing verified; page body not automatically verified"
			return true, nil
		}
		return false, nil
	})
	if err != nil && evidence.Result == "incomplete" {
		return fmt.Errorf("incomplete: %s: %w", evidence.Reason, err)
	}
	if err == nil {
		fmt.Println("PASS: delegated WebKit socket and exit routing; page body was not automatically verified.")
	}
	return err
}
