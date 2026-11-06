package forward

import (
	"context"
	"errors"
	"io"
	"net"
	"net/netip"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Kel077/pith/internal/api"
	"github.com/Kel077/pith/internal/engine/attribution"
	"github.com/Kel077/pith/internal/engine/telemetry"
	"golang.org/x/net/dns/dnsmessage"
	"gvisor.dev/gvisor/pkg/tcpip"
	"gvisor.dev/gvisor/pkg/tcpip/adapters/gonet"
	"gvisor.dev/gvisor/pkg/tcpip/network/ipv4"
)

func TestReloadUDPInitialLifetimeMustBeFresh(t *testing.T) {
	scene := routeScene("example0")
	scene.Rules[0].Targets = []string{"example.com"}
	var fresh, cached atomic.Int32
	x := newFixture(t, echoDial(t), scene, reloadInterfaces, func(opts *Options) {
		lookup := opts.Lookup
		opts.Lookup = func(ctx context.Context, proto api.Protocol, local, remote netip.AddrPort) (attribution.Attribution, error) {
			cached.Add(1)
			a, err := lookup(ctx, proto, local, remote)
			a.SocketGeneration = 1 // previous socket still in the PCB cache
			return a, err
		}
		opts.LookupFresh = func(ctx context.Context, proto api.Protocol, local, remote netip.AddrPort) (attribution.Attribution, error) {
			fresh.Add(1)
			a, err := lookup(ctx, proto, local, remote)
			a.SocketGeneration = 2 // current socket before and after Reload
			return a, err
		}
	})
	conn, err := gonet.DialUDP(x.app, nil, &tcpip.FullAddress{NIC: 1, Addr: tcpip.AddrFrom4([4]byte{192, 0, 2, 1}), Port: 443}, ipv4.ProtocolNumber)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	exchange(t, conn, string(quicHello(t)))
	if err := x.f.Reload(api.Scene{ID: "test", Rules: []api.Rule{{ID: "block", Enabled: true, Protocol: api.UDP, Action: api.Block, Targets: []string{"example.com"}}}}, api.DefaultSettings()); err != nil {
		t.Fatal(err)
	}
	conn.Write([]byte{0x40, 1, 2, 3})
	rows := waitRecords(t, x, func(rows []api.Connection) bool { return len(rows) == 2 })
	if cached.Load() != 0 || fresh.Load() != 2 || rows[0].Domain != "example.com" || rows[0].Outcome != api.Blocked {
		t.Fatal("cached initial generation let a short header escape its new block", rows, cached.Load(), fresh.Load())
	}
}

func TestReloadCanceledUDPWorkerCannotRemoveSuccessorContext(t *testing.T) {
	x := newFixture(t, echoDial(t), api.Scene{ID: "test"})
	local, remote := netip.MustParseAddrPort("192.0.2.123:50000"), netip.MustParseAddrPort("192.0.2.1:443")
	key := [2]netip.AddrPort{local, remote}
	a := attribution.Attribution{App: api.AppRef{Path: "/Applications/Example.app"}, SocketGeneration: 1}
	x.f.mu.Lock()
	old := &udpReloadContext{owner: a, verified: true, domain: "example.com", until: time.Now().Add(time.Minute)}
	x.f.udpReload[key] = old
	x.f.mu.Unlock()
	fl := x.f.begin(api.UDP, local, remote)
	if fl == nil || x.f.udpLookup(fl) == nil {
		t.Fatal("old reconstruction did not start")
	}
	defer x.f.end(fl, &api.Connection{})
	if err := x.f.Reload(api.Scene{ID: "second"}, api.DefaultSettings()); err != nil {
		t.Fatal(err)
	}
	x.f.mu.Lock()
	successor := &udpReloadContext{owner: a, verified: true, domain: "later.example.com", until: time.Now().Add(time.Minute)}
	x.f.udpReload[key] = successor
	x.f.mu.Unlock()
	a.SocketGeneration++
	if _, ok := x.f.udpDomain(fl, a, nil, "", "", false); ok {
		t.Fatal("revoked worker was allowed to update UDP identity")
	}
	x.f.mu.Lock()
	got := x.f.udpReload[key]
	x.f.mu.Unlock()
	if got != successor {
		t.Fatal("revoked worker removed the successor's domain protection")
	}
	// Even a current worker holding a superseded pointer cannot delete a newer
	// record for the same tuple.
	fl2 := x.f.begin(api.UDP, local, remote)
	if fl2 == nil {
		t.Fatal("successor flow could not register")
	}
	defer x.f.end(fl2, &api.Connection{})
	x.f.mu.Lock()
	fl2.resume, fl2.fresh = old, true
	old.users++
	x.f.mu.Unlock()
	if _, ok := x.f.udpDomain(fl2, a, nil, "example.com", "example.com", false); ok {
		t.Fatal("a stale context pointer replaced current protection")
	}
	x.f.mu.Lock()
	got = x.f.udpReload[key]
	x.f.mu.Unlock()
	if got != successor {
		t.Fatal("stale worker mutated the replacement context")
	}
}

