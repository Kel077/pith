package main

import (
	"bufio"
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/netip"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/Kel077/pith/internal/api"
	"github.com/Kel077/pith/internal/engine/capture"
	"github.com/Kel077/pith/internal/netif"
)

type checkOptions struct {
	batch          bool
	safeFailure    bool
	preserve       bool
	appBypass      bool
	crash          bool
	auth           authorization
	observe        time.Duration
	renewHotspot   bool
	udpDNS         bool
	udpDNSBatch    bool
	quic           bool
	quicBatch      bool
	appBundle      bool
	appBundleBatch bool
	webkit         time.Duration
	webkitOpen     bool
	throughput     string
	vpnRestart     bool
}

type tlsEvidence struct {
	Local      string `json:"local,omitempty"`
	StatusCode int    `json:"statusCode"`
	BodyBytes  int    `json:"bodyBytes"`
	PublicIP   string `json:"publicIP,omitempty"`
}

type preflight struct {
	Interface string      `json:"interface"`
	TLS       tlsEvidence `json:"tls"`
}

type checkCase struct {
	Name          string         `json:"name"`
	Connection    api.Connection `json:"connection"`
	TLS           tlsEvidence    `json:"tls"`
	DNS           []dnsEvidence  `json:"dns,omitempty"`
	QUIC          *quicEvidence  `json:"quic,omitempty"`
	ExpectedIP    string         `json:"expectedPublicIP,omitempty"`
	ExitIPDrift   bool           `json:"exitIPDrift,omitempty"`
	EvidenceNote  string         `json:"evidenceNote,omitempty"`
	RequestPID    int            `json:"requestPID,omitempty"`
	RequestPath   string         `json:"requestExecutable,omitempty"`
	RequestTarget string         `json:"requestTarget,omitempty"`
	Error         string         `json:"error,omitempty"`
}

type checkReport struct {
	Started          time.Time              `json:"started"`
	Finished         time.Time              `json:"finished"`
	Mode             string                 `json:"mode"`
	Baseline         string                 `json:"baselineLabel"`
	BaselineSHA      string                 `json:"baselineSHA256"`
	CaptureMode      string                 `json:"captureMode"`
	Cases            []checkCase            `json:"cases"`
	Traffic          api.Traffic            `json:"traffic"`
	Events           []api.Event            `json:"events"`
	Before           snapshot               `json:"before"`
	After            snapshot               `json:"after"`
	Error            string                 `json:"error,omitempty"`
	StopError        string                 `json:"stopError,omitempty"`
	Restored         bool                   `json:"restored"`
	Observation      bool                   `json:"manualObservationOnly"`
	Notes            []string               `json:"notes,omitempty"`
	Hotspot          *hotspotObservation    `json:"hotspotRenewal,omitempty"`
	SkippedDNS       []capture.DNSExclusion `json:"skippedDNS,omitempty"`
	Connections      []api.Connection       `json:"connections,omitempty"`
	LogError         string                 `json:"connectionLogError,omitempty"`
	RouteDiagnostics []string               `json:"routeDiagnostics,omitempty"`
	WebKit           *webkitEvidence        `json:"webKit,omitempty"`
	Throughput       *throughputEvidence    `json:"throughput,omitempty"`
	VPNRestart       *vpnRestartEvidence    `json:"vpnRestart,omitempty"`
}

func check(parent context.Context, s *session, c api.Service, opts checkOptions) (resultErr error) {
	return checkWithOps(parent, s, c, opts, liveOps())
}

