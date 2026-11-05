package engine

import (
	"context"
	"errors"
	"net/netip"
	"path/filepath"
	"sync/atomic"
	"time"

	"github.com/Kel077/pith/internal/api"
	"github.com/Kel077/pith/internal/engine/capture"
	"github.com/Kel077/pith/internal/engine/echo"
	"github.com/Kel077/pith/internal/engine/egress"
	"github.com/Kel077/pith/internal/engine/forward"
	"github.com/Kel077/pith/internal/engine/passthrough"
)

type session struct {
	capture                *capture.Capture
	forward                *forward.Forwarder
	echo                   *echo.Relay
	existing               *passthrough.Table
	router                 *egress.Router
	guard                  *capture.Guard
	routeDone              chan struct{}
	scene                  api.Scene
	settings               api.Settings
	ifaces                 []api.Interface
	cancel                 context.CancelFunc
	done                   chan struct{}
	baseSent, baseReceived uint64
	lastSent, lastReceived uint64
	lastAt                 time.Time
	leaseUntil             time.Time
	leaseTimer             *time.Timer
	leaseDone              chan struct{}
}

func (s *session) stopLease() {
	if s.leaseTimer != nil && !s.leaseTimer.Stop() {
		<-s.leaseDone
	}
}

func (e *Engine) startLocked(ctx context.Context, scene api.Scene, settings api.Settings) (result error) {
	e.state(api.Status{State: api.Starting, SceneID: scene.ID})
	s := &session{scene: scene, settings: settings, done: make(chan struct{})}
	if e.opts.TestProfile != nil {
		s.leaseUntil = time.Now().Add(e.scopedLease)
		var cancelStartup context.CancelFunc
		ctx, cancelStartup = context.WithDeadline(ctx, s.leaseUntil)
		defer cancelStartup()
	}
	runCtx, cancel := context.WithCancel(context.Background())
	s.cancel = cancel
	changes, err := e.opts.WatchRoutes(runCtx)
	if err == nil && changes == nil {
		err = errors.New("route observer is unavailable")
	}
	if err != nil {
		cancel()
		e.state(api.Status{State: api.Failed, Error: errorValue(err)})
		return err
	}
	defer func() {
		if result != nil {
			s.stopLease()
			s.cancel()
			if s.guard != nil {
				s.guard.Revoke(result)
			}
			if s.routeDone != nil {
				<-s.routeDone
			} else {
				for event := range changes {
					e.routeDiagnostic(event.Diagnostic)
				}
			}
			if s.capture != nil {
				result = errors.Join(result, s.capture.Close())
				e.mu.Lock()
				e.status.LastCapture = s.capture.Identity()
				e.mu.Unlock()
			}
			if s.echo != nil {
				s.echo.Close()
			}
			if s.forward != nil {
				s.forward.Close()
			}
			if s.existing != nil {
				result = errors.Join(result, s.existing.Close())
			}
			// Startup has no running telemetry monitor yet. Publish its final flows
			// as well as persisting them, so SafeStart failures retain dial errors.
			result = errors.Join(result, e.flush(true))
			e.state(api.Status{State: api.Failed, Error: errorValue(result)})
		}
	}()
	if err := e.store.SetLogDays(settings.LogDays); err != nil {
		return err
	}
	ifaces, err := e.opts.Interfaces(ctx)
	if err != nil {
		return err
	}
	s.ifaces = ifaces
	var probe probeTarget
	if settings.SafeStart && e.opts.Verify == nil {
		probe, err = prepareProbe(ctx, e.opts, settings.IPv6)
		if err != nil {
			return &api.Error{Code: api.ErrNetworkLost, Detail: err.Error()}
		}
	}
	var dns []netip.Addr
	for _, iface := range ifaces {
		for _, server := range iface.DNS {
			if addr, err := netip.ParseAddr(server); err == nil {
				dns = append(dns, addr)
			}
		}
	}
	var expected []netip.Prefix
	if e.opts.TestProfile != nil {
		expected = e.opts.TestProfile.Prefixes()
	}
	s.capture, err = capture.Prepare(ctx, capture.Options{IPv6: settings.IPv6, Scopes: e.opts.Scopes, DNS: dns, ExpectedPrefixes: expected, StatePath: filepath.Join(e.opts.DataDir, "capture.json"), Driver: e.opts.Driver, Diagnostic: e.routeDiagnostic})
	if err != nil {
		return err
	}
	if e.opts.TestProfile != nil {
		// This timer closes owned capture even when a controller, Reload or disk
		// operation holds the engine lifecycle lock. Stop always joins its work.
		s.leaseDone = make(chan struct{})
		s.leaseTimer = time.AfterFunc(time.Until(s.leaseUntil), func() {
			defer close(s.leaseDone)
			_ = s.capture.Close()
		})
	}
	e.mu.Lock()
	e.status.LastCapture = s.capture.Identity()
	e.mu.Unlock()
	s.router = egress.New(s.capture.Plan.Before, ifaces, e.opts.Dial)
	s.guard, err = capture.NewGuard(s.capture, ifaces)
	if err != nil {
		return err
	}
	s.router.Guard = s.guard
	s.routeDone = make(chan struct{})
	go func() {
		defer close(s.routeDone)
		for event := range changes {
			e.routeDiagnostic(s.guard.RouteDiagnostic(event))
			if runCtx.Err() == nil {
				s.guard.Observe(event)
			}
		}
		if runCtx.Err() == nil {
			s.guard.Revoke(errors.New("route observer closed"))
		}
	}()
	if err := s.guard.Validate(false); err != nil {
		return err
	}
	if err := s.router.Reload(scene, settings); err != nil {
		return err
	}
	s.existing, err = passthrough.New(passthrough.Options{Routes: s.capture.Plan.Before, Snapshot: e.opts.ExistingSockets, Sender: e.opts.RawSender, Guard: s.guard})
	if err != nil {
		return err
	}
	var bridge atomic.Pointer[forward.Forwarder]
	s.echo = echo.New(echo.Options{System: s.router.System, Exchange: e.opts.EchoExchange, Guard: s.guard, Write: func(packet []byte) error {
		if f := bridge.Load(); f != nil {
			return f.WritePacket(packet)
		}
		return errors.New("tunnel is not ready")
	}})
	s.forward, err = forward.New(forward.Options{Device: s.capture.Device, Router: s.router, Store: e.store, Lookup: e.opts.Lookup, IPv6: settings.IPv6, Echo: s.echo.Handle, Packet: func(p []byte) bool { return s.guard.Packet(p) || s.existing.Packet(p) }, Unknown: s.existing.Unknown, InterceptTCP: s.existing.Intercepted})
	if err != nil {
		return err
	}
	bridge.Store(s.forward)
	_, s.baseSent, s.baseReceived = e.store.Totals()
	s.lastSent, s.lastReceived = s.baseSent, s.baseReceived
	s.lastAt = time.Now()
	if err := s.capture.Activate(ctx); err != nil {
		return err
	}
	if err := s.guard.Validate(true); err != nil {
		return err
	}
	started := time.Now()
	if e.scopedLease > 0 && s.leaseUntil.IsZero() {
		s.leaseUntil = started.Add(e.scopedLease)
	}
	if settings.SafeStart {
		verifyCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
		stopVerify := context.AfterFunc(s.guard.Context(), cancel)
		if e.opts.Verify != nil {
			err = e.opts.Verify(verifyCtx, s.capture, s.forward)
		} else {
			err = probe.verify(verifyCtx, s.capture.Plan, s.forward)
		}
		cancel()
		stopVerify()
		if err != nil {
			return &api.Error{Code: api.ErrNetworkLost, Detail: errors.Join(err, s.guard.Err()).Error()}
		}
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := s.guard.Validate(true); err != nil {
		return err
	}
	e.mu.Lock()
	e.run = s
	e.status = api.Status{State: api.Running, SceneID: scene.ID, Since: started, Warnings: warnings(s), LastCapture: e.status.LastCapture, RouteDiagnostics: e.status.RouteDiagnostics}
	e.publishLocked(api.Event{Status: statusPointer(e.status)})
	e.mu.Unlock()
	go e.monitor(runCtx, s)
	return nil
}

func (e *Engine) stopLocked() error {
	e.mu.Lock()
	s := e.run
	if s == nil {
		e.status = api.Status{State: api.Stopped, LastCapture: e.status.LastCapture, RouteDiagnostics: e.status.RouteDiagnostics}
		e.publishLocked(api.Event{Status: statusPointer(e.status)})
		e.mu.Unlock()
		_, err := e.store.Flush(true)
		return err
	}
	e.run = nil
	e.status.State = api.Stopping
	e.publishLocked(api.Event{Status: statusPointer(e.status)})
	e.mu.Unlock()
	s.cancel()
	s.stopLease()
	s.guard.Revoke(errors.New("routing stopped"))
	<-s.routeDone
	// Restore kernel routing before waiting for application relays or disk I/O.
	err := s.capture.Close()
	e.mu.Lock()
	e.status.LastCapture = s.capture.Identity()
	e.mu.Unlock()
	s.echo.Close()
	s.forward.Close()
	err = errors.Join(err, s.existing.Close())
	<-s.done
	err = errors.Join(err, e.flush(true))
	status := api.Status{State: api.Stopped}
	if err != nil {
		status.State = api.Failed
		status.Error = errorValue(err)
	}
	e.state(status)
	return err
}

func warnings(s *session) []api.Warning {
	warnings := append([]api.Warning(nil), s.capture.Plan.Warnings...)
	missing := make(map[string]bool)
	noDomains := false
	for _, rule := range s.scene.Rules {
		if !rule.Enabled {
			continue
		}
		for _, ref := range rule.Via {
			iface := ref.Resolve(s.ifaces)
			if (iface == nil || !iface.Up) && !missing[ref.Name] {
				missing[ref.Name] = true
				warnings = append(warnings, api.Warning{Code: api.WarnInterfaceDown, Detail: ref.Name})
			}
		}
		if !s.settings.SniffDomains && !noDomains {
			for _, raw := range rule.Targets {
				target, err := api.ParseTarget(raw)
				if err == nil && target.IsDomain() {
					warnings = append(warnings, api.Warning{Code: api.WarnNoDomains})
					noDomains = true
					break
				}
			}
		}
	}
	return warnings
}

func (e *Engine) flush(force bool) error {
	rows, err := e.store.Flush(force)
	e.mu.Lock()
	defer e.mu.Unlock()
	for _, row := range rows {
		copy := row
		e.publishLocked(api.Event{Connection: &copy})
	}
	return err
}

func (e *Engine) monitor(ctx context.Context, s *session) {
	defer close(s.done)
	updates, watchErr := e.opts.WatchInterfaces(ctx)
	if watchErr == nil && updates == nil {
		watchErr = errors.New("interface observer is unavailable")
	}
	defer func() {
		s.cancel()
		if updates != nil {
			for range updates {
			}
		}
	}()
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	var leaseExpired <-chan time.Time
	if !s.leaseUntil.IsZero() {
		lease := time.NewTimer(time.Until(s.leaseUntil))
		defer lease.Stop()
		leaseExpired = lease.C
	}
	fail := func(err error) {
		if err != nil && ctx.Err() == nil {
			s.guard.Revoke(err)
			go e.failSession(s, err)
		}
	}
	if watchErr != nil {
		updates = nil
	}
	for {
		select {
		case <-ctx.Done():
			return
		case <-s.guard.Context().Done():
			fail(s.guard.Err())
			return
		case <-leaseExpired:
			fail(errors.New("capture test lease expired; routing was stopped"))
			return
		case err, ok := <-s.capture.Faults:
			if ok {
				fail(err)
			} else {
				fail(errors.New("tunnel event stream closed"))
			}
			return
		case err, ok := <-s.forward.Faults():
			if ok {
				fail(err)
			} else {
				fail(errors.New("tunnel packet stream closed"))
			}
			return
		case err := <-s.existing.Faults():
			fail(err)
			return
		case ifaces, ok := <-updates:
			if !ok {
				updates = nil
				continue
			}
			if err := s.guard.Interfaces(ifaces); err != nil {
				fail(err)
				return
			}
			s.router.Interfaces(ifaces)
			e.mu.Lock()
			s.ifaces = ifaces
			if e.run == s {
				e.status.Warnings = warnings(s)
				e.publishLocked(api.Event{Status: statusPointer(e.status)})
			}
			e.mu.Unlock()
		case now := <-ticker.C:
			// Metadata observation can recover by polling. Ownership still has the
			// independent route observer and verified snapshot below.
			if updates == nil {
				readCtx, cancel := context.WithTimeout(ctx, time.Second)
				if ifaces, err := e.opts.Interfaces(readCtx); err == nil {
					s.guard.Interfaces(ifaces)
					s.router.Interfaces(ifaces)
					e.mu.Lock()
					s.ifaces = ifaces
					e.mu.Unlock()
				}
				cancel()
			}
			if err := s.guard.Validate(true); err != nil {
				fail(err)
				return
			}
			if err := s.existing.Refresh(); err != nil {
				fail(err)
				return
			}
			if err := e.flush(false); err != nil {
				fail(err)
				return
			}
			open, sent, received := e.store.Totals()
			elapsed := now.Sub(s.lastAt).Seconds()
			e.mu.Lock()
			if e.run == s {
				e.status.Open = open
				e.status.Sent = sent - s.baseSent
				e.status.Received = received - s.baseReceived
				if elapsed > 0 {
					e.status.SendRate = uint64(float64(sent-s.lastSent) / elapsed)
					e.status.ReceiveRate = uint64(float64(received-s.lastReceived) / elapsed)
				}
				e.publishLocked(api.Event{Status: statusPointer(e.status)})
			}
			e.mu.Unlock()
			s.lastAt, s.lastSent, s.lastReceived = now, sent, received
		}
	}
}

func (e *Engine) failSession(s *session, err error) {
	e.lifecycle.Lock()
	defer e.lifecycle.Unlock()
	if e.run != s {
		return
	}
	cleanup := e.stopLocked()
	e.state(api.Status{State: api.Failed, Error: &api.Error{Code: api.ErrNetworkLost, Detail: errors.Join(err, cleanup).Error()}})
}
