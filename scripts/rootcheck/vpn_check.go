package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"time"

	"github.com/Kel077/pith/internal/api"
	"github.com/Kel077/pith/internal/engine/capture"
	"golang.org/x/net/dns/dnsmessage"
)

var errVPNInconclusive = errors.New("VPN acceptance incomplete")

type vpnRestartDeps struct {
	snapshot                                                       func(context.Context) (snapshot, error)
	dns                                                            func(context.Context, *vpnRestartPlan) (dnsEvidence, error)
	public                                                         func(context.Context, *session) (tlsEvidence, error)
	prompt                                                         func(string)
	poll, withdrawWindow, minimumDown, rebuildWindow, finalReserve time.Duration
}

func liveVPNChecks(snapshot func(context.Context) (snapshot, error)) vpnRestartDeps {
	return vpnRestartDeps{
		snapshot: snapshot,
		dns: func(ctx context.Context, p *vpnRestartPlan) (dnsEvidence, error) {
			ctx, cancel := context.WithTimeout(ctx, dnsResponseTimeout)
			defer cancel()
			sequence, err := newDNSSequence()
			if err != nil {
				return dnsEvidence{}, err
			}
			conn, err := (&net.Dialer{Timeout: dnsResponseTimeout}).DialContext(ctx, "udp4", p.DNS)
			if err != nil {
				return dnsEvidence{}, err
			}
			defer conn.Close()
			return sequence.exchange(ctx, conn, p.OwnName, dnsmessage.TypeA)
		},
		public: func(ctx context.Context, s *session) (tlsEvidence, error) {
			ctx, cancel := context.WithTimeout(ctx, 3*time.Second)
			defer cancel()
			return exchangeTLS(ctx, s, "", nil)
		},
		prompt: func(line string) { fmt.Println(line) },
		poll:   250 * time.Millisecond, withdrawWindow: 45 * time.Second,
		minimumDown: 10 * time.Second, rebuildWindow: 45 * time.Second, finalReserve: 15 * time.Second,
	}
}

type vpnPoll struct {
	At        time.Time `json:"at"`
	Connected bool      `json:"connected"`
	Interface string    `json:"interface,omitempty"`
	Index     int       `json:"index,omitempty"`
	State     api.State `json:"state"`
	Since     time.Time `json:"since"`
}
type vpnDNSCase struct {
	Phase             string      `json:"phase"`
	Exchange          dnsEvidence `json:"exchange"`
	ExpectedTimeout   bool        `json:"expectedTimeout,omitempty"`
	AfterConnectionID uint64      `json:"afterConnectionID,omitempty"`
	CheckNoForwarding bool        `json:"checkNoForwarding,omitempty"`
}
type vpnSnapshot struct {
	Phase    string   `json:"phase"`
	Snapshot snapshot `json:"snapshot"`
}
type vpnRestartEvidence struct {
	ReadyAt          time.Time     `json:"readyAt"`
	WithdrewAt       time.Time     `json:"withdrewAt"`
	ReconnectAt      time.Time     `json:"reconnectPromptAt"`
	RestoredAt       time.Time     `json:"restoredAt"`
	WithdrawalWindow time.Duration `json:"withdrawalWindow"`
	RebuildWindow    time.Duration `json:"rebuildWindow"`
	MinimumDown      time.Duration `json:"minimumDown"`
	Polls            []vpnPoll     `json:"polls"`
	Queries          []vpnDNSCase  `json:"queries"`
	Snapshots        []vpnSnapshot `json:"snapshots"`
	ControlsPassed   bool          `json:"controlsPassed"`
	PostStopPassed   bool          `json:"postStopPassed"`
	Incomplete       string        `json:"incompleteReason,omitempty"`
	Notes            []string      `json:"notes"`
}

func claimVPN(s *session) error {
	if err := s.reviewedVPNPlan(); err != nil {
		return err
	}
	if err := privateTempDirectory(s.Dir); err != nil {
		return err
	}
	file, err := os.OpenFile(filepath.Join(s.Dir, "vpn-restart-attempt.json"), os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		return errors.New("VPN restart attempt already used or cannot be durably recorded; do not repeat in this session")
	}
	err = json.NewEncoder(file).Encode(struct {
		At   time.Time `json:"at"`
		Plan string    `json:"planSHA256"`
	}{time.Now(), s.VPNPlanSHA256})
	err = errors.Join(err, file.Sync(), file.Close())
	if err != nil {
		return err
	}
	dir, err := os.Open(s.Dir)
	if err != nil {
		return err
	}
	return errors.Join(dir.Sync(), dir.Close())
}

