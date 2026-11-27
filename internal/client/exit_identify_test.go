//go:build darwin

package client

import (
	"context"
	"errors"
	"net"
	"net/http"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Kel077/pith/internal/api"
	"github.com/Kel077/pith/internal/router/routertest"
)

func identifyingExit(t *testing.T, handler http.Handler) (*Client, api.Interface) {
	t.Helper()
	c, n := exitClient(t)
	n.links[0].Kind, n.links[0].Router = api.KindEthernet, "127.0.0.1"
	srv := serveRouter(t, handler, "127.0.0.1:0")
	_, c.routerPort, _ = net.SplitHostPort(srv.Listener.Addr().String())
	links, err := c.Interfaces(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	return c, links[0]
}

func TestExitIdentificationOutlivesItsStarter(t *testing.T) {
	f := routertest.New("203.0.113.10")
	gate, started := make(chan struct{}), make(chan struct{}, 4)
	open := sync.OnceFunc(func() { close(gate) })
	defer open()
	var calls atomic.Int64
	c, link := identifyingExit(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		started <- struct{}{}
		select {
		case <-gate:
			f.ServeHTTP(w, r)
		case <-r.Context().Done():
		}
	}))
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan bool, 1)
	go func() { done <- c.exitCanChange(ctx, link) }()
	awaitExit(t, started)
	cancel()
	if <-done {
		t.Fatal("a canceled reader didn't leave")
	}
	waiting := &observedWait{Context: context.Background(), waiting: make(chan struct{})}
	go func() { done <- c.exitCanChange(waiting, link) }()
	awaitExit(t, waiting.waiting)
	open()
	if !<-done || calls.Load() != 1 {
		t.Fatal("identification was canceled or repeated", calls.Load())
	}
}

func TestExitIdentificationRetriesTransientFailures(t *testing.T) {
	f := routertest.New("203.0.113.10")
	c, link := identifyingExit(t, f)
	clock := newExitClock(c)
	base := c.dialer
	var calls atomic.Int64
	c.dialer = func(name string, d time.Duration, dns []string) (*net.Dialer, error) {
		if calls.Add(1) == 1 {
			// Not a refused connection, which tells a router Pith doesn't know.
			return nil, errors.New("synthetic temporary failure")
		}
		return base(name, d, dns)
	}
	if c.exitCanChange(context.Background(), link) || calls.Load() != 1 {
		t.Fatal("transient identification failure wasn't reported")
	}
	clock.advance(time.Minute - time.Nanosecond)
	if c.exitCanChange(context.Background(), link) || calls.Load() != 1 {
		t.Fatal("identification retried before a minute")
	}
	clock.advance(time.Nanosecond)
	if !c.exitCanChange(context.Background(), link) || calls.Load() != 2 {
		t.Fatal("identification didn't recover after a minute", calls.Load())
	}
	clock.advance(time.Hour)
	if !c.exitCanChange(context.Background(), link) || calls.Load() != 2 {
		t.Fatal("successful identification wasn't cached", calls.Load())
	}
}

func TestExitIdentificationKeepsUnsupportedDevices(t *testing.T) {
	f := routertest.New("203.0.113.10")
	f.Model = "CR1000A"
	var calls atomic.Int64
	c, link := identifyingExit(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		f.ServeHTTP(w, r)
	}))
	clock := newExitClock(c)
	for range 4 {
		if c.exitCanChange(context.Background(), link) {
			t.Fatal("unsupported router can change addresses")
		}
		clock.advance(24 * time.Hour)
	}
	if calls.Load() != 1 {
		t.Fatal("unsupported router was identified again", calls.Load())
	}
}

func TestExitCloseCancelsDetachedIdentification(t *testing.T) {
	started, ended := make(chan struct{}), make(chan struct{})
	c, link := identifyingExit(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		close(started)
		<-r.Context().Done()
		close(ended)
	}))
	done := make(chan struct{})
	go func() { defer close(done); c.exitCanChange(context.Background(), link) }()
	awaitExit(t, started)
	closed := make(chan struct{})
	go func() { defer close(closed); _ = c.Close() }()
	select {
	case <-closed:
	case <-time.After(time.Second):
		t.Fatal("Close didn't cancel identification promptly")
	}
	awaitExit(t, done)
	awaitExit(t, ended)
	c.exitJobs.Wait()
}