func TestReloadOnlyCurrentUDPWorkerSuppliesRetainedName(t *testing.T) {
	x := newFixture(t, echoDial(t), api.Scene{ID: "test"})
	local, remote := netip.MustParseAddrPort("192.0.2.123:50000"), netip.MustParseAddrPort("192.0.2.1:443")
	old := x.f.begin(api.UDP, local, remote)
	next := x.f.begin(api.UDP, local, remote)
	if old == nil || next == nil {
		t.Fatal("could not model the endpoint-close/finalization overlap")
	}
	defer x.f.end(old, &api.Connection{})
	defer x.f.end(next, &api.Connection{})
	x.f.mu.Lock()
	old.domain, old.verified = "old.example.com", true
	next.domain, next.verified = "new.example.com", true
	x.f.mu.Unlock()
	if err := x.f.Reload(api.Scene{ID: "next"}, api.DefaultSettings()); err != nil {
		t.Fatal(err)
	}
	x.f.mu.Lock()
	saved := x.f.udpReload[[2]netip.AddrPort{local, remote}]
	x.f.mu.Unlock()
	if saved == nil || saved.domain != "new.example.com" {
		t.Fatal("old worker supplied stale domain evidence for its successor", saved)
	}
}

func TestReloadPendingUDPDoesNotPoisonRetry(t *testing.T) {
	for _, port := range []uint16{53, 443} {
		t.Run(map[uint16]string{53: "DNS", 443: "QUIC"}[port], func(t *testing.T) {
			entered := make(chan struct{})
			var calls atomic.Int32
			x := newFixture(t, echoDial(t), routeScene("example0"), reloadInterfaces, func(opts *Options) {
				opts.DNSIdle = 2 * time.Second
				lookup := opts.Lookup
				opts.LookupFresh = func(ctx context.Context, proto api.Protocol, local, remote netip.AddrPort) (attribution.Attribution, error) {
					if calls.Add(1) == 1 {
						close(entered)
						<-ctx.Done()
						return attribution.Attribution{}, ctx.Err()
					}
					return lookup(ctx, proto, local, remote)
				}
			})
			conn, err := gonet.DialUDP(x.app, nil, &tcpip.FullAddress{NIC: 1, Addr: tcpip.AddrFrom4([4]byte{192, 0, 2, 1}), Port: port}, ipv4.ProtocolNumber)
			if err != nil {
				t.Fatal(err)
			}
			defer conn.Close()
			message := string(reloadDNSQuery(t))
			if port == 443 {
				message = string(quicHello(t))
			}
			io.WriteString(conn, message)
			select {
			case <-entered:
			case <-time.After(time.Second):
				t.Fatal("initial attribution did not begin")
			}
			if err := x.f.Reload(routeScene("example1"), api.DefaultSettings()); err != nil {
				t.Fatal(err)
			}
			waitNoFlows(t, x)
			exchange(t, conn, message)
			rows := x.store.Connections(api.ConnectionQuery{})
			if len(rows) != 1 || rows[0].Interface != "example1" || rows[0].Outcome != api.Routed {
				t.Fatal("canceled pending UDP poisoned the next complete request", rows)
			}
		})
	}
}

func reloadDNSQuery(t *testing.T) []byte {
	t.Helper()
	message := dnsmessage.Message{Header: dnsmessage.Header{ID: 7, RecursionDesired: true}, Questions: []dnsmessage.Question{{Name: dnsmessage.MustNewName("example.com."), Type: dnsmessage.TypeA, Class: dnsmessage.ClassINET}}}
	packet, err := message.Pack()
	if err != nil {
		t.Fatal(err)
	}
	return packet
}

