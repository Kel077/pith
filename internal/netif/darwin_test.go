//go:build darwin

package netif

import (
	"context"
	"net"
	"testing"
	"time"
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
