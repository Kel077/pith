package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/Kel077/pith/internal/api"
)

func TestVPNRebuiltDNSSettlesWithinOriginalWindow(t *testing.T) {
	s := vpnFixture(t)
	c := newVPNService(t, s)
	c.set("rebuilt")
	deps := liveVPNChecks(c.snapshot)
	deps.dnsRetryDelay = 5 * time.Millisecond
	attempts := 0
	deps.dns = func(ctx context.Context, p *vpnRestartPlan) (dnsEvidence, error) {
		attempts++
		e, err := c.dns(ctx, p)
		e.Local = fmt.Sprintf("192.0.2.1:%d", 51000+attempts)
		if attempts < 3 {
			e.ResponseBytes, e.Answers = 0, nil
			e.Error = context.DeadlineExceeded.Error()
			return e, context.DeadlineExceeded
		}
		return e, err
	}
	evidence := &vpnRestartEvidence{}
	running, _ := c.Status(context.Background())
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	started := time.Now()
	if err := settleVPNDNS(ctx, s, c, running, evidence, deps); err != nil {
		t.Fatal(err)
	}
	if attempts != 3 || len(evidence.Queries) != 3 || time.Since(started) < 2*deps.dnsRetryDelay || evidence.RestoredAt.IsZero() || evidence.DNSReadyAt.Before(evidence.RestoredAt) {
		t.Fatal(attempts, evidence)
	}
	for i, query := range evidence.Queries {
		if query.Phase != fmt.Sprintf("rebuilt-attempt-%d", i+1) || !query.CheckNoForwarding || i < 2 && query.Exchange.Error == "" {
			t.Fatal(query)
		}
	}
	// A late log entry for an earlier timed-out attempt still invalidates the
	// stage even though the final DNS attempt answered correctly.
	evidence.ControlsPassed = true
	first := evidence.Queries[0].Exchange
	if checkVPNFinalRows(evidence, []api.Connection{{ID: 1, Remote: first.Remote, Local: first.Local, Process: api.Process{PID: os.Getpid()}}}) == nil || evidence.ControlsPassed {
		t.Fatal("successful retry hid forwarding of an earlier attempt")
	}
	plan, err := s.vpnPlanBytes()
	if err != nil || !strings.Contains(string(plan), "seven in check") || !strings.Contains(string(plan), "three attempts separated by one second") {
		t.Fatal("frozen request budget missing", err)
	}
}

func TestVPNRebuiltDNSDoesNotRetrySafetyFailures(t *testing.T) {
	for _, kind := range []string{"forwarded-timeout", "wrong-own-address", "capture-stopped", "parent-canceled", "exhausted"} {
		t.Run(kind, func(t *testing.T) {
			s := vpnFixture(t)
			c := newVPNService(t, s)
			c.set("rebuilt")
			deps := liveVPNChecks(c.snapshot)
			deps.dnsRetryDelay = time.Millisecond
			ctx, cancel := context.WithTimeout(context.Background(), time.Second)
			defer cancel()
			attempts := 0
			deps.dns = func(context.Context, *vpnRestartPlan) (dnsEvidence, error) {
				attempts++
				e := vpnAnswer(s)
				if kind == "wrong-own-address" {
					e.Answers[0].Value = "203.0.113.3"
					return e, nil
				}
				if kind == "forwarded-timeout" {
					c.mu.Lock()
					c.rows = append(c.rows, api.Connection{ID: 1, Remote: e.Remote, Local: e.Local, Process: api.Process{PID: os.Getpid()}})
					c.mu.Unlock()
				}
				if kind == "capture-stopped" {
					c.mu.Lock()
					c.state.State = api.Stopped
					c.mu.Unlock()
				}
				if kind == "parent-canceled" {
					cancel()
				}
				e.ResponseBytes, e.Answers = 0, nil
				e.Error = context.DeadlineExceeded.Error()
				return e, context.DeadlineExceeded
			}
			running, _ := c.Status(ctx)
			evidence := &vpnRestartEvidence{}
			err := settleVPNDNS(ctx, s, c, running, evidence, deps)
			want := 1
			if kind == "exhausted" {
				want = 3
				if !errors.Is(err, errVPNInconclusive) {
					t.Fatal("exhausted settling must be incomplete", err)
				}
			}
			if err == nil || attempts != want || len(evidence.Queries) != want || !evidence.DNSReadyAt.IsZero() {
				t.Fatal(kind, err, attempts, evidence)
			}
		})
	}
}

func TestVPNRouteRecoveryAndDNSShareRebuildDeadline(t *testing.T) {
	s := vpnFixture(t)
	c := newVPNService(t, s)
	c.set("down")
	started := time.Now()
	deps := liveVPNChecks(func(ctx context.Context) (snapshot, error) {
		if time.Since(started) >= 30*time.Millisecond {
			c.set("rebuilt")
		}
		return c.snapshot(ctx)
	})
	deps.poll = time.Millisecond
	deps.dnsRetryDelay = time.Millisecond
	attempts := 0
	ctx, cancel := context.WithTimeout(context.Background(), 80*time.Millisecond)
	defer cancel()
	deadline, _ := ctx.Deadline()
	deps.dns = func(query context.Context, _ *vpnRestartPlan) (dnsEvidence, error) {
		attempts++
		end, ok := query.Deadline()
		if !ok || !end.Equal(deadline) {
			t.Error("DNS got a new window after route appearance", end, deadline)
		}
		<-query.Done()
		e := vpnAnswer(s)
		e.ResponseBytes, e.Answers = 0, nil
		return e, query.Err()
	}
	running, _ := c.Status(ctx)
	evidence := &vpnRestartEvidence{}
	err := settleVPNDNS(ctx, s, c, running, evidence, deps)
	if !errors.Is(err, errVPNInconclusive) || attempts != 1 || !evidence.DNSReadyAt.IsZero() || time.Since(started) > time.Second {
		t.Fatal(err, attempts, evidence)
	}
}
