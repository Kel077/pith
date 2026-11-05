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
	scene                  api.Scene
	settings               api.Settings
	ifaces                 []api.Interface
	cancel                 context.CancelFunc
	done                   chan struct{}
	baseSent, baseReceived uint64
	lastSent, lastReceived uint64
	lastAt                 time.Time
	leaseUntil             time.Time
}

func (e *Engine) startLocked(ctx context.Context, scene api.Scene, settings api.Settings) (result error) {
	e.state(api.Status{State: api.Starting, SceneID: scene.ID})
	s := &session{scene: scene, settings: settings, done: make(chan struct{})}
	defer func() {
		if result != nil {
			if s.capture != nil {
				result = errors.Join(result, s.capture.Close())
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
			e.store.Flush(true)
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
	s.capture, err = capture.Prepare(ctx, capture.Options{IPv6: settings.IPv6, Scopes: e.opts.Scopes, DNS: dns, StatePath: filepath.Join(e.opts.DataDir, "capture.json"), Driver: e.opts.Driver})
	if err != nil {
		return err
	}
	e.mu.Lock()
	e.status.LastCapture = s.capture.Identity()
	e.mu.Unlock()
	s.router = egress.New(s.capture.Plan.Before, ifaces, e.opts.Dial)
	if err := s.router.Reload(scene, settings); err != nil {
		return err
	}
	s.existing, err = passthrough.New(passthrough.Options{Routes: s.capture.Plan.Before, Snapshot: e.opts.ExistingSockets, Sender: e.opts.RawSender})
	if err != nil {
		return err
	}
	var bridge atomic.Pointer[forward.Forwarder]
	s.echo = echo.New(echo.Options{System: s.router.System, Exchange: e.opts.EchoExchange, Write: func(packet []byte) error {
		if f := bridge.Load(); f != nil {
			return f.WritePacket(packet)
		}
		return errors.New("tunnel is not ready")
	}})
	s.forward, err = forward.New(forward.Options{Device: s.capture.Device, Router: s.router, Store: e.store, Lookup: e.opts.Lookup, IPv6: settings.IPv6, Echo: s.echo.Handle, Packet: s.existing.Packet, Unknown: s.existing.Unknown, InterceptTCP: s.existing.Intercepted})
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
	started := time.Now()
	if e.scopedLease > 0 {
		s.leaseUntil = started.Add(e.scopedLease)
	}
	if settings.SafeStart {
		verifyCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
		if e.opts.Verify != nil {
			err = e.opts.Verify(verifyCtx, s.capture, s.forward)
		} else {
			err = probe.verify(verifyCtx, s.capture.Plan, s.forward)
		}
		cancel()
		if err != nil {
			return &api.Error{Code: api.ErrNetworkLost, Detail: err.Error()}
		}
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	runCtx, cancel := context.WithCancel(context.Background())
	s.cancel = cancel
	e.mu.Lock()
	e.run = s
	e.status = api.Status{State: api.Running, SceneID: scene.ID, Since: started, Warnings: warnings(s), LastCapture: e.status.LastCapture}
	e.publishLocked(api.Event{Status: statusPointer(e.status)})
	e.mu.Unlock()
	go e.monitor(runCtx, s)
	return nil
}

func (e *Engine) stopLocked() error {
	e.mu.Lock()
	s := e.run
	if s == nil {
		e.status = api.Status{State: api.Stopped, LastCapture: e.status.LastCapture}
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
	// Restore kernel routing before waiting for application relays or disk I/O.
	err := s.capture.Close()
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
	updates, _ := e.opts.WatchInterfaces(ctx)
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
			go e.failSession(s, err)
		}
	}
	for {
		select {
		case <-ctx.Done():
			return
		case <-leaseExpired:
			fail(errors.New("scoped capture test lease expired; routing was stopped"))
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
			s.router.Interfaces(ifaces)
			e.mu.Lock()
			s.ifaces = ifaces
			if e.run == s {
				e.status.Warnings = warnings(s)
				e.publishLocked(api.Event{Status: statusPointer(e.status)})
			}
			e.mu.Unlock()
		case now := <-ticker.C:
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
