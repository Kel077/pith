package engine

import (
	"context"
	"errors"
	"net"
	"net/netip"
	"slices"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Kel077/pith/internal/api"
	"github.com/Kel077/pith/internal/engine/capture"
	"github.com/Kel077/pith/internal/engine/egress"
	"github.com/Kel077/pith/internal/engine/forward"
	"github.com/Kel077/pith/internal/rules"
)

func startTopology(t *testing.T) (*Engine, *testDriver) {
	t.Helper()
	e, d := newTestEngine(t, func(context.Context, *capture.Capture, *forward.Forwarder) error { return nil })
	e.opts.Scopes = nil // Full capture exists only in this memory driver.
	d.routes = append(d.routes, capture.Route{Prefix: netip.MustParsePrefix("192.0.2.0/24"), Interface: "utun50", Index: 50}, capture.Route{Prefix: netip.MustParsePrefix("2001:db8:50::/48"), Interface: "utun50", Index: 50})
	cfg := api.DefaultConfig()
	cfg.Settings.IPv6 = true
	if err := e.Start(context.Background(), cfg.Scenes[0], cfg.Settings); err != nil {
		t.Fatal(err)
	}
	return e, d
}

func waitTopologyStop(t *testing.T, e *Engine, d *testDriver) {
	t.Helper()
	// One polling interval, ownership observation and post-close verification,
	// plus scheduling margin. The owned device closes before final verification.
	deadline := time.After(7 * time.Second)
	for {
		status, _ := e.Status(context.Background())
		if status.State == api.Failed {
			if status.Error == nil || status.Error.Code != api.ErrNetworkLost {
				t.Fatal(status)
			}
			routes, _ := d.Snapshot()
			if slices.ContainsFunc(routes, func(r capture.Route) bool { return r.Index == 42 }) {
				t.Fatal("capture survived fault", routes)
			}
			select {
			case <-d.devices[0].done:
			default:
				t.Fatal("device remained open")
			}
			return
		}
		select {
		case <-deadline:
			t.Fatal("topology fault did not stop capture")
		case <-time.After(5 * time.Millisecond):
		}
	}
}

func TestTopologySnapshotFaultsStopWithoutRewritingExternalRoutes(t *testing.T) {
	for _, mode := range []string{"capture-removed", "capture-replaced", "capture-duplicate", "shadow-route", "snapshot-error"} {
		t.Run(mode, func(t *testing.T) {
			e, d := startTopology(t)
			d.mu.Lock()
			switch mode {
			case "capture-removed":
				d.routes = slices.DeleteFunc(d.routes, func(r capture.Route) bool { return r.Prefix == netip.MustParsePrefix("1.0.0.0/8") })
			case "capture-replaced":
				for i := range d.routes {
					if d.routes[i].Prefix == netip.MustParsePrefix("1.0.0.0/8") {
						d.routes[i].Interface = "utun51"
						d.routes[i].Index = 51
					}
				}
			case "capture-duplicate":
				d.routes = append(d.routes, d.routes[len(d.routes)-1])
			case "shadow-route":
				d.routes = append(d.routes, capture.Route{Prefix: netip.MustParsePrefix("198.51.100.0/24"), Interface: "utun51", Index: 51})
			case "snapshot-error":
				d.snapshotErr = errors.New("test observation failed")
			}
			want := slices.DeleteFunc(slices.Clone(d.routes), func(r capture.Route) bool { return r.Index == 42 })
			d.mu.Unlock()
			waitTopologyStop(t, e, d)
			d.mu.Lock()
			d.snapshotErr = nil
			d.mu.Unlock()
			got, _ := d.Snapshot()
			if !slices.Equal(got, want) {
				t.Fatal("cleanup edited external topology", got, want)
			}
		})
	}
}

