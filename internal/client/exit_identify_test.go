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
			return nil, &net.OpError{Op: "dial", Net: "tcp", Err: errors.New("synthetic temporary failure")}
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
