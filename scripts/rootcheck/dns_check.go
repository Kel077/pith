package main

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"os"
	"time"

	"github.com/Kel077/pith/internal/api"
	"github.com/Kel077/pith/internal/engine/capture"
	"golang.org/x/net/dns/dnsmessage"
)

type dnsCheckDeps struct {
	dial func(context.Context, string) (net.Conn, error)
	idle func(context.Context, api.Service, api.Status, api.Connection, time.Time) (api.Connection, error)
}

func liveDNSChecks() dnsCheckDeps {
	return dnsCheckDeps{
		dial: func(ctx context.Context, target string) (net.Conn, error) {
			// An ordinary app socket must cross capture; binding it here would
			// bypass the engine and invalidate the acceptance evidence.
			return (&net.Dialer{Timeout: dnsResponseTimeout}).DialContext(ctx, "udp4", target)
		},
		idle: waitDNSIdle,
	}
}

type dnsFlow struct {
	conn           net.Conn
	afterID        uint64
	log            api.Connection
	sent, received uint64
	lastReply      time.Time
}

func maxConnectionID(ctx context.Context, c api.Service) (uint64, error) {
	rows, err := c.Connections(ctx, api.ConnectionQuery{Limit: 500})
	var id uint64
	for _, row := range rows {
		id = max(id, row.ID)
	}
	return id, err
}