func TestCaptureEventsAndOwnershipObserverFailureStop(t *testing.T) {
	for _, event := range []capture.Change{{Index: 42, Prefix: netip.MustParsePrefix("1.0.0.0/8")}, {Err: errors.New("route observer overflow")}} {
		t.Run("event", func(t *testing.T) { e, d := startTopology(t); d.changes <- event; waitTopologyStop(t, e, d) })
	}
}

func TestVPNWithdrawalPacketsNeverReachRulesOrNativeRelays(t *testing.T) {
	for _, family := range []int{4, 6} {
		for _, proto := range []byte{1, 6, 17, 58} {
			t.Run(netip.AddrFrom4([4]byte{192, 0, 2, proto}).String(), func(t *testing.T) {
				e, d := startTopology(t)
				// No route notification has arrived and the stored route still exists.
				// Even an explicit physical route with fallback cannot leak this packet.
				scene := api.Scene{ID: "guard", Rules: []api.Rule{{ID: "physical", Enabled: true, Action: api.Route, Protocol: api.AnyProtocol, Via: []api.InterfaceRef{{Name: "en0"}}, Fallback: true}}}
				settings := api.DefaultConfig().Settings
				settings.IPv6 = true
				if err := e.Reload(context.Background(), scene, settings); err != nil {
					t.Fatal(err)
				}
				packet := make([]byte, 40)
				if family == 4 {
					packet[0] = 0x45
					packet[9] = proto
					copy(packet[16:20], netip.MustParseAddr("192.0.2.8").AsSlice())
				} else {
					packet[0] = 0x60
					packet[6] = proto
					copy(packet[24:40], netip.MustParseAddr("2001:db8:50::8").AsSlice())
				}
				if !e.run.guard.Packet(packet) {
					t.Fatal("VPN packet admitted")
				}
				d.devices[len(d.devices)-1].packets <- packet
				if e.run.guard.Err() != nil {
					t.Fatal("VPN withdrawal revoked capture")
				}
			})
		}
	}
}

func TestTopologyDialCancellationHasNoFallback(t *testing.T) {
	e, d := startTopology(t)
	r := e.run.router
	started := make(chan struct{})
	var calls atomic.Int32
	// Install the seam before any synthetic flow is sent.
	// Use the router's guarded context through a standalone router sharing its guard.
	guard := r.Guard
	router := egress.New(e.run.capture.Plan.Before, []api.Interface{{Name: "en0", Up: true}}, func(ctx context.Context, _, _, _ string) (net.Conn, error) {
		calls.Add(1)
		close(started)
		<-ctx.Done()
		return nil, ctx.Err()
	})
	router.Guard = guard
	result := make(chan error, 1)
	go func() {
		_, err := router.Dial(context.Background(), rules.Decision{RuleID: "via", Via: []api.InterfaceRef{{Name: "en0"}}, Fallback: true}, api.TCP, netip.MustParseAddrPort("203.0.113.9:443"))
		result <- err
	}()
	<-started
	d.changes <- capture.Change{Index: 42, Prefix: netip.MustParsePrefix("1.0.0.0/8")}
	select {
	case err := <-result:
		if err == nil {
			t.Fatal("dial survived generation revocation")
		}
	case <-time.After(time.Second):
		t.Fatal("pending dial was not canceled")
	}
	if calls.Load() != 1 {
		t.Fatal("topology failure fell back", calls.Load())
	}
	waitTopologyStop(t, e, d)
}