func checkWithOps(parent context.Context, s *session, c api.Service, opts checkOptions, ops rootcheckOps) (resultErr error) {
	if !s.Installed || !s.cancellationRecorded() {
		return errors.New("check requires recorded cancellation or a managed skip, and this session's reviewed installation")
	}
	choices := 0
	for _, selected := range []bool{opts.batch, opts.safeFailure, opts.preserve, opts.appBypass, opts.crash, opts.observe != 0, opts.udpDNS, opts.udpDNSBatch, opts.quic, opts.quicBatch, opts.appBundle, opts.appBundleBatch, opts.webkit != 0, opts.throughput != "", opts.vpnRestart} {
		if selected {
			choices++
		}
	}
	if choices > 1 || opts.observe < 0 || opts.observe > 90*time.Second || opts.webkit < 0 || opts.webkit > 60*time.Second {
		return errors.New("choose one check mode: batch, safe-failure, preserve, app-bypass, crash, udp-dns, udp-dns-batch, quic, quic-batch, app-bundle, app-bundle-batch, throughput a|b, vpn-restart, webkit in (0,60s], or observe in (0,90s]")
	}
	if opts.webkitOpen && opts.webkit == 0 {
		return errors.New("--webkit-open requires an explicit --webkit observation duration")
	}
	if opts.vpnRestart {
		if err := s.reviewedVPNPlan(); err != nil {
			return err
		}
		if ops.vpn == nil {
			return errors.New("VPN acceptance boundaries unavailable")
		}
	}
	if opts.throughput != "" {
		if _, err := throughputAdapter(opts.throughput); err != nil {
			return err
		}
		if err := s.reviewedThroughputPlan(); err != nil {
			return err
		}
		if ops.throughput == nil {
			return errors.New("throughput boundaries unavailable")
		}
	}
	if opts.renewHotspot && opts.observe == 0 {
		return errors.New("--renew-hotspot requires an explicit --observe duration")
	}
	if opts.renewHotspot && s.TestProfile != nil {
		return errors.New("hotspot renewal is currently accepted only in the host-ipv4 harness")
	}
	if choices != 0 && !s.SmokePassed {
		return errors.New("run the default smoke check successfully before additional checks")
	}
	if opts.udpDNS || opts.udpDNSBatch {
		if err := s.reviewedDNSPlan(); err != nil {
			return err
		}
		if opts.udpDNSBatch && !s.DNSPassed {
			return errors.New("run --udp-dns successfully before --udp-dns-batch")
		}
	}
	if opts.quic || opts.quicBatch {
		if err := s.reviewedQUICPlan(); err != nil {
			return err
		}
		if opts.quicBatch && !s.QUICPassed {
			return errors.New("run --quic successfully before --quic-batch")
		}
	}
	if opts.appBundle || opts.appBundleBatch {
		if err := s.reviewedBundlePlan(); err != nil {
			return err
		}
		if opts.appBundleBatch && !s.BundlePassed {
			return errors.New("run --app-bundle successfully before --app-bundle-batch")
		}
	}
	if opts.webkit != 0 {
		if err := s.reviewedWebKitPlan(); err != nil {
			return err
		}
	}
	if opts.appBypass || opts.renewHotspot || opts.appBundle || opts.appBundleBatch {
		if err := appBypassPreflight(s); err != nil {
			return err
		}
	}
	if err := ops.verifyInstalled(s); err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(parent, s.checkTimeout())
	defer cancel()
	status, err := c.Status(ctx)
	if err != nil {
		return err
	}
	if err := idleReviewedStatus(s, status); err != nil {
		return err
	}
	report := checkReport{Started: time.Now(), Mode: "smoke", Baseline: s.BaselineLabel, BaselineSHA: s.baselineDigest(), CaptureMode: s.captureMode()}
	if s.CancelSkipped {
		report.Notes = append(report.Notes, "Manual cancellation dialog skipped under explicit managed authorization; not passed.")
	}
	if opts.batch {
		report.Mode = "batch"
	} else if opts.udpDNS {
		report.Mode = "udp-dns"
	} else if opts.udpDNSBatch {
		report.Mode = "udp-dns-batch"
	} else if opts.quic {
		report.Mode = "quic"
	} else if opts.quicBatch {
		report.Mode = "quic-batch"
	} else if opts.appBundle {
		report.Mode = "app-bundle"
	} else if opts.appBundleBatch {
		report.Mode = "app-bundle-batch"
	} else if opts.webkit != 0 {
		report.Mode, report.Observation = "webkit", true
	} else if opts.throughput != "" {
		report.Mode = "throughput-" + opts.throughput
	} else if opts.vpnRestart {
		report.Mode = "vpn-restart"
	} else if opts.safeFailure {
		report.Mode = "safe-failure"
	} else if opts.preserve {
		report.Mode = "preserve"
	} else if opts.appBypass {
		report.Mode = "app-bypass"
	} else if opts.crash {
		report.Mode = "crash"
	} else if opts.observe > 0 {
		report.Mode, report.Observation = "observe", true
		if opts.renewHotspot {
			report.Mode, report.Observation = "hotspot-renewal", false
		}
	}
	report.Before, err = ops.snapshot(ctx)
	if err != nil {
		return err
	}
	if !sameCleanupBaseline(s.Original, report.Before) {
		return errors.New("routing or tunnel inventory changed since the current baseline; use rebaseline only after a verified stop and an approved mode change")
	}
	plan, err := s.snapshotPlan(report.Before)
	if err != nil {
		return err
	}
	report.SkippedDNS = plan.SkippedDNS
	for _, ref := range s.Interfaces {
		if iface := ref.Resolve(report.Before.Interfaces); iface == nil || !iface.Up {
			return errors.New("a reviewed interface is missing or down")
		}
		if s.ipv6() && !s.usableInterface(ref.Resolve(report.Before.Interfaces)) {
			return errors.New("a reviewed interface has no global IPv6 address; IPv4 cannot satisfy this check")
		}
	}
	if choices == 0 {
		s.SmokePassed = false
	}
	if choices == 0 || opts.udpDNS {
		s.DNSPassed = false
	}
	if choices == 0 || opts.quic {
		s.QUICPassed = false
	}
	if choices == 0 || opts.appBundle {
		s.BundlePassed = false
	}
	// Read-only admission failures preserve the last cleanup proof, so an
	// approved mode switch followed by an accidental check can still rebaseline.
	// From this point cleanup may issue Stop, so invalidate and persist first.
	if err := s.invalidateCleanup(ops); err != nil {
		return err
	}
	// The cleanup context is independent of signal cancellation and test timeout.
	// The helper's immutable test lease is the final bound if this process
	// is killed before its deferred Stop can run.
	defer func() {
		defer func() {
			if opts.vpnRestart && resultErr != nil {
				remindVPNReconnect(report.VPNRestart, *ops.vpn)
			}
		}()
		cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cleanupCancel()
		record, cleanupErr := collectCleanup(cleanupCtx, s, c, ops, "check")
		if opts.vpnRestart && report.VPNRestart != nil && report.VPNRestart.ControlsPassed && (cleanupErr == nil || errors.Is(cleanupErr, errExternalChange)) {
			postCtx, postCancel := context.WithTimeout(context.Background(), 4*time.Second)
			resultErr = errors.Join(resultErr, verifyVPNAfterStop(postCtx, s, record.After, report.VPNRestart, *ops.vpn))
			postCancel()
		}
		report.After, report.Restored = record.After, record.Restored
		report.RouteDiagnostics = slices.Clone(record.Status.RouteDiagnostics)
		// A failed Start may never enter Running or deliver its last event before
		// subscription cancellation. Read the bounded log after Stop, including
		// SafeStart's outbound error, independently of event delivery.
		logCtx, logCancel := context.WithTimeout(context.Background(), 3*time.Second)
		report.Connections, err = c.Connections(logCtx, api.ConnectionQuery{Since: report.Started, Limit: 500})
		logCancel()
		if err != nil {
			report.LogError = err.Error()
		}
		if opts.vpnRestart && report.VPNRestart != nil {
			if err != nil {
				resultErr = errors.Join(resultErr, fmt.Errorf("cannot verify final VPN DNS forwarding records: %w", err))
			} else {
				resultErr = errors.Join(resultErr, checkVPNFinalRows(report.VPNRestart, report.Connections))
			}
		}
		if cleanupErr != nil {
			report.StopError = cleanupErr.Error()
		}
		resultErr = errors.Join(resultErr, cleanupErr)
		if opts.vpnRestart && resultErr != nil {
			remindVPNReconnect(report.VPNRestart, *ops.vpn)
		}
		report.Finished = time.Now()
		if resultErr != nil {
			report.Error = resultErr.Error()
		}
		name, archiveErr := ops.archive(s, "check-"+report.Mode, report)
		resultErr = errors.Join(resultErr, archiveErr)
		if archiveErr == nil {
			fmt.Println("Private check report:", filepath.Join(s.Dir, name))
		}
		if resultErr == nil && choices == 0 {
			s.SmokePassed = true
		}
		if resultErr == nil && opts.udpDNS {
			s.DNSPassed = true
		}
		if resultErr == nil && opts.quic {
			s.QUICPassed = true
		}
		if resultErr == nil && opts.appBundle {
			s.BundlePassed = true
		}
		// A test assertion and cleanup are separate results. A failed case must
		// stay failed, but verified rollback remains valid evidence for rebaseline.
		resultErr = errors.Join(resultErr, s.recordCleanup(record, cleanupErr, ops))
		if resultErr == nil {
			fmt.Println("PASS: this command completed and routing is stopped; uninstall the idle helper when the session is finished.")
		}
	}()
	var speed *throughputRun
	if opts.vpnRestart {
		if err := beginVPNCheck(ctx, s, report.Before, &report, *ops.vpn); err != nil {
			return err
		}
	}
	if opts.throughput != "" {
		speed, err = newThroughputRun(s, opts.throughput, &report, *ops.throughput)
		if err != nil {
			return err
		}
		if err := speed.phase(ctx, c, "direct", 0, nil); err != nil {
			return err
		}
	}
	settings := api.DefaultSettings()
	settings.KeepRouting, settings.ResumeOnLaunch, settings.OpenAtLogin = false, false, false
	settings.IPv6, settings.SafeStart, settings.SniffDomains, settings.LogDays = s.ipv6(), true, true, 1
	cfg := api.Config{Version: api.ConfigVersion, Selected: "rootcheck", Scenes: []api.Scene{{ID: "rootcheck", Name: "Acceptance: " + s.captureMode()}}, Settings: settings}
	address, _ := netip.ParseAddrPort(s.Target)
	block := api.Rule{ID: "block", Enabled: true, Targets: []string{address.Addr().String()}, Protocol: api.TCP, Action: api.Block}
	if opts.safeFailure {
		cfg.Scenes[0].Rules = []api.Rule{block}
	}
	if err := c.SaveConfig(ctx, cfg); err != nil {
		return err
	}
	if opts.safeFailure {
		if err := s.startCapture(ctx, c, cfg.Selected, ops); api.Code(err) != api.ErrNetworkLost {
			return fmt.Errorf("blocked SafeStart returned %v, expected ErrNetworkLost", err)
		}
		status, err := c.Status(ctx)
		if err != nil || status.State != api.Failed || status.Error == nil || status.Error.Code != api.ErrNetworkLost {
			return fmt.Errorf("SafeStart did not report the expected failed status: state=%s error=%v", status.State, err)
		}
		fmt.Println("PASS: the blocked SafeStart returned ErrNetworkLost; checking route rollback next.")
		return nil
	}
	eventCtx, eventCancel := context.WithCancel(ctx)
	events, err := c.Subscribe(eventCtx)
	if err != nil {
		eventCancel()
		return err
	}
	var eventMu sync.Mutex
	eventsDone := make(chan struct{})
	go func() {
		defer close(eventsDone)
		for event := range events {
			eventMu.Lock()
			if len(report.Events) < 2000 {
				report.Events = append(report.Events, event)
			}
			eventMu.Unlock()
		}
	}()
	defer func() { eventCancel(); <-eventsDone }()
	var native *tls.Conn
	if opts.preserve {
		// A native kernel TCP socket is already established when the helper
		// snapshots PCBs. Its next request must keep working after host capture.
		native, err = connectTLS(ctx, s, "")
		if err != nil {
			return fmt.Errorf("open native connection before capture: %w", err)
		}
		defer native.Close()
	}
	if err := s.startCapture(ctx, c, cfg.Selected, ops); err != nil {
		return err
	}
	status, err = c.Status(ctx)
	if err != nil || status.State != api.Running {
		return fmt.Errorf("Start did not reach running: state=%s error=%v", status.State, err)
	}
	runningSince := status.Since
	system := capture.SystemInterface(report.Before.Routes, address.Addr())
	echoBaseline := s
	runCase := func(ctx context.Context, name string, rules []api.Rule, outcome api.Outcome, iface, ruleID string, existing *tls.Conn) error {
		cfg.Scenes[0].Rules = rules
		if err := c.SaveConfig(ctx, cfg); err != nil {
			return err
		}
		last, err := c.Connections(ctx, api.ConnectionQuery{Limit: 500})
		if err != nil {
			return err
		}
		var maxID uint64
		for _, conn := range last {
			if conn.ID > maxID {
				maxID = conn.ID
			}
		}
		// Select the exact still-open connection before sending its HTTP request;
		// an earlier closed connection with the same rule cannot satisfy this case.
		var existingID uint64
		if existing != nil {
			maxID = 0
			for _, conn := range last {
				if conn.Process.PID == os.Getpid() && conn.Remote == s.Target && conn.RuleID == ruleID && conn.Ended.IsZero() && conn.ID > existingID {
					existingID = conn.ID
				}
			}
			if existingID == 0 {
				return errors.New("the open connection has no attributed log entry before hot reload")
			}
		}
		entry := checkCase{Name: name}
		entry.TLS, err = exchangeTLS(ctx, s, "", existing)
		expectSuccess := outcome != api.Blocked && outcome != api.Unreachable
		if (err == nil) != expectSuccess {
			entry.Error = fmt.Sprintf("TLS result does not match expected outcome %s: %v", outcome, err)
			report.Cases = append(report.Cases, entry)
			return errors.New(entry.Error)
		}
		pollCtx, pollCancel := context.WithTimeout(ctx, 4*time.Second)
		defer pollCancel()
		err = waitUntil(pollCtx, func() (bool, error) {
			connections, err := c.Connections(pollCtx, api.ConnectionQuery{Limit: 500})
			if err != nil {
				return false, err
			}
			for _, conn := range connections {
				if conn.ID <= maxID || conn.Process.PID != os.Getpid() || conn.Remote != s.Target || conn.Protocol != api.TCP || existing != nil && conn.ID != existingID {
					continue
				}
				if conn.Ended.IsZero() {
					continue
				}
				entry.Connection = conn
				if conn.Outcome != outcome || conn.Interface != iface || conn.RuleID != ruleID {
					return false, fmt.Errorf("connection log differs: outcome=%s interface=%s rule=%s", conn.Outcome, conn.Interface, conn.RuleID)
				}
				if expectSuccess && (conn.Sent == 0 || conn.Received == 0) {
					return false, errors.New("successful connection has no bidirectional byte counts")
				}
				return true, nil
			}
			return false, nil
		})
		if err == nil && expectSuccess {
			echoInterface := iface
			if outcome == api.System {
				echoInterface = ""
			}
			err = echoBaseline.checkEcho(&entry, echoInterface)
		}
		if err == nil {
			current, statusErr := c.Status(ctx)
			if statusErr != nil || current.State != api.Running || !current.Since.Equal(runningSince) {
				err = fmt.Errorf("hot reload restarted or stopped capture: state=%s error=%v", current.State, statusErr)
			}
		}
		if err != nil {
			entry.Error = err.Error()
		}
		report.Cases = append(report.Cases, entry)
		if err == nil {
			fmt.Println("PASS:", name)
		}
		return err
	}
	if err := runCase(ctx, "system routing after SafeStart", nil, api.System, system, "", nil); err != nil {
		return err
	}
	if opts.udpDNS || opts.udpDNSBatch {
		if err := runDNSChecks(ctx, s, c, cfg, status, &report, opts.udpDNSBatch, liveDNSChecks()); err != nil {
			return err
		}
		return collectTraffic(ctx, c, &report)
	}
	if opts.quic || opts.quicBatch {
		if err := runQUICChecks(ctx, s, c, cfg, status, &report, opts.quicBatch, func(ctx context.Context, fragmented bool) (quicEvidence, error) {
			return exchangeQUIC(ctx, s.QUICTarget, s.QUICServerName, fragmented, nil)
		}); err != nil {
			return err
		}
		return collectTraffic(ctx, c, &report)
	}
	if opts.appBundle || opts.appBundleBatch {
		if err := runBundleChecks(ctx, s, c, cfg, status, system, &report, opts.appBundleBatch); err != nil {
			return err
		}
		return collectTraffic(ctx, c, &report)
	}
	if opts.webkit != 0 {
		return runWebKitCheck(ctx, s, c, cfg, status, &report, opts.webkit, opts.webkitOpen, liveWebKitChecks())
	}
	if speed != nil {
		if err := speed.captured(ctx, c, cfg, status); err != nil {
			return err
		}
		return collectTraffic(ctx, c, &report)
	}
	if opts.vpnRestart {
		return runVPNRestart(ctx, s, c, cfg, status, &report, *ops.vpn)
	}
	if opts.crash {
		err := crashCapture(ctx, s, c, opts.auth, ops)
		entry := checkCase{Name: "crash removes owned capture and helper restarts idle"}
		if err != nil {
			entry.Error = err.Error()
		}
		report.Cases = append(report.Cases, entry)
		return err
	}
	if native != nil {
		entry := checkCase{Name: "native TCP connection survives capture activation", ExpectedIP: s.baselineIP("")}
		entry.TLS, err = exchangeTLS(ctx, s, "", native)
		if err == nil && entry.ExpectedIP != "" && entry.TLS.PublicIP != entry.ExpectedIP {
			err = errors.New("preserved connection's public-IP echo differs from the system preflight")
		}
		if err != nil {
			entry.Error = err.Error()
		}
		report.Cases = append(report.Cases, entry)
		if err != nil {
			return err
		}
		fmt.Println("PASS: native TCP connection survived host-route capture; this legacy flow is not a new proxied connection.")
	}
	route := func(id string, targets []string, via ...api.InterfaceRef) api.Rule {
		return api.Rule{ID: id, Enabled: true, Targets: targets, Protocol: api.TCP, Action: api.Route, Via: via}
	}
	a := route("route-a", []string{address.Addr().String()}, s.Interfaces[0])
	b := route("route-b", []string{address.Addr().String()}, s.Interfaces[1])
	if err := runCase(ctx, "IP rule through interface A", []api.Rule{a}, api.Routed, s.Interfaces[0].Name, a.ID, nil); err != nil {
		return err
	}
	if opts.observe > 0 {
		if opts.renewHotspot {
			return observeHotspot(ctx, s, c, status, opts.observe, &report, ops, func(probeCtx context.Context, bound tlsEvidence) error {
				fresh := *s
				fresh.Preflight = append([]preflight(nil), s.Preflight...)
				for i := range fresh.Preflight {
					if fresh.Preflight[i].Interface == s.Interfaces[0].Name {
						fresh.Preflight[i].TLS = bound
					}
				}
				echoBaseline = &fresh
				return runCase(probeCtx, "new connection uses interface A after hotspot renewal", []api.Rule{a}, api.Routed, s.Interfaces[0].Name, a.ID, nil)
			})
		}
		fmt.Printf("Manual observation window: %s. Capture mode: %s; stops when this command returns.\n", opts.observe, s.captureMode())
		deadline := time.NewTimer(opts.observe)
		defer deadline.Stop()
		ticker := time.NewTicker(time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-deadline.C:
				return nil
			case <-ticker.C:
				st, err := c.Status(ctx)
				if err != nil || st.State != api.Running || !st.Since.Equal(runningSince) || st.LastCapture == nil || status.LastCapture == nil || !sameCapture(*st.LastCapture, *status.LastCapture) {
					return fmt.Errorf("manual observation interrupted: state=%s error=%v", st.State, err)
				}
			}
		}
	}
	if opts.batch {
		if err := runCase(ctx, "IP rule through interface B", []api.Rule{b}, api.Routed, s.Interfaces[1].Name, b.ID, nil); err != nil {
			return err
		}
		domain := route("domain", []string{s.ServerName}, s.Interfaces[0])
		if err := runCase(ctx, "TLS SNI domain rule", []api.Rule{domain}, api.Routed, s.Interfaces[0].Name, domain.ID, nil); err != nil {
			return err
		}
		multi := route("multi", []string{"unrelated.example", address.Addr().String()}, s.Interfaces[1])
		if err := runCase(ctx, "second target of one rule", []api.Rule{multi}, api.Routed, s.Interfaces[1].Name, multi.ID, nil); err != nil {
			return err
		}
		if err := runCase(ctx, "block wins over an earlier route", []api.Rule{a, block}, api.Blocked, "", block.ID, nil); err != nil {
			return err
		}
		missing := api.InterfaceRef{Name: "pith-missing"}
		fallback := route("ordered-fallback", a.Targets, missing, s.Interfaces[0])
		if err := runCase(ctx, "ordered interface fallback", []api.Rule{fallback}, api.Fallback, s.Interfaces[0].Name, fallback.ID, nil); err != nil {
			return err
		}
		fallback = route("system-fallback", a.Targets, missing)
		fallback.Fallback = true
		if err := runCase(ctx, "explicit fallback to system routing", []api.Rule{fallback}, api.System, system, fallback.ID, nil); err != nil {
			return err
		}
		strict := route("strict", a.Targets, missing)
		if err := runCase(ctx, "missing interface without fallback", []api.Rule{strict}, api.Unreachable, "", strict.ID, nil); err != nil {
			return err
		}
		// Complete TLS on A, then edit the rule before writing the HTTP request.
		// The original connection must keep A while a new connection chooses B.
		cfg.Scenes[0].Rules = []api.Rule{a}
		if err := c.SaveConfig(ctx, cfg); err != nil {
			return err
		}
		conn, err := connectTLS(ctx, s, "")
		if err != nil {
			return err
		}
		defer conn.Close()
		if err := runCase(ctx, "open connection survives hot reload", []api.Rule{b}, api.Routed, s.Interfaces[0].Name, a.ID, conn); err != nil {
			return err
		}
		if err := runCase(ctx, "new connection uses hot-reloaded route", []api.Rule{b}, api.Routed, s.Interfaces[1].Name, b.ID, nil); err != nil {
			return err
		}
	}
	if opts.appBypass {
		if err := runAppBypass(ctx, s, c, cfg, runningSince, system, &report); err != nil {
			return err
		}
	}
	return collectTraffic(ctx, c, &report)
}