func TestReloadUnclassifiedQUICCannotLosePendingInitial(t *testing.T) {
	for _, tc := range []struct {
		name, window string
		port         uint16
	}{
		{"lookup", "lookup", 443},
		{"classification", "classification", 443},
		{"lookup on DNS port", "lookup", 53},
		{"classification on DNS port", "classification", 53},
	} {
		window := tc.window
		t.Run(tc.name, func(t *testing.T) {
			entered := make(chan struct{})
			var lookups, classifications, dials atomic.Int32
			x := newFixture(t, func(context.Context, string, string, string) (net.Conn, error) {
				dials.Add(1)
				return nil, errors.New("unclassified tail escaped the reload guard")
			}, api.Scene{ID: "test"}, reloadInterfaces, func(opts *Options) {
				opts.DNSIdle = 2 * time.Second
				lookup := opts.Lookup
				opts.LookupFresh = func(ctx context.Context, proto api.Protocol, local, remote netip.AddrPort) (attribution.Attribution, error) {
					if lookups.Add(1) == 1 && window == "lookup" {
						close(entered)
						<-ctx.Done()
						return attribution.Attribution{}, ctx.Err()
					}
					return lookup(ctx, proto, local, remote)
				}
				if window == "classification" {
					opts.beforeUDPClassify = func(ctx context.Context) {
						if classifications.Add(1) == 1 {
							close(entered)
							<-ctx.Done()
						}
					}
				}
			})
			conn, err := gonet.DialUDP(x.app, nil, &tcpip.FullAddress{NIC: 1, Addr: tcpip.AddrFrom4([4]byte{192, 0, 2, 1}), Port: tc.port}, ipv4.ProtocolNumber)
			if err != nil {
				t.Fatal(err)
			}
			defer conn.Close()
			conn.Write(quicHello(t))
			select {
			case <-entered:
			case <-time.After(time.Second):
				t.Fatal("Initial did not reach the pending classification window")
			}
			block := api.Scene{ID: "test", Rules: []api.Rule{{ID: "block", Enabled: true, Protocol: api.UDP, Action: api.Block, Targets: []string{"example.com"}}}}
			if err := x.f.Reload(block, api.DefaultSettings()); err != nil {
				t.Fatal(err)
			}
			waitNoFlows(t, x)
			conn.Write([]byte{0x40, 1, 2, 3})
			deadline := time.Now().Add(time.Second)
			for lookups.Load() < 2 && time.Now().Before(deadline) {
				time.Sleep(time.Millisecond)
			}
			waitNoFlows(t, x)
			if rows := x.store.Connections(api.ConnectionQuery{}); dials.Load() != 0 || len(rows) != 0 {
				t.Fatal("pending Initial lost its domain block to a fresh but nameless tail", dials.Load(), rows)
			}
			x.f.mu.Lock()
			guarded := len(x.f.udpReload) == 1
			for _, saved := range x.f.udpReload {
				guarded = guarded && saved.unclassified
			}
			x.f.udpFreshAt = time.Time{}
			x.f.mu.Unlock()
			if !guarded || lookups.Load() != 2 {
				t.Fatal("pending tuple lost its unclassified protection", guarded, lookups.Load())
			}
			// A complete replacement Initial can recover its own name and is
			// judged by the new block instead of being held forever with gen zero.
			conn.Write(quicHello(t))
			rows := waitRecords(t, x, func(rows []api.Connection) bool { return len(rows) == 1 })
			if rows[0].Domain != "example.com" || rows[0].Outcome != api.Blocked || dials.Load() != 0 {
				t.Fatal("complete Initial failed to reclassify the unknown predecessor", rows)
			}
			waitNoFlows(t, x)
			x.f.mu.Lock()
			x.f.udpFreshAt = time.Time{}
			x.f.mu.Unlock()
			conn.Write([]byte{0x40, 1, 2, 3})
			rows = waitRecords(t, x, func(rows []api.Connection) bool { return len(rows) == 2 })
			if rows[0].Domain != "example.com" || rows[0].Outcome != api.Blocked || dials.Load() != 0 {
				t.Fatal("replacement classification failed to protect the following tail", rows)
			}
		})
	}
}