func beginVPNCheck(ctx context.Context, s *session, before snapshot, report *checkReport, deps vpnRestartDeps) (resultErr error) {
	defer func() {
		resultErr = vpnTimeoutResult(resultErr)
		if report.VPNRestart != nil && errors.Is(resultErr, errVPNInconclusive) {
			report.VPNRestart.Incomplete = resultErr.Error()
		}
	}()
	if deps.snapshot == nil || deps.dns == nil || deps.public == nil || deps.prompt == nil || deps.poll <= 0 {
		return errors.New("VPN acceptance dependencies unavailable")
	}
	if err := claimVPN(s); err != nil {
		return err
	}
	evidence := &vpnRestartEvidence{MinimumDown: deps.minimumDown, Notes: []string{
		"One manually requested disconnect/reconnect. No VPN command, peer request or automatic retry.",
		"Status/capture identity and independent snapshots are sampled; the internal egress generation is not exposed by the API.",
		"Own-name MagicDNS is device-local evidence, not remote tailnet connectivity or a VPN daemon self-update/restart.",
		"Withdrawal evidence is a sent query timing out with no matching helper-forwarded tuple; this is not a wire capture proving absence of every possible packet leak.",
		"Existing long-lived TCP continuity and arbitrary peer prefixes remain separate acceptance items.",
	}}
	report.VPNRestart = evidence
	tunnel, up, err := vpnTunnelAt(before, s.VPNRestart.DNS)
	if err != nil {
		return err
	}
	if !up || !reflect.DeepEqual(tunnel, s.VPNRestart.Tunnel) {
		return errors.New("VPN no longer matches the frozen preflight identity and routes")
	}
	exchange, err := deps.dns(ctx, s.VPNRestart)
	evidence.Queries = append(evidence.Queries, vpnDNSCase{Phase: "before-capture", Exchange: exchange})
	if err != nil {
		return err
	}
	_, err = vpnOwnAnswers(s.VPNRestart, tunnel, exchange)
	return err
}

// A controller observation is additional evidence, never authority to edit routes.
func vpnCheckpoint(ctx context.Context, s *session, c api.Service, running api.Status, evidence *vpnRestartEvidence, deps vpnRestartDeps) (snapshot, vpnTunnel, bool, error) {
	var tunnel vpnTunnel
	st, err := c.Status(ctx)
	if err != nil {
		return snapshot{}, tunnel, false, err
	}
	if st.State != api.Running || !st.Since.Equal(running.Since) || st.LastCapture == nil || running.LastCapture == nil || !sameCapture(*st.LastCapture, *running.LastCapture) {
		return snapshot{}, tunnel, false, errors.New("Pith stopped or changed capture identity during VPN observation")
	}
	view, err := deps.snapshot(ctx)
	if err != nil {
		return view, tunnel, false, fmt.Errorf("%w: cannot observe routing: %v", errVPNInconclusive, err)
	}
	if err := vpnCaptureIntact(view, running); err != nil {
		return view, tunnel, false, err
	}
	external := throughputExternalView(view, &running)
	if view.BootSession != s.Original.BootSession {
		return view, tunnel, false, errors.New("boot identity changed during VPN observation")
	}
	public, _ := netip.ParseAddrPort(s.Target)
	egress := capture.SystemInterface(external.Routes, public.Addr())
	if egress == "" || strings.HasPrefix(egress, "utun") {
		return view, tunnel, false, errors.New("public control no longer has a physical system exit without an exit node")
	}
	tunnel, up, err := vpnTunnelAt(external, s.VPNRestart.DNS)
	if err != nil {
		return view, tunnel, up, err
	}
	if up && !slices.Equal(tunnel.Addresses, s.VPNRestart.Tunnel.Addresses) {
		return view, tunnel, up, errors.New("rebuilt tunnel does not retain the reviewed local VPN addresses")
	}
	if len(evidence.Polls) >= 512 {
		return view, tunnel, up, fmt.Errorf("%w: observation count exceeded", errVPNInconclusive)
	}
	evidence.Polls = append(evidence.Polls, vpnPoll{time.Now(), up, tunnel.Name, tunnel.Index, st.State, st.Since})
	return view, tunnel, up, nil
}

