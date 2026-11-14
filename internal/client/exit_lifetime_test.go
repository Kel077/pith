package client

import (
	"context"
	"errors"
	"net"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Kel077/pith/internal/api"
)

type exitClock struct{ nanos atomic.Int64 }

func newExitClock(c *Client) *exitClock {
	clock := &exitClock{}
	clock.nanos.Store(time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC).UnixNano())
	c.exitNow = func() time.Time { return time.Unix(0, clock.nanos.Load()) }
	return clock
}

func (c *exitClock) advance(d time.Duration) { c.nanos.Add(int64(d)) }

func TestExitPollLookupOutlivesItsStarter(t *testing.T) {
	c, n := exitClient(t)
	gate := make(chan struct{})
	open := sync.OnceFunc(func() { close(gate) })
	defer open()
	n.gate = gate
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { _, err := c.exitInfo(ctx, api.InterfaceRef{Name: "example0"}, -1); done <- err }()
	awaitExit(t, n.started)
	awaitExit(t, n.started)
	cancel()
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatal("the starter didn't stop waiting", err)
	}
	waiting := &observedWait{Context: context.Background(), waiting: make(chan struct{})}
	var info api.ProxyInfo
	var err error
	joined := make(chan struct{})
	go func() { defer close(joined); info, err = c.exitInfo(waiting, api.InterfaceRef{Name: "example0"}, -1) }()
	awaitExit(t, waiting.waiting)
	open()
	awaitExit(t, joined)
	if err != nil || info.PublicIPv4 != "198.51.100.9" || info.PublicIPv6 != "2001:db8::9" || n.tests() != [2]int{1, 1} {
		t.Fatal("a departing starter ended or repeated the lookup", info, err, n.tests())
	}
}

func TestExitPollRestartsACanceledInterfaceProbe(t *testing.T) {
	c, n := exitClient(t)
	links, err := c.Interfaces(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	c.exitCanChange(context.Background(), links[0])
	gate := make(chan struct{})
	open := sync.OnceFunc(func() { close(gate) })
	defer open()
	n.gate = gate
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { _, err := c.Probe(ctx, "example0", ""); done <- err }()
	awaitExit(t, n.started)
	awaitExit(t, n.started)
	waiting := &observedWait{Context: context.Background(), waiting: make(chan struct{})}
	joined := make(chan struct{})
	var info api.ProxyInfo
	go func() { defer close(joined); info, err = c.exitInfo(waiting, api.InterfaceRef{Name: "example0"}, -1) }()
	awaitExit(t, waiting.waiting)
	cancel()
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	awaitExit(t, n.started)
	awaitExit(t, n.started)
	open()
	awaitExit(t, joined)
	if err != nil || info.PublicIPv4 != "198.51.100.9" || info.PublicIPv6 != "2001:db8::9" || n.tests() != [2]int{2, 2} {
		t.Fatal("a poll inherited another caller's cancellation", info, err, n.tests())
	}
}

func TestExitPollFailureBackoff(t *testing.T) {
	for _, failure := range []string{"empty", "error", "timeout"} {
		t.Run(failure, func(t *testing.T) {
			c, n := exitClient(t)
			clock := newExitClock(c)
			var calls atomic.Int64
			base := c.dialer
			c.dialer = func(name string, d time.Duration, dns []string) (*net.Dialer, error) {
				calls.Add(1)
				if failure == "error" {
					return nil, errors.New("synthetic public-address test failure")
				}
				return base(name, d, dns)
			}
			n.public = [2]string{}
			if failure == "timeout" {
				c.probeTimeout = 5 * time.Millisecond
				n.gate = make(chan struct{}) // the server stops when the request is canceled
			}
			ref := api.InterfaceRef{Name: "example0"}
			for i, delay := range []time.Duration{30 * time.Second, time.Minute, 2 * time.Minute, 4 * time.Minute, 8 * time.Minute, 10 * time.Minute, 10 * time.Minute} {
				info, err := c.exitInfo(context.Background(), ref, -1)
				if err != nil || info.PublicIPv4 != "" || info.PublicIPv6 != "" || calls.Load() != int64(i+1) {
					t.Fatal("failure wasn't remembered", info, err, calls.Load())
				}
				c.exitMu.Lock()
				backoff := c.exits[ref.Name].backoff
				c.exitMu.Unlock()
				if backoff != delay {
					t.Fatalf("failure %d: backoff %v, want %v", i+1, backoff, delay)
				}
				clock.advance(delay - time.Nanosecond)
				if _, err := c.exitInfo(context.Background(), ref, -1); err != nil || calls.Load() != int64(i+1) {
					t.Fatal("poll retried before its backoff expired", err, calls.Load())
				}
				clock.advance(time.Nanosecond)
			}
		})
	}
}

func TestExitPollBackoffResets(t *testing.T) {
	c, n := exitClient(t)
	newExitClock(c)
	ref := api.InterfaceRef{Name: "example0"}
	n.public = [2]string{}
	for range 3 {
		if _, err := c.exitInfo(context.Background(), ref, 0); err != nil {
			t.Fatal(err)
		}
	}
	if n.tests() != [2]int{3, 3} {
		t.Fatal("explicit requests didn't bypass backoff", n.tests())
	}
	n.mu.Lock()
	n.public = [2]string{"198.51.100.9", "2001:db8::9"}
	n.mu.Unlock()
	if _, err := c.exitInfo(context.Background(), ref, 0); err != nil {
		t.Fatal(err)
	}
	c.exitMu.Lock()
	backoff := c.exits[ref.Name].backoff
	c.exitMu.Unlock()
	if backoff != 0 {
		t.Fatal("success didn't reset backoff", backoff)
	}
	for _, reset := range []string{"address change", "interface change"} {
		n.mu.Lock()
		n.public = [2]string{}
		n.mu.Unlock()
		if _, err := c.exitInfo(context.Background(), ref, 0); err != nil {
			t.Fatal(err)
		}
		if reset == "address change" {
			c.forgetExitAddresses(ref.Name)
		} else {
			n.mu.Lock()
			n.links[0].Router = "192.0.2.254"
			n.mu.Unlock()
		}
		before := n.tests()
		if _, err := c.exitInfo(context.Background(), ref, -1); err != nil || n.tests() != [2]int{before[0] + 1, before[1] + 1} {
			t.Fatal("a change didn't reset backoff", reset, err, n.tests())
		}
		c.exitMu.Lock()
		backoff = c.exits[ref.Name].backoff
		c.exitMu.Unlock()
		if backoff != exitFailureAge {
			t.Fatal("a change retained consecutive failures", reset, backoff)
		}
	}
}

func TestExitCloseCancelsDetachedLookup(t *testing.T) {
	c, n := exitClient(t)
	n.gate = make(chan struct{})
	done := make(chan struct{})
	go func() {
		defer close(done)
		_, _ = c.exitInfo(context.Background(), api.InterfaceRef{Name: "example0"}, -1)
	}()
	awaitExit(t, n.started)
	awaitExit(t, n.started)
	closed := make(chan struct{})
	go func() { defer close(closed); _ = c.Close() }()
	select {
	case <-closed:
	case <-time.After(time.Second):
		t.Fatal("Close didn't cancel the detached lookup promptly")
	}
	awaitExit(t, done)
	c.exitJobs.Wait()
	c.exitMu.Lock()
	state := c.exits["example0"]
	defer c.exitMu.Unlock()
	if state.lookup != nil || !state.failed.IsZero() || publicIPs(state.public) != 0 {
		t.Fatal("Close left a lookup running or remembered its cancellation", state)
	}
}