func TestPendingDNSRecoveryRequiresCompleteQuery(t *testing.T) {
	query := reloadDNSQuery(t)
	var edns dnsmessage.Message
	if err := edns.Unpack(query); err != nil {
		t.Fatal(err)
	}
	edns.Additionals = []dnsmessage.Resource{{Header: dnsmessage.ResourceHeader{Name: dnsmessage.MustNewName("."), Type: dnsmessage.TypeOPT, Class: dnsmessage.Class(1232)}, Body: &dnsmessage.OPTResource{Options: []dnsmessage.Option{{Code: 12, Data: []byte{0, 0}}}}}}
	withEDNS, err := edns.Pack()
	if err != nil {
		t.Fatal(err)
	}
	response := append([]byte(nil), query...)
	response[2] |= 0x80
	for _, tc := range []struct {
		name  string
		data  []byte
		valid bool
	}{
		{"query", query, true},
		{"EDNS query", withEDNS, true},
		{"opaque payload", []byte("DNS retry"), false},
		{"short header", []byte{0x40, 1, 2, 3}, false},
		{"response", response, false},
		{"incomplete query", query[:len(query)-1], false},
		{"trailing opaque data", append(append([]byte(nil), query...), 0x40, 1, 2, 3), false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := completeDNSRequest(tc.data); got != tc.valid {
				t.Fatal("incorrect pending-DNS recovery classification", got)
			}
		})
	}
}

func TestReloadUDPUnverifiedNameRequiresCurrentInitial(t *testing.T) {
	scene := routeScene("example0")
	scene.Rules[0].Targets = []string{"example.com"}
	var calls atomic.Int32
	x := newFixture(t, echoDial(t), scene, reloadInterfaces, func(opts *Options) {
		lookup := opts.Lookup
		opts.LookupFresh = func(ctx context.Context, proto api.Protocol, local, remote netip.AddrPort) (attribution.Attribution, error) {
			a, err := lookup(ctx, proto, local, remote)
			if calls.Add(1) == 1 {
				// The name is observed, but its old socket lifetime was uncertain.
				a.SocketGeneration = 0
				return a, attribution.ErrAmbiguous
			}
			a.SocketGeneration = 2
			return a, err
		}
	})
	conn, err := gonet.DialUDP(x.app, nil, &tcpip.FullAddress{NIC: 1, Addr: tcpip.AddrFrom4([4]byte{192, 0, 2, 1}), Port: 443}, ipv4.ProtocolNumber)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	exchange(t, conn, string(quicHello(t)))
	scene.Rules = []api.Rule{{ID: "block", Enabled: true, Protocol: api.UDP, Action: api.Block, Targets: []string{"example.com"}}}
	if err := x.f.Reload(scene, api.DefaultSettings()); err != nil {
		t.Fatal(err)
	}
	conn.Write([]byte{0x40, 1, 2, 3})
	deadline := time.Now().Add(time.Second)
	for calls.Load() < 2 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	waitNoFlows(t, x)
	if rows := x.store.Connections(api.ConnectionQuery{}); len(rows) != 1 {
		t.Fatal("unverified anchor let a short header lose its old name", rows)
	}
	x.f.mu.Lock()
	x.f.udpFreshAt = time.Time{}
	x.f.mu.Unlock()
	conn.Write(quicHello(t))
	rows := waitRecords(t, x, func(rows []api.Connection) bool { return len(rows) == 2 })
	if rows[0].Outcome != api.Blocked || rows[0].Domain != "example.com" {
		t.Fatal("current complete Initial could not reclassify an unverified predecessor", rows)
	}
	waitNoFlows(t, x)
	x.f.mu.Lock()
	x.f.udpFreshAt = time.Time{}
	x.f.mu.Unlock()
	conn.Write([]byte{0x40, 1, 2, 3})
	rows = waitRecords(t, x, func(rows []api.Connection) bool { return len(rows) == 3 })
	if rows[0].Outcome != api.Blocked || rows[0].Domain != "example.com" {
		t.Fatal("a blocked replacement Initial discarded protection for its tail", rows)
	}
}