func vpnCaptureIntact(view snapshot, st api.Status) error {
	id := st.LastCapture
	if id == nil || view.InterfaceIndices[id.Interface] != id.Index {
		return errors.New("owned capture interface missing during VPN observation")
	}
	iface := (api.InterfaceRef{Name: id.Interface}).Resolve(view.Interfaces)
	if iface == nil || !iface.Up {
		return errors.New("owned capture interface down during VPN observation")
	}
	for _, raw := range id.Prefixes {
		prefix, err := netip.ParsePrefix(raw)
		if err != nil {
			return err
		}
		count := 0
		for _, route := range view.Routes {
			if route.Prefix != prefix || route.Scoped {
				continue
			}
			count++
			if route.Interface != id.Interface || route.Index != id.Index || route.Gateway.IsValid() || route.Reject || route.Local {
				return errors.New("capture route ownership changed during VPN observation")
			}
		}
		if count != 1 {
			return errors.New("capture route missing or duplicated during VPN observation")
		}
	}
	return nil
}

func vpnWindow(ctx context.Context, maximum, reserve time.Duration) time.Duration {
	if deadline, ok := ctx.Deadline(); ok {
		return max(0, min(maximum, time.Until(deadline)-reserve))
	}
	return maximum
}

func waitVPNState(ctx context.Context, s *session, c api.Service, running api.Status, evidence *vpnRestartEvidence, deps vpnRestartDeps, connected bool, window time.Duration) (snapshot, vpnTunnel, error) {
	ctx, cancel := context.WithTimeout(ctx, window)
	defer cancel()
	for {
		view, tunnel, up, err := vpnCheckpoint(ctx, s, c, running, evidence, deps)
		if err != nil {
			if ctx.Err() != nil {
				return view, tunnel, fmt.Errorf("%w: VPN transition deadline", errVPNInconclusive)
			}
			return view, tunnel, err
		}
		if connected && up || !connected && !vpnDNSProtected(throughputExternalView(view, &running), s.VPNRestart.DNS) {
			return view, tunnel, nil
		}
		if err := processPause(ctx, deps.poll); err != nil {
			return view, tunnel, fmt.Errorf("%w: VPN transition deadline", errVPNInconclusive)
		}
	}
}

func noVPNForward(ctx context.Context, c api.Service, after uint64, target, local string) error {
	rows, err := c.Connections(ctx, api.ConnectionQuery{Limit: 500})
	if err != nil {
		return err
	}
	for _, row := range rows {
		if row.ID > after && row.Remote == target && row.Local == local && row.Process.PID == os.Getpid() {
			return errors.New("device-local VPN DNS query entered a Pith forwarding session")
		}
	}
	return nil
}

func queryVPN(ctx context.Context, s *session, c api.Service, tunnel vpnTunnel, evidence *vpnRestartEvidence, deps vpnRestartDeps, phase string, down bool) error {
	id, err := maxConnectionID(ctx, c)
	if err != nil {
		return err
	}
	exchange, queryErr := deps.dns(ctx, s.VPNRestart)
	evidence.Queries = append(evidence.Queries, vpnDNSCase{Phase: phase, Exchange: exchange, ExpectedTimeout: down, AfterConnectionID: id, CheckNoForwarding: true})
	if err := noVPNForward(ctx, c, id, s.VPNRestart.DNS, exchange.Local); err != nil {
		return err
	}
	if down {
		var timed net.Error
		local, localErr := netip.ParseAddrPort(exchange.Local)
		if queryErr == nil || !errors.As(queryErr, &timed) || !timed.Timeout() || exchange.QueryBytes == 0 || exchange.ResponseBytes != 0 || localErr != nil || local.Port() == 0 || exchange.Remote != s.VPNRestart.DNS || exchange.Name != s.VPNRestart.OwnName {
			return fmt.Errorf("%w: withdrawal query did not produce a sent, unanswered datagram timeout", errVPNInconclusive)
		}
		return nil
	}
	if queryErr != nil {
		return queryErr
	}
	_, err = vpnOwnAnswers(s.VPNRestart, tunnel, exchange)
	return err
}

