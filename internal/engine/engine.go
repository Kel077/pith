// Package engine implements the privileged helper backend. Construction is
// idle; only Start creates an interface or installs capture routes.
package engine

import (
	"context"
	"errors"
	"fmt"
	"net/netip"
	"path/filepath"
	"slices"
	"sync"
	"time"

	"github.com/Kel077/pith/internal/api"
	"github.com/Kel077/pith/internal/engine/capture"
	"github.com/Kel077/pith/internal/engine/echo"
	"github.com/Kel077/pith/internal/engine/egress"
	"github.com/Kel077/pith/internal/engine/forward"
	"github.com/Kel077/pith/internal/engine/passthrough"
	"github.com/Kel077/pith/internal/engine/telemetry"
	"github.com/Kel077/pith/internal/netif"
)

type Options struct {
	DataDir         string
	Scopes          []netip.Prefix // development: capture only these prefixes
	ProbeAddress    string         // empty: api.ipify.org:443; IP requires ProbeServerName
	ProbeServerName string
	// Test seams below never relax the production helper's root requirement.
	Driver          capture.Driver
	WatchRoutes     func(context.Context) (<-chan capture.Change, error)
	Interfaces      func(context.Context) ([]api.Interface, error)
	WatchInterfaces func(context.Context) (<-chan []api.Interface, error)
	Dial            egress.DialFunc
	Lookup          forward.LookupFunc
	EchoExchange    echo.ExchangeFunc
	ExistingSockets passthrough.SnapshotFunc
	RawSender       passthrough.Sender
	Verify          func(context.Context, *capture.Capture, *forward.Forwarder) error
}

type Engine struct {
	opts        Options
	store       *telemetry.Store
	lifecycle   sync.Mutex
	mu          sync.Mutex
	status      api.Status
	run         *session
	subs        map[chan api.Event]struct{}
	done        chan struct{}
	closed      bool
	scopedLease time.Duration
}

func New(opts Options) (*Engine, error) {
	if !filepath.IsAbs(opts.DataDir) {
		return nil, fmt.Errorf("engine data directory must be absolute")
	}
	opts.Scopes = slices.Clone(opts.Scopes)
	if opts.Interfaces == nil {
		opts.Interfaces = netif.List
	}
	if opts.WatchInterfaces == nil {
		opts.WatchInterfaces = netif.Watch
	}
	if opts.WatchRoutes == nil {
		opts.WatchRoutes = capture.Watch
	}
	store, err := telemetry.Open(telemetry.Options{Dir: opts.DataDir, LogDays: 7})
	if err != nil {
		return nil, err
	}
	if err := capture.Recover(filepath.Join(opts.DataDir, "capture.json"), opts.Driver); err != nil {
		return nil, err
	}
	e := &Engine{opts: opts, store: store, status: api.Status{State: api.Stopped}, subs: make(map[chan api.Event]struct{}), done: make(chan struct{})}
	if len(opts.Scopes) > 0 {
		// A development controller can disappear without sending Stop. Never
		// leave scoped test capture running indefinitely in the idle daemon.
		e.scopedLease = 5 * time.Minute
	}
	return e, nil
}

func validate(scene api.Scene, settings api.Settings) error {
	return (api.Config{Version: api.ConfigVersion, Scenes: []api.Scene{scene}, Selected: scene.ID, Settings: settings}).Validate()
}