func TestReloadAbortsHalfOpenTCPBeforeAndAfterRegistration(t *testing.T) {
	for _, before := range []bool{false, true} {
		t.Run(map[bool]string{false: "registered", true: "registration gap"}[before], func(t *testing.T) {
			entered, release := make(chan struct{}), make(chan struct{})
			defer func() {
				select {
				case <-release:
				default:
					close(release)
				}
			}()
			var once sync.Once
			var hold atomic.Bool
			x := newFixture(t, echoDial(t), routeScene("example0"), reloadInterfaces, func(opts *Options) {
				if before {
					opts.beforeTCPHandshake = func() {
						if hold.Load() {
							once.Do(func() { close(entered); <-release })
						}
					}
				}
			})
			ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			defer cancel()
			stable, err := x.dialTCP(ctx, "192.0.2.1:443")
			if err != nil {
				t.Fatal(err)
			}
			defer stable.Close()
			exchange(t, stable, "GET / HTTP/1.1\r\nHost: example.com\r\n\r\n")
			hold.Store(true)
			x.device.drop.Store(true) // discard SYN-ACK, so no final ACK can arrive
			done := make(chan struct{})
			pendingCtx, pendingCancel := context.WithCancel(ctx)
			defer pendingCancel()
			go func() {
				defer close(done)
				conn, _ := x.dialTCP(pendingCtx, "192.0.2.2:443")
				if conn != nil {
					conn.Close()
				}
			}()
			if before {
				select {
				case <-entered:
				case <-ctx.Done():
					t.Fatal("handshake did not reach registration gap")
				}
			} else {
				deadline := time.Now().Add(time.Second)
				for len(x.f.stack.RegisteredEndpoints()) < 2 && time.Now().Before(deadline) {
					time.Sleep(time.Millisecond)
				}
			}
			scene := routeScene("example0")
			scene.Rules = append(scene.Rules, api.Rule{ID: "block-pending", Enabled: true, Protocol: api.TCP, Action: api.Block, Targets: []string{"192.0.2.2"}})
			if err := x.f.Reload(scene, api.DefaultSettings()); err != nil {
				t.Fatal(err)
			}
			if before {
				close(release)
			}
			deadline := time.Now().Add(time.Second)
			for {
				x.f.mu.Lock()
				active := x.f.active
				x.f.mu.Unlock()
				if active == 1 {
					break
				}
				if time.Now().After(deadline) {
					t.Fatal("revoked half-open handshake retained capacity", active)
				}
				time.Sleep(time.Millisecond)
			}
			pendingCancel()
			<-done
			x.device.drop.Store(false)
			exchange(t, stable, "unrelated flow is still open")
		})
	}
}

type stalledStore struct {
	*telemetry.Store
	entered chan struct{}
	release chan struct{}
	after   bool
}

type stalledEndStore struct {
	*telemetry.Store
	entered chan struct{}
	release chan struct{}
	once    sync.Once
}

func (s *stalledEndStore) EndWithReason(id uint64, problem string, reason api.EndReason) {
	if reason == api.EndRulesChanged {
		s.once.Do(func() { close(s.entered) })
		<-s.release
	}
	s.Store.EndWithReason(id, problem, reason)
}