func vpnPublic(ctx context.Context, s *session, c api.Service, cfg api.Config, running api.Status, report *checkReport, deps vpnRestartDeps, name string, route bool) error {
	if err := sameRunningCapture(ctx, c, running); err != nil {
		return err
	}
	cfg.Scenes[0].Rules = nil
	if route {
		cfg.Scenes[0].Rules = []api.Rule{{ID: "vpn-public", Enabled: true, Targets: []string{strings.Split(s.Target, ":")[0]}, Protocol: api.TCP, Action: api.Route, Via: []api.InterfaceRef{s.Interfaces[0]}}}
	}
	if err := c.SaveConfig(ctx, cfg); err != nil {
		return err
	}
	before, err := deps.snapshot(ctx)
	if err != nil {
		return err
	}
	public, _ := netip.ParseAddrPort(s.Target)
	want := capture.SystemInterface(throughputExternalView(before, &running).Routes, public.Addr())
	outcome, rule := api.System, ""
	if route {
		iface := s.Interfaces[0].Resolve(before.Interfaces)
		if iface == nil || !iface.Up {
			return errors.New("reviewed physical rule interface unavailable")
		}
		want, outcome, rule = iface.Name, api.Routed, "vpn-public"
	}
	id, err := maxConnectionID(ctx, c)
	if err != nil {
		return err
	}
	entry := checkCase{Name: name}
	entry.TLS, err = deps.public(ctx, s)
	if err == nil && (entry.TLS.StatusCode < 200 || entry.TLS.StatusCode >= 300 || entry.TLS.BodyBytes <= 0 || entry.TLS.Local == "") {
		err = errors.New("public VPN control lacks a successful exact TLS exchange")
	}
	if err == nil {
		waitCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
		defer cancel()
		path, pathErr := os.Executable()
		if pathErr != nil {
			return pathErr
		}
		err = waitUntil(waitCtx, func() (bool, error) {
			rows, err := c.Connections(waitCtx, api.ConnectionQuery{Limit: 500})
			if err != nil {
				return false, err
			}
			for _, row := range rows {
				if row.ID <= id || row.Local != entry.TLS.Local || row.Remote != s.Target || row.Process.PID != os.Getpid() || row.Protocol != api.TCP {
					continue
				}
				entry.Connection = row
				if !canonicalSame(row.Process.Path, path) || row.RuleID != rule || row.Outcome != outcome || row.Interface != want {
					return false, errors.New("public VPN control used the wrong process, rule or interface")
				}
				out, outErr := netip.ParseAddrPort(row.OutboundLocal)
				return outErr == nil && out.Port() != 0 && row.Sent > 0 && row.Received > 0, nil
			}
			return false, nil
		})
	}
	if err != nil {
		entry.Error = err.Error()
	}
	report.Cases = append(report.Cases, entry)
	return err
}

// Keep observing during bounded requests so reconnecting early cannot be hidden
// by a blocking UDP read. Always cancel and join the request worker.
func vpnDisconnectedControls(ctx context.Context, s *session, c api.Service, cfg api.Config, running api.Status, report *checkReport, deps vpnRestartDeps) error {
	evidence := report.VPNRestart
	workCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	done := make(chan error, 1)
	// The worker writes query/case evidence; the observer writes only Polls.
	go func() {
		err := queryVPN(workCtx, s, c, vpnTunnel{}, evidence, deps, "withdrawn", true)
		if err == nil {
			err = vpnPublic(workCtx, s, c, cfg, running, report, deps, "system public control during VPN withdrawal", false)
		}
		done <- err
	}()
	finished := false
	defer func() {
		cancel()
		if !finished {
			<-done
		}
	}()
	for {
		view, _, up, err := vpnCheckpoint(ctx, s, c, running, evidence, deps)
		if err != nil {
			return err
		}
		if up || vpnDNSProtected(throughputExternalView(view, &running), s.VPNRestart.DNS) {
			return fmt.Errorf("%w: VPN reconnected before the disconnected controls and minimum interval completed", errVPNInconclusive)
		}
		if finished && time.Since(evidence.WithdrewAt) >= deps.minimumDown {
			return nil
		}
		timer := time.NewTimer(deps.poll)
		select {
		case err := <-done:
			timer.Stop()
			finished = true
			if err != nil {
				return err
			}
		case <-timer.C:
		case <-ctx.Done():
			timer.Stop()
			return fmt.Errorf("%w: disconnected observation deadline", errVPNInconclusive)
		}
	}
}