func collectTraffic(ctx context.Context, c api.Service, report *checkReport) error {
	since := report.Started.Truncate(time.Second)
	var err error
	report.Traffic, err = c.Traffic(ctx, api.TrafficQuery{Since: since, Step: time.Second})
	if err != nil {
		return err
	}
	var sent, received uint64
	for _, sample := range report.Traffic.Series {
		if sample.At.Before(since) || sample.At.Sub(since)%time.Second != 0 {
			return errors.New("live traffic series is not aligned to the query's Since")
		}
		sent += sample.Sent
		received += sample.Received
	}
	if sent == 0 || received == 0 || len(report.Traffic.Apps) == 0 {
		return errors.New("traffic statistics are missing successful traffic or app totals")
	}
	fmt.Println("PASS: connection attribution, byte counts, live traffic, and app totals; checking Stop cleanup next.")
	return nil
}

func connectTLS(ctx context.Context, s *session, iface string) (*tls.Conn, error) {
	return connectTLSObserved(ctx, s, iface, nil)
}

func connectTLSObserved(ctx context.Context, s *session, iface string, local func(string)) (*tls.Conn, error) {
	probeCtx, cancel := context.WithTimeout(ctx, 8*time.Second)
	defer cancel()
	dialer, err := netif.Dialer(iface, 3*time.Second, nil)
	if err != nil {
		return nil, err
	}
	conn, err := dialer.DialContext(probeCtx, s.tcpNetwork(), s.Target)
	if err != nil {
		return nil, err
	}
	if local != nil {
		local(conn.LocalAddr().String())
	}
	tlsConn := tls.Client(conn, &tls.Config{ServerName: s.ServerName, MinVersion: tls.VersionTLS12, NextProtos: []string{"http/1.1"}})
	if err := tlsConn.HandshakeContext(probeCtx); err != nil {
		tlsConn.Close()
		return nil, err
	}
	return tlsConn, nil
}