func (e *Engine) Status(ctx context.Context) (api.Status, error) {
	if err := ctx.Err(); err != nil {
		return api.Status{}, err
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	return cloneStatus(e.status), nil
}
func cloneStatus(s api.Status) api.Status {
	s.Warnings = slices.Clone(s.Warnings)
	if s.LastCapture != nil {
		identity := *s.LastCapture
		identity.Prefixes = slices.Clone(identity.Prefixes)
		s.LastCapture = &identity
	}
	if s.Error != nil {
		copy := *s.Error
		s.Error = &copy
	}
	return s
}
func (e *Engine) state(s api.Status) {
	e.mu.Lock()
	if s.LastCapture == nil {
		s.LastCapture = e.status.LastCapture
	}
	e.status = s
	e.publishLocked(api.Event{Status: statusPointer(s)})
	e.mu.Unlock()
}
func statusPointer(s api.Status) *api.Status { copy := cloneStatus(s); return &copy }

func (e *Engine) Start(ctx context.Context, scene api.Scene, settings api.Settings) error {
	if err := validate(scene, settings); err != nil {
		return err
	}
	e.lifecycle.Lock()
	defer e.lifecycle.Unlock()
	if e.closed {
		return errors.New("engine is closed")
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if e.run != nil && e.run.settings.IPv6 == settings.IPv6 {
		return e.reloadLocked(scene, settings)
	}
	if e.run != nil {
		if err := e.stopLocked(); err != nil {
			return err
		}
	}
	return e.startLocked(ctx, scene, settings)
}

func (e *Engine) Reload(ctx context.Context, scene api.Scene, settings api.Settings) error {
	if err := validate(scene, settings); err != nil {
		return err
	}
	e.lifecycle.Lock()
	defer e.lifecycle.Unlock()
	if err := ctx.Err(); err != nil {
		return err
	}
	if e.closed {
		return errors.New("engine is closed")
	}
	if e.run == nil {
		return nil
	}
	if e.run.settings.IPv6 != settings.IPv6 {
		if err := e.stopLocked(); err != nil {
			return err
		}
		return e.startLocked(ctx, scene, settings)
	}
	return e.reloadLocked(scene, settings)
}

func (e *Engine) reloadLocked(scene api.Scene, settings api.Settings) error {
	if err := e.store.SetLogDays(settings.LogDays); err != nil {
		return err
	}
	if err := e.run.router.Reload(scene, settings); err != nil {
		return err
	}
	e.mu.Lock()
	e.run.scene, e.run.settings = scene, settings
	e.status.SceneID = scene.ID
	e.status.Warnings = warnings(e.run)
	e.publishLocked(api.Event{Status: statusPointer(e.status)})
	e.mu.Unlock()
	return nil
}

func (e *Engine) Stop(context.Context) error {
	e.lifecycle.Lock()
	defer e.lifecycle.Unlock()
	return e.stopLocked()
}

func (e *Engine) Close() error {
	e.lifecycle.Lock()
	defer e.lifecycle.Unlock()
	if e.closed {
		return nil
	}
	e.closed = true
	err := e.stopLocked()
	e.mu.Lock()
	close(e.done)
	for ch := range e.subs {
		close(ch)
		delete(e.subs, ch)
	}
	e.mu.Unlock()
	return err
}

func (e *Engine) Connections(ctx context.Context, q api.ConnectionQuery) ([]api.Connection, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return e.store.Connections(q), nil
}
func (e *Engine) ClearConnections(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	return e.store.Clear()
}
func (e *Engine) Traffic(ctx context.Context, q api.TrafficQuery) (api.Traffic, error) {
	if err := ctx.Err(); err != nil {
		return api.Traffic{}, err
	}
	return e.store.Traffic(q), nil
}

func (e *Engine) Subscribe(ctx context.Context) (<-chan api.Event, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	select {
	case <-e.done:
		return nil, errors.New("engine is closed")
	default:
	}
	ch := make(chan api.Event, 128)
	e.subs[ch] = struct{}{}
	ch <- api.Event{Status: statusPointer(e.status)}
	go func() {
		select {
		case <-ctx.Done():
		case <-e.done:
		}
		e.mu.Lock()
		defer e.mu.Unlock()
		if _, ok := e.subs[ch]; ok {
			delete(e.subs, ch)
			close(ch)
		}
	}()
	return ch, nil
}

func (e *Engine) publishLocked(event api.Event) {
	for ch := range e.subs {
		select {
		case ch <- event:
		default:
			close(ch)
			delete(e.subs, ch)
		}
	}
}

func errorValue(err error) *api.Error {
	var value *api.Error
	if errors.As(err, &value) {
		copy := *value
		return &copy
	}
	return &api.Error{Code: api.ErrInternal, Detail: err.Error()}
}