func TestReloadClosesEverySocketBeforeWaitingForTelemetry(t *testing.T) {
	var stalled *stalledEndStore
	peers := make(chan net.Conn, 2)
	x := newFixture(t, func(context.Context, string, string, string) (net.Conn, error) {
		a, b := net.Pipe()
		peers <- b
		t.Cleanup(func() { a.Close(); b.Close() })
		return tupleConn{a}, nil
	}, api.Scene{ID: "test"}, func(opts *Options) {
		stalled = &stalledEndStore{Store: opts.Store.(*telemetry.Store), entered: make(chan struct{}), release: make(chan struct{})}
		opts.Store = stalled
	})
	var released sync.Once
	release := func() { released.Do(func() { close(stalled.release) }) }
	defer release()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	var outgoing []net.Conn
	for _, remote := range []string{"192.0.2.1:443", "192.0.2.2:443"} {
		conn, err := x.dialTCP(ctx, remote)
		if err != nil {
			t.Fatal(err)
		}
		defer conn.Close()
		request := "GET / HTTP/1.1\r\nHost: example.com\r\n\r\n"
		io.WriteString(conn, request)
		select {
		case peer := <-peers:
			peer.SetReadDeadline(time.Now().Add(time.Second))
			if _, err := io.ReadFull(peer, make([]byte, len(request))); err != nil {
				t.Fatal(err)
			}
			outgoing = append(outgoing, peer)
		case <-ctx.Done():
			t.Fatal("outbound socket did not open")
		}
	}
	done := make(chan error, 1)
	go func() {
		done <- x.f.Reload(api.Scene{ID: "test", Rules: []api.Rule{{ID: "block", Enabled: true, Protocol: api.TCP, Action: api.Block}}}, api.DefaultSettings())
	}()
	defer func() {
		release()
		select {
		case err := <-done:
			if err != nil {
				t.Error(err)
			}
		case <-time.After(time.Second):
			t.Error("reload did not finish after telemetry resumed")
		}
	}()
	select {
	case <-stalled.entered:
	case <-ctx.Done():
		t.Fatal("reload did not reach terminal telemetry")
	}
	for _, out := range outgoing {
		out.SetReadDeadline(time.Now().Add(time.Second))
		if n, err := out.Read(make([]byte, 1)); n != 0 || !errors.Is(err, io.EOF) {
			t.Fatal("one slow terminal record delayed another affected socket's closure", n, err)
		}
	}
	// The deferred release also unblocks the relay workers before fixture cleanup.
}

func (s *stalledStore) AddContext(ctx context.Context, c api.Connection) uint64 {
	var id uint64
	if s.after {
		id = s.Store.AddContext(ctx, c)
	}
	close(s.entered)
	<-s.release
	if s.after {
		return id
	}
	return s.Store.AddContext(ctx, c)
}

func TestReloadDoesNotHoldRegistryWhilePublishingTelemetry(t *testing.T) {
	for _, after := range []bool{false, true} {
		t.Run(map[bool]string{false: "before record", true: "late publication"}[after], func(t *testing.T) {
			var stalled *stalledStore
			peer := make(chan net.Conn, 1)
			x := newFixture(t, func(context.Context, string, string, string) (net.Conn, error) {
				a, b := net.Pipe()
				peer <- b
				t.Cleanup(func() { a.Close(); b.Close() })
				return tupleConn{a}, nil
			}, api.Scene{ID: "test"}, func(opts *Options) {
				stalled = &stalledStore{Store: opts.Store.(*telemetry.Store), entered: make(chan struct{}), release: make(chan struct{}), after: after}
				opts.Store = stalled
			})
			defer close(stalled.release)
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			defer cancel()
			conn, err := x.dialTCP(ctx, "192.0.2.1:443")
			if err != nil {
				t.Fatal(err)
			}
			defer conn.Close()
			io.WriteString(conn, "GET / HTTP/1.1\r\nHost: example.com\r\n\r\n")
			select {
			case <-stalled.entered:
			case <-ctx.Done():
				t.Fatal("publication did not start")
			}
			reloaded := make(chan error, 1)
			go func() {
				reloaded <- x.f.Reload(api.Scene{ID: "test", Rules: []api.Rule{{ID: "block", Enabled: true, Protocol: api.TCP, Action: api.Block}}}, api.DefaultSettings())
			}()
			select {
			case err := <-reloaded:
				if err != nil {
					t.Fatal(err)
				}
			case <-time.After(time.Second):
				t.Fatal("telemetry publication held the global flow registry")
			}
			out := <-peer
			out.SetReadDeadline(time.Now().Add(time.Second))
			if n, err := out.Read(make([]byte, 1)); n != 0 || !errors.Is(err, io.EOF) {
				t.Fatal("pending telemetry delayed socket closure or forwarded data", n, err)
			}
			stalled.release <- struct{}{}
			waitNoFlows(t, x)
			rows := x.store.Connections(api.ConnectionQuery{})
			if !after && len(rows) != 0 {
				t.Fatal("canceled unpublished socket created a record", rows)
			}
			if after && (len(rows) != 1 || rows[0].Ended.IsZero() || rows[0].Outcome != api.System || rows[0].EndReason != api.EndRulesChanged || rows[0].Problem != "") {
				t.Fatal("late telemetry publication was not finalized exactly once", rows)
			}
		})
	}
}