func TestTopologyClosesEstablishedConnectionsAndRejectsLateDialResults(t *testing.T) {
	for _, late := range []bool{false, true} {
		t.Run("connection", func(t *testing.T) {
			e, d := startTopology(t)
			guard := e.run.guard
			client, peer := net.Pipe()
			defer peer.Close()
			router := egress.New(e.run.capture.Plan.Before, []api.Interface{{Name: "en0", Up: true}}, func(context.Context, string, string, string) (net.Conn, error) {
				if late {
					d.mu.Lock()
					d.indices = map[string]int{"en0": 99}
					d.mu.Unlock()
				}
				return client, nil
			})
			router.Guard = guard
			result, err := router.Dial(context.Background(), rules.Decision{}, api.TCP, netip.MustParseAddrPort("203.0.113.9:443"))
			if late {
				if err == nil || result.Conn != nil {
					t.Fatal("accepted dial on replaced interface")
				}
			} else {
				if err != nil {
					t.Fatal(err)
				}
				defer result.Conn.Close()
				d.changes <- capture.Change{Index: 42, Prefix: netip.MustParsePrefix("1.0.0.0/8")}
			}
			peer.SetReadDeadline(time.Now().Add(time.Second))
			if _, err := peer.Read(make([]byte, 1)); err == nil {
				t.Fatal("connection stayed usable")
			} else if timeout, ok := err.(net.Error); ok && timeout.Timeout() {
				t.Fatal("connection was not closed")
			}
			if !late {
				waitTopologyStop(t, e, d)
			} else if guard.Err() != nil {
				t.Fatal("late dial revoked capture")
			}
		})
	}
}

func TestObserverSetupFailureDoesNotCreateCapture(t *testing.T) {
	e, d := newTestEngine(t, nil)
	e.opts.WatchRoutes = func(context.Context) (<-chan capture.Change, error) { return nil, errors.New("observer unavailable") }
	cfg := api.DefaultConfig()
	if e.Start(context.Background(), cfg.Scenes[0], cfg.Settings) == nil || len(d.devices) != 0 {
		t.Fatal("started without route observation")
	}
}

func TestClosedInterfaceObserverFallsBackToPolling(t *testing.T) {
	e, _ := newTestEngine(t, nil)
	var polls atomic.Int32
	read := e.opts.Interfaces
	e.opts.Interfaces = func(ctx context.Context) ([]api.Interface, error) { polls.Add(1); return read(ctx) }
	e.opts.WatchInterfaces = func(context.Context) (<-chan []api.Interface, error) {
		ch := make(chan []api.Interface)
		close(ch)
		return ch, nil
	}
	cfg := api.DefaultConfig()
	if err := e.Start(context.Background(), cfg.Scenes[0], cfg.Settings); err != nil {
		t.Fatal(err)
	}
	deadline := time.After(3 * time.Second)
	for polls.Load() < 2 {
		select {
		case <-deadline:
			t.Fatal("metadata poll did not recover")
		case <-time.After(5 * time.Millisecond):
		}
	}
	status, _ := e.Status(context.Background())
	if status.State != api.Running {
		t.Fatal(status)
	}
}

func TestStableGenerationAndScopedVPNAdmission(t *testing.T) {
	e, d := newTestEngine(t, func(context.Context, *capture.Capture, *forward.Forwarder) error { return nil })
	d.routes = append(d.routes, capture.Route{Prefix: netip.MustParsePrefix("192.0.2.0/24"), Interface: "utun50", Index: 50})
	cfg := api.DefaultConfig()
	if err := e.Start(context.Background(), cfg.Scenes[0], cfg.Settings); err != nil {
		t.Fatal(err)
	}
	g := e.run.guard
	if err := g.Admit(netip.MustParseAddr("192.0.2.10")); err != nil {
		t.Fatal("rejected deliberate narrower scope", err)
	}
	d.mu.Lock()
	slices.Reverse(d.routes)
	d.mu.Unlock()
	if err := g.Validate(true); err != nil {
		t.Fatal("route order changed generation", err)
	}
	if err := g.Interfaces([]api.Interface{{Name: "en0", Label: "Renamed service", Up: true}}); err != nil {
		t.Fatal("label changed generation", err)
	}
	if err := g.Interfaces([]api.Interface{{Name: "en0", Up: false}}); err != nil {
		t.Fatal("down interface stopped routing", err)
	}
	if _, err := g.Binding("en0"); err == nil {
		t.Fatal("bound a down interface")
	}
}