func runVPNRestart(ctx context.Context, s *session, c api.Service, cfg api.Config, running api.Status, report *checkReport, deps vpnRestartDeps) (resultErr error) {
	evidence := report.VPNRestart
	defer func() {
		resultErr = vpnTimeoutResult(resultErr)
		if errors.Is(resultErr, errVPNInconclusive) {
			evidence.Incomplete = resultErr.Error()
		}
	}()
	view, tunnel, up, err := vpnCheckpoint(ctx, s, c, running, evidence, deps)
	if err != nil {
		return err
	}
	if !up {
		return fmt.Errorf("%w: VPN disconnected before READY", errVPNInconclusive)
	}
	evidence.Snapshots = append(evidence.Snapshots, vpnSnapshot{"running-before", view})
	if err := queryVPN(ctx, s, c, tunnel, evidence, deps, "running-before", false); err != nil {
		return err
	}
	if err := vpnPublic(ctx, s, c, cfg, running, report, deps, "physical rule before VPN withdrawal", true); err != nil {
		return err
	}
	evidence.WithdrawalWindow = vpnWindow(ctx, deps.withdrawWindow, deps.minimumDown+15*time.Second+deps.finalReserve)
	if evidence.WithdrawalWindow < 15*time.Second {
		return fmt.Errorf("%w: insufficient lease remaining for human action", errVPNInconclusive)
	}
	evidence.ReadyAt = time.Now()
	deps.prompt(fmt.Sprintf("READY: Turn the VPN off within %s, keep it off about 15s, then turn it on once; no exit node.", evidence.WithdrawalWindow.Round(time.Second)))
	view, _, err = waitVPNState(ctx, s, c, running, evidence, deps, false, evidence.WithdrawalWindow)
	if err != nil {
		return err
	}
	evidence.WithdrewAt = time.Now()
	evidence.Snapshots = append(evidence.Snapshots, vpnSnapshot{"withdrawn", view})
	if err := vpnDisconnectedControls(ctx, s, c, cfg, running, report, deps); err != nil {
		return err
	}
	evidence.RebuildWindow = vpnWindow(ctx, deps.rebuildWindow, deps.finalReserve)
	if evidence.RebuildWindow <= 0 {
		return fmt.Errorf("%w: no lease remaining for reconnection", errVPNInconclusive)
	}
	evidence.ReconnectAt = time.Now()
	deps.prompt(fmt.Sprintf("RECONNECT: Disconnected controls completed; turn the VPN on now, within %s; no exit node.", evidence.RebuildWindow.Round(time.Second)))
	view, tunnel, err = waitVPNState(ctx, s, c, running, evidence, deps, true, evidence.RebuildWindow)
	if err != nil {
		return err
	}
	evidence.RestoredAt = time.Now()
	evidence.Snapshots = append(evidence.Snapshots, vpnSnapshot{"rebuilt", view})
	if err := queryVPN(ctx, s, c, tunnel, evidence, deps, "rebuilt", false); err != nil {
		return err
	}
	if err := vpnPublic(ctx, s, c, cfg, running, report, deps, "physical rule after VPN rebuild", true); err != nil {
		return err
	}
	if err := vpnPublic(ctx, s, c, cfg, running, report, deps, "system public control after VPN rebuild", false); err != nil {
		return err
	}
	if _, _, up, err := vpnCheckpoint(ctx, s, c, running, evidence, deps); err != nil {
		return err
	} else if !up {
		return fmt.Errorf("%w: VPN disconnected again", errVPNInconclusive)
	}
	evidence.ControlsPassed = true
	deps.prompt("RESTORED: VPN and public controls passed with the same Pith capture; stopping and verifying cleanup.")
	return collectTraffic(ctx, c, report)
}

func vpnTimeoutResult(err error) error {
	if err == nil || errors.Is(err, errVPNInconclusive) {
		return err
	}
	var timeout net.Error
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) || errors.As(err, &timeout) && timeout.Timeout() {
		return fmt.Errorf("%w: %v", errVPNInconclusive, err)
	}
	return err
}

func checkVPNFinalRows(evidence *vpnRestartEvidence, rows []api.Connection) error {
	for _, query := range evidence.Queries {
		if !query.CheckNoForwarding {
			continue
		}
		for _, row := range rows {
			if row.ID > query.AfterConnectionID && row.Remote == query.Exchange.Remote && row.Local == query.Exchange.Local && row.Process.PID == os.Getpid() {
				evidence.ControlsPassed = false
				return errors.New("post-Stop logs contain a forwarded device-local VPN DNS tuple")
			}
		}
	}
	return nil
}

func verifyVPNAfterStop(ctx context.Context, s *session, view snapshot, evidence *vpnRestartEvidence, deps vpnRestartDeps) (resultErr error) {
	defer func() {
		resultErr = vpnTimeoutResult(resultErr)
		if errors.Is(resultErr, errVPNInconclusive) {
			evidence.Incomplete = resultErr.Error()
		}
	}()
	tunnel, up, err := vpnTunnelAt(view, s.VPNRestart.DNS)
	if err != nil {
		return err
	}
	if !up {
		return errors.New("VPN route absent after Pith Stop")
	}
	exchange, err := deps.dns(ctx, s.VPNRestart)
	evidence.Queries = append(evidence.Queries, vpnDNSCase{Phase: "after-stop", Exchange: exchange})
	if err == nil {
		_, err = vpnOwnAnswers(s.VPNRestart, tunnel, exchange)
	}
	evidence.PostStopPassed = err == nil
	return err
}