func runDNSChecks(ctx context.Context, s *session, c api.Service, cfg api.Config, running api.Status, report *checkReport, batch bool, deps dnsCheckDeps) error {
	sequence, err := newDNSSequence()
	if err != nil {
		return err
	}
	path, err := executablePath()
	if err != nil {
		return err
	}
	report.Notes = append(report.Notes,
		"DNS evidence uses unique query IDs, exact application tuples and pre-NAT outbound tuples; public NAT ports are unknown.",
		"Answer addresses and TTLs are recorded only. Cache expiry, arbitrary UDP services and existing UDP sockets are not hardware-tested by this stage.")
	queries := 0
	limit := 1
	if batch {
		limit = 9*len(s.dnsTargets()) + 2
	}
	save := func(rules []api.Rule) error {
		cfg.Scenes[0].Rules = rules
		return c.SaveConfig(ctx, cfg)
	}
	open := func(target string) (*dnsFlow, error) {
		id, err := maxConnectionID(ctx, c)
		if err != nil {
			return nil, err
		}
		conn, err := deps.dial(ctx, target)
		if err != nil {
			return nil, err
		}
		return &dnsFlow{conn: conn, afterID: id}, nil
	}
	// The caller keeps a socket open only for reuse/idle assertions; every other
	// request gets a fresh source port. All sockets close on every error path.
	run := func(name string, flow *dnsFlow, target string, kinds []dnsmessage.Type, outcome api.Outcome, iface, rule string) (resultErr error) {
		if flow == nil {
			var err error
			flow, err = open(target)
			if err != nil {
				return err
			}
			defer flow.conn.Close()
		}
		entry := checkCase{Name: name, RequestPID: os.Getpid(), RequestPath: path, RequestTarget: target}
		defer func() {
			entry.Connection = flow.log
			if resultErr != nil {
				entry.Error = resultErr.Error()
			}
			report.Cases = append(report.Cases, entry)
			if resultErr == nil {
				fmt.Println("PASS:", name)
			}
		}()
		if len(kinds) < 1 || len(kinds) > 3 {
			return errors.New("DNS case exceeds its query budget")
		}
		positive := outcome != api.Blocked && outcome != api.Unreachable
		for _, kind := range kinds {
			queries++
			if queries > limit {
				return errors.New("DNS command exceeds its frozen query budget")
			}
			evidence, exchangeErr := sequence.exchange(ctx, flow.conn, s.DNSName, kind)
			entry.DNS = append(entry.DNS, evidence)
			if ctx.Err() != nil {
				return ctx.Err()
			}
			if positive && exchangeErr != nil {
				return fmt.Errorf("DNS exchange: %w", exchangeErr)
			}
			if !positive && (exchangeErr == nil || evidence.ResponseBytes != 0 || evidence.QueryBytes == 0) {
				return errors.New("denied DNS flow did not send a query and receive no reply")
			}
			if positive {
				flow.sent += uint64(evidence.QueryBytes)
				flow.received += uint64(evidence.ResponseBytes)
				flow.lastReply = time.Now()
			}
			want := dnsFlowExpectation{PID: os.Getpid(), Path: path, Local: evidence.Local, Remote: target, AfterID: flow.afterID,
				ID: flow.log.ID, OutboundLocal: flow.log.OutboundLocal, Outcome: outcome, Interface: iface, RuleID: rule, Sent: flow.sent, Received: flow.received}
			row, err := waitDNSRecord(ctx, c, want)
			if row.ID != 0 {
				flow.log = row
			}
			if err != nil {
				return err
			}
			if err := sameRunningCapture(ctx, c, running); err != nil {
				return err
			}
		}
		return nil
	}
	aType, aaaaType := []dnsmessage.Type{dnsmessage.TypeA}, []dnsmessage.Type{dnsmessage.TypeAAAA}
	for i, target := range s.dnsTargets() {
		addr, _ := netip.ParseAddrPort(target)
		route := func(id string, via ...api.InterfaceRef) api.Rule {
			return api.Rule{ID: id, Enabled: true, Targets: []string{addr.Addr().String()}, Protocol: api.UDP, Action: api.Route, Via: via}
		}
		a, b := route("dns-a", s.Interfaces[0]), route("dns-b", s.Interfaces[1])
		label := fmt.Sprintf("DNS resolver %d: ", i+1)
		if !batch {
			if err := save([]api.Rule{a}); err != nil {
				return err
			}
			return run(label+"single A smoke through interface A", nil, target, aType, api.Routed, s.Interfaces[0].Name, a.ID)
		}
		system := capture.SystemInterface(report.Before.Routes, addr.Addr())
		if system == "" {
			return errors.New("frozen DNS resolver has no effective system route")
		}
		if err := save(nil); err != nil {
			return err
		}
		if err := run(label+"unmatched system egress", nil, target, aType, api.System, system, ""); err != nil {
			return err
		}
		if err := save([]api.Rule{a}); err != nil {
			return err
		}
		reuse, err := open(target)
		if err != nil {
			return err
		}
		err = func() error {
			defer reuse.conn.Close()
			if err := run(label+"A and AAAA reuse one session on A", reuse, target, []dnsmessage.Type{dnsmessage.TypeA, dnsmessage.TypeAAAA}, api.Routed, s.Interfaces[0].Name, a.ID); err != nil {
				return err
			}
			original := reuse.log
			afterID, err := maxConnectionID(ctx, c)
			if err != nil {
				return err
			}
			if err := save([]api.Rule{b}); err != nil {
				return err
			}
			ended, err := waitReloadEnded(ctx, c, running, original)
			entry := checkCase{Name: label + "hot reload ends the previous UDP session on A", Connection: ended}
			if err != nil {
				entry.Error = err.Error()
			}
			report.Cases = append(report.Cases, entry)
			if err != nil {
				return err
			}
			// The native socket and source port stay open. The next datagram must
			// create a new engine session with fresh byte counts and policy B.
			if reuse.conn.LocalAddr().String() != original.Local {
				return errors.New("native UDP source tuple changed during hot reload")
			}
			reuse.afterID, reuse.log, reuse.sent, reuse.received = afterID, api.Connection{}, 0, 0
			return run(label+"same source port creates a new session on B after hot reload", reuse, target, aType, api.Routed, s.Interfaces[1].Name, b.ID)
		}()
		if err != nil {
			return err
		}
		if err := run(label+"new UDP session selects B after hot reload", nil, target, aaaaType, api.Routed, s.Interfaces[1].Name, b.ID); err != nil {
			return err
		}
		fallback := route("dns-ordered-fallback", api.InterfaceRef{Name: "pith-missing"}, s.Interfaces[0])
		if err := save([]api.Rule{fallback}); err != nil {
			return err
		}
		if err := run(label+"missing adapter falls back to A", nil, target, aType, api.Fallback, s.Interfaces[0].Name, fallback.ID); err != nil {
			return err
		}
		fallback = route("dns-system-fallback", api.InterfaceRef{Name: "pith-missing"})
		fallback.Fallback = true
		if err := save([]api.Rule{fallback}); err != nil {
			return err
		}
		if err := run(label+"explicit system fallback", nil, target, aaaaType, api.System, system, fallback.ID); err != nil {
			return err
		}
		block := api.Rule{ID: "dns-block", Enabled: true, Targets: a.Targets, Protocol: api.UDP, Action: api.Block}
		if err := save([]api.Rule{a, block}); err != nil {
			return err
		}
		if err := run(label+"block wins over an earlier route", nil, target, aType, api.Blocked, "", block.ID); err != nil {
			return err
		}
		strict := route("dns-strict", api.InterfaceRef{Name: "pith-missing"})
		if err := save([]api.Rule{strict}); err != nil {
			return err
		}
		if err := run(label+"missing adapter without fallback", nil, target, aType, api.Unreachable, "", strict.ID); err != nil {
			return err
		}
		if i == 0 {
			if err := save([]api.Rule{a}); err != nil {
				return err
			}
			idle, err := open(target)
			if err != nil {
				return err
			}
			err = func() error {
				defer idle.conn.Close()
				if err := run(label+"open DNS session before idle expiry", idle, target, aType, api.Routed, s.Interfaces[0].Name, a.ID); err != nil {
					return err
				}
				ended, err := deps.idle(ctx, c, running, idle.log, idle.lastReply)
				entry := checkCase{Name: label + "DNS idle session expires within its bound", Connection: ended}
				if err != nil {
					entry.Error = err.Error()
				}
				report.Cases = append(report.Cases, entry)
				if err != nil {
					return err
				}
				// Reuse the native socket and source port, but require a newly
				// attributed engine session with the new policy. A new outbound
				// socket may legitimately receive the old ephemeral port again.
				if err := save([]api.Rule{b}); err != nil {
					return err
				}
				id, err := maxConnectionID(ctx, c)
				if err != nil {
					return err
				}
				idle.afterID, idle.log, idle.sent, idle.received = id, api.Connection{}, 0, 0
				return run(label+"same source port creates a new session on B after idle", idle, target, aaaaType, api.Routed, s.Interfaces[1].Name, b.ID)
			}()
			if err != nil {
				return err
			}
		}
	}
	return nil
}