func TestExitInfoIdentifiesAndLooksUpInParallel(t *testing.T) {
	c, n := exitClient(t)
	n.links[0].Kind, n.links[0].Router = api.KindEthernet, "127.0.0.1"
	probeGate, deviceGate := make(chan struct{}), make(chan struct{})
	openProbe := sync.OnceFunc(func() { close(probeGate) })
	openDevice := sync.OnceFunc(func() { close(deviceGate) })
	defer openProbe()
	defer openDevice()
	n.gate = probeGate
	deviceStarted := make(chan struct{})
	mark := sync.OnceFunc(func() { close(deviceStarted) })
	f := routertest.New("203.0.113.10")
	srv := serveRouter(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mark()
		select {
		case <-deviceGate:
			f.ServeHTTP(w, r)
		case <-r.Context().Done():
		}
	}), "127.0.0.1:0")
	_, c.routerPort, _ = net.SplitHostPort(srv.Listener.Addr().String())
	c.keepExitConfig(api.Config{Aliases: map[string]string{exampleExit.HardwareAddr: "Desk"}})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan struct{})
	var info api.ProxyInfo
	var err error
	go func() { defer close(done); info, err = c.exitInfo(ctx, api.InterfaceRef{Name: "example0"}, -1) }()
	awaitExit(t, deviceStarted)
	deadline := time.NewTimer(time.Second)
	defer deadline.Stop()
	for range 2 {
		select {
		case <-n.started:
		case <-deadline.C:
			t.Fatal("the public-address test waited for identification")
		}
	}
	c.exitMu.Lock()
	lookup, identifying := c.exits["example0"].lookup, c.exits["example0"].capability
	c.exitMu.Unlock()
	openProbe()
	awaitExit(t, lookup.done)
	cancel()
	awaitExit(t, done)
	if !errors.Is(err, context.Canceled) || info.Interface != "Desk" || !info.Online || info.Changing ||
		info.PublicIPv4 != "198.51.100.9" || info.PublicIPv6 != "2001:db8::9" || info.At.IsZero() {
		t.Fatal("a canceled status read lost what the client knew", info, err)
	}
	select {
	case <-identifying.done:
		t.Fatal("the status reader canceled identification")
	default:
	}
	openDevice()
	awaitExit(t, identifying.done)
}

func TestExitInfoReturnsKnownStateWithErrors(t *testing.T) {
	for _, up := range []bool{false, true} {
		t.Run(map[bool]string{false: "down", true: "up"}[up], func(t *testing.T) {
			c, n := exitClient(t)
			n.links[0].Up = up
			ref := api.InterfaceRef{Name: "example0", HardwareAddr: exampleExit.HardwareAddr}
			c.keepExitConfig(api.Config{Aliases: map[string]string{ref.Key(): "Desk"}})
			c.exitChanges = map[string]*exitChange{ref.Name: {link: n.links[0], done: make(chan struct{})}}
			defer delete(c.exitChanges, ref.Name)
			ctx, cancel := context.WithCancel(context.Background())
			cancel()
			info, err := c.exitInfo(ctx, ref, -1)
			if !errors.Is(err, context.Canceled) || info.Interface != "Desk" || info.Online != up || !info.Changing {
				t.Fatal("an expired status read fabricated or lost interface state", info, err)
			}
			if n.tests() != [2]int{} {
				t.Fatal("an expired status read tested an interface", n.tests())
			}
		})
	}
	t.Run("unreadable inventory", func(t *testing.T) {
		c, _ := exitClient(t)
		links, err := c.Interfaces(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		ref := api.InterfaceRef{Name: "example0", HardwareAddr: exampleExit.HardwareAddr}
		c.keepExitConfig(api.Config{Aliases: map[string]string{ref.Key(): "Desk"}})
		c.exitChanges = map[string]*exitChange{ref.Name: {link: links[0], done: make(chan struct{})}}
		defer delete(c.exitChanges, ref.Name)
		failure := errors.New("synthetic inventory failure")
		c.interfaces = func(context.Context) ([]api.Interface, error) { return nil, failure }
		info, err := c.exitInfo(context.Background(), ref, -1)
		if !errors.Is(err, failure) || info.Interface != "Desk" || info.Online || !info.Changing {
			t.Fatal("an unreadable inventory was reported online", info, err)
		}
	})
}
