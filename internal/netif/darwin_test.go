//go:build darwin

package netif

import (
	"context"
	"errors"
	"net"
	"reflect"
	"sync"
	"testing"
	"time"

	"github.com/Kel077/pith/internal/api"
)

func TestLiveDiscoveryAndWatchCancellation(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	interfaces, err := List(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(interfaces) == 0 {
		t.Fatal("no interfaces")
	}
	for _, iface := range interfaces {
		if iface.Name == "" || iface.Label == "" || iface.Kind == "" {
			t.Fatal("incomplete interface")
		}
	}
	watchCtx, stop := context.WithCancel(ctx)
	events, err := Watch(watchCtx)
	if err != nil {
		stop()
		t.Fatal(err)
	}
	select {
	case <-events:
	case <-ctx.Done():
		stop()
		t.Fatal("missing initial snapshot")
	}
	select {
	case _, ok := <-events:
		if !ok {
			stop()
			t.Fatal("watch exited before cancellation")
		}
	case <-time.After(50 * time.Millisecond):
	}
	stop()
	select {
	case _, ok := <-events:
		if ok {
			t.Fatal("unexpected second snapshot")
		}
	case <-ctx.Done():
		t.Fatal("watch did not stop")
	}
}

// fakeList answers reads in turn; past its results, it repeats the last.
type fakeList struct {
	mu      sync.Mutex
	results []listResult
	calls   int
}

type listResult struct {
	ifs []api.Interface
	err error
}

func (f *fakeList) list(context.Context) ([]api.Interface, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	r := f.results[min(f.calls, len(f.results)-1)]
	f.calls++
	return r.ifs, r.err
}

// A read that fails after a change keeps the stream: the next one that works
// sends its snapshot. The stream ends with its watcher, after stopping it.
func TestFollowKeepsTheStreamThroughFailedReads(t *testing.T) {
	en0 := []api.Interface{{Name: "en0", Up: true}}
	both := []api.Interface{{Name: "en0", Up: true}, {Name: "en1", Up: true}}
	failed := errors.New("read routes: context deadline exceeded")
	f := &fakeList{results: []listResult{{ifs: en0}, {err: failed}, {err: failed}, {ifs: both}}}
	changes := make(chan struct{}, 1)
	stopped := make(chan struct{})
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	out, err := follow(ctx, changes, f.list, 20*time.Millisecond, func() { close(stopped) })
	if err != nil {
		t.Fatal(err)
	}
	if got := <-out; !reflect.DeepEqual(got, en0) {
		t.Fatal(got)
	}
	changes <- struct{}{}
	select {
	case got, ok := <-out:
		if !ok {
			t.Fatal("a failed read ended the stream")
		}
		if !reflect.DeepEqual(got, both) {
			t.Fatal(got)
		}
	case <-ctx.Done():
		t.Fatal("no snapshot after the failed reads")
	}
	close(changes)
	select {
	case _, ok := <-out:
		if ok {
			t.Fatal("a snapshot after the watcher ended")
		}
	case <-ctx.Done():
		t.Fatal("the stream outlived its watcher")
	}
	select {
	case <-stopped:
	default:
		t.Fatal("the stream ended before its watcher was stopped")
	}
}

// The first read is tried three times; when all fail, there is no stream.
func TestFollowTriesTheFirstReadThreeTimes(t *testing.T) {
	en0 := []api.Interface{{Name: "en0", Up: true}}
	failed := errors.New("read network configuration: signal: killed")
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	f := &fakeList{results: []listResult{{err: failed}, {err: failed}, {ifs: en0}}}
	out, err := follow(ctx, make(chan struct{}), f.list, 20*time.Millisecond, func() {})
	if err != nil {
		t.Fatal(err)
	}
	if got := <-out; !reflect.DeepEqual(got, en0) {
		t.Fatal(got)
	}
	f = &fakeList{results: []listResult{{err: failed}}}
	stopped := false
	if _, err := follow(ctx, make(chan struct{}), f.list, 20*time.Millisecond, func() { stopped = true }); !errors.Is(err, failed) || !stopped || f.calls != 3 {
		t.Fatalf("err %v, stopped %v, reads %d", err, stopped, f.calls)
	}
}

func TestBoundSocketOnLoopback(t *testing.T) {
	listener, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	dialer, err := Dialer("lo0", time.Second, nil)
	if err != nil {
		t.Fatal(err)
	}
	conn, err := dialer.DialContext(context.Background(), "tcp4", listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	conn.Close()
	if _, err := Dialer("pith-no-such-interface", time.Second, nil); err == nil {
		t.Fatal("unknown interface accepted")
	}
}