func exchangeTLS(ctx context.Context, s *session, iface string, conn *tls.Conn) (tlsEvidence, error) {
	ctx, cancel := context.WithTimeout(ctx, 8*time.Second)
	defer cancel()
	var result tlsEvidence
	var err error
	if conn == nil {
		conn, err = connectTLSObserved(ctx, s, iface, func(local string) { result.Local = local })
		if err != nil {
			return result, err
		}
	}
	defer conn.Close()
	result.Local = conn.LocalAddr().String()
	stopCancel := context.AfterFunc(ctx, func() { conn.Close() })
	defer stopCancel()
	if err := conn.SetDeadline(time.Now().Add(8 * time.Second)); err != nil {
		return result, err
	}
	_, port, _ := net.SplitHostPort(s.Target)
	host := s.ServerName
	if port != "443" {
		host = net.JoinHostPort(host, port)
	}
	if _, err := fmt.Fprintf(conn, "GET / HTTP/1.1\r\nHost: %s\r\nConnection: close\r\nUser-Agent: Pith-rootcheck\r\n\r\n", host); err != nil {
		return result, err
	}
	res, err := http.ReadResponse(bufio.NewReader(conn), nil)
	if err != nil {
		return result, err
	}
	defer res.Body.Close()
	result.StatusCode = res.StatusCode
	body, err := io.ReadAll(io.LimitReader(res.Body, 4096))
	result.BodyBytes = len(body)
	if err != nil {
		return result, err
	}
	if res.StatusCode < 200 || res.StatusCode >= 400 {
		return result, fmt.Errorf("test endpoint returned HTTP %d; choose a working HTTP/1.1 endpoint", res.StatusCode)
	}
	if addr, err := netip.ParseAddr(strings.TrimSpace(string(body))); err == nil && addr.IsGlobalUnicast() && !addr.IsPrivate() {
		if addr.Is4In6() || addr.Zone() != "" || addr.Is6() != s.ipv6() {
			return result, errors.New("public-IP echo differs from the frozen endpoint's address family")
		}
		result.PublicIP = addr.String()
	}
	return result, nil
}

func (s *session) baselineIP(iface string) string {
	for _, probe := range s.Preflight {
		if probe.Interface == iface {
			return probe.TLS.PublicIP
		}
	}
	return ""
}