type dnsFlowExpectation struct {
	PID                 int
	Path, Local, Remote string
	AfterID, ID         uint64
	OutboundLocal       string
	Outcome             api.Outcome
	Interface, RuleID   string
	Sent, Received      uint64
}

func checkDNSRecord(rows []api.Connection, want dnsFlowExpectation) (api.Connection, bool, error) {
	var row api.Connection
	for _, candidate := range rows {
		if candidate.ID <= want.AfterID || candidate.Protocol != api.UDP || candidate.Process.PID != want.PID || candidate.Local != want.Local || candidate.Remote != want.Remote {
			continue
		}
		if want.ID != 0 && candidate.ID != want.ID {
			return candidate, false, errors.New("UDP tuple unexpectedly created another engine session without expiry or a policy change")
		}
		if row.ID != 0 {
			return candidate, false, errors.New("multiple UDP records match one application tuple")
		}
		row = candidate
	}
	if row.ID == 0 {
		return row, false, nil
	}
	if row.Process.Path != want.Path || row.Outcome != want.Outcome || row.Interface != want.Interface || row.RuleID != want.RuleID {
		return row, false, errors.New("DNS connection process, outcome, interface or rule differs from the expected path")
	}
	if row.Sent > want.Sent || row.Received > want.Received {
		return row, false, errors.New("DNS flow has unexpected additional payload bytes")
	}
	if row.Sent < want.Sent || row.Received < want.Received {
		return row, false, nil
	}
	positive := want.Outcome != api.Blocked && want.Outcome != api.Unreachable
	if !positive {
		if row.OutboundLocal != "" || row.Ended.IsZero() {
			return row, false, errors.New("denied DNS flow has an outbound socket or remains open")
		}
		return row, true, nil
	}
	local, err := netip.ParseAddrPort(row.OutboundLocal)
	if err != nil || !local.Addr().Is4() || local.Addr().IsUnspecified() || local.Port() == 0 || !row.Ended.IsZero() || row.EndReason != "" {
		return row, false, errors.New("successful DNS flow has no live IPv4 outbound socket tuple")
	}
	if want.OutboundLocal != "" && row.OutboundLocal != want.OutboundLocal {
		return row, false, errors.New("UDP session changed its outbound source tuple")
	}
	return row, true, nil
}

func waitDNSRecord(parent context.Context, c api.Service, want dnsFlowExpectation) (api.Connection, error) {
	ctx, cancel := context.WithTimeout(parent, 3*time.Second)
	defer cancel()
	var row api.Connection
	err := waitUntil(ctx, func() (bool, error) {
		rows, err := c.Connections(ctx, api.ConnectionQuery{Limit: 500})
		if err != nil {
			return false, err
		}
		var ok bool
		row, ok, err = checkDNSRecord(rows, want)
		return ok, err
	})
	return row, err
}

func sameRunningCapture(ctx context.Context, c api.Service, original api.Status) error {
	st, err := c.Status(ctx)
	if err != nil {
		return err
	}
	if st.State != api.Running || !st.Since.Equal(original.Since) || st.LastCapture == nil || original.LastCapture == nil || !sameCapture(*st.LastCapture, *original.LastCapture) {
		return errors.New("acceptance stage stopped or restarted the reviewed capture")
	}
	return nil
}

func waitDNSIdle(parent context.Context, c api.Service, running api.Status, original api.Connection, lastReply time.Time) (api.Connection, error) {
	ctx, cancel := context.WithTimeout(parent, 14*time.Second)
	defer cancel()
	var ended api.Connection
	err := waitUntil(ctx, func() (bool, error) {
		if err := sameRunningCapture(ctx, c, running); err != nil {
			return false, err
		}
		rows, err := c.Connections(ctx, api.ConnectionQuery{Limit: 500})
		if err != nil {
			return false, err
		}
		for _, row := range rows {
			if row.ID != original.ID {
				continue
			}
			ended = row
			if row.Process != original.Process || row.Protocol != original.Protocol || row.Remote != original.Remote || row.Outcome != original.Outcome || row.Local != original.Local || row.OutboundLocal != original.OutboundLocal || row.Sent != original.Sent || row.Received != original.Received || row.RuleID != original.RuleID || row.Interface != original.Interface {
				return false, errors.New("idle DNS session changed its identity, route or byte counts")
			}
			if row.Ended.IsZero() {
				return false, nil
			}
			if row.Problem != "" || row.EndReason != "" || row.Ended.Sub(lastReply) < 9*time.Second {
				return false, errors.New("DNS session ended before idle expiry or for another reason")
			}
			return true, nil
		}
		return false, errors.New("DNS session disappeared before its idle expiry was recorded")
	})
	return ended, err
}
