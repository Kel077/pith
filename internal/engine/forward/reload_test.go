package forward

import (
	"context"
	"crypto/aes"
	"crypto/cipher"
	"encoding/hex"
	"errors"
	"io"
	"net"
	"net/netip"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Kel077/pith/internal/api"
	"github.com/Kel077/pith/internal/engine/attribution"
	"github.com/Kel077/pith/internal/engine/domains"
	"github.com/Kel077/pith/internal/rules"
	"gvisor.dev/gvisor/pkg/tcpip"
	"gvisor.dev/gvisor/pkg/tcpip/adapters/gonet"
	"gvisor.dev/gvisor/pkg/tcpip/network/ipv4"
)

func routeScene(iface string) api.Scene {
	return api.Scene{ID: "test", Rules: []api.Rule{{ID: "route", Enabled: true, Protocol: api.AnyProtocol, Action: api.Route, Via: []api.InterfaceRef{{Name: iface}}}}}
}

func reloadInterfaces(opts *Options) {
	opts.Router.Interfaces([]api.Interface{{Name: "en0", Up: true}, {Name: "example0", Up: true}, {Name: "example1", Up: true}})
	opts.UDPIdle = 2 * time.Second
}

func exchange(t *testing.T, conn net.Conn, message string) {
	t.Helper()
	conn.SetDeadline(time.Now().Add(2 * time.Second))
	if _, err := io.WriteString(conn, message); err != nil {
		t.Fatal(err)
	}
	buf := make([]byte, len(message))
	if _, err := io.ReadFull(conn, buf); err != nil || string(buf) != message {
		t.Fatal("relay exchange:", string(buf), err)
	}
}

func waitRecords(t *testing.T, x *fixtureStack, ready func([]api.Connection) bool) []api.Connection {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for {
		rows := x.store.Connections(api.ConnectionQuery{})
		if ready(rows) {
			return rows
		}
		if time.Now().After(deadline) {
			t.Fatal("connection records did not settle", rows)
		}
		time.Sleep(time.Millisecond)
	}
}

func waitNoFlows(t *testing.T, x *fixtureStack) {
	t.Helper()
	deadline := time.Now().Add(time.Second)
	for {
		x.f.mu.Lock()
		active := x.f.active
		x.f.mu.Unlock()
		if active == 0 {
			return
		}
		if time.Now().After(deadline) {
			t.Fatal("closed flow workers did not finish", active)
		}
		time.Sleep(time.Millisecond)
	}
}

func TestReloadKeepsSamePolicyIntent(t *testing.T) {
	scene := routeScene("example0")
	x := newFixture(t, echoDial(t), scene, reloadInterfaces)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	conn, err := x.dialTCP(ctx, "192.0.2.1:443")
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	exchange(t, conn, "GET / HTTP/1.1\r\nHost: example.com\r\n\r\n")
	initial := x.store.Connections(api.ConnectionQuery{})[0]
	scene.Rules[0].ID = "renamed"
	scene.Rules[0].Via[0].Label = "A different display label"
	scene.Rules[0].Via = append(scene.Rules[0].Via, api.InterfaceRef{Name: "example1"})
	scene.Rules[0].Fallback = true
	scene.Rules = append(scene.Rules, api.Rule{ID: "unrelated", Enabled: true, Protocol: api.TCP, Action: api.Block, Targets: []string{"203.0.113.0/24"}})
	if err := x.f.Reload(scene, api.DefaultSettings()); err != nil {
		t.Fatal(err)
	}
	exchange(t, conn, "same preferred interface")
	rows := x.store.Connections(api.ConnectionQuery{})
	if len(rows) != 1 || rows[0].ID != initial.ID || !rows[0].Ended.IsZero() || rows[0].Interface != "example0" {
		t.Fatal("an unrelated rule, ID, label or fallback change interrupted the flow", rows)
	}
}

func TestReloadResetsChangedTCPFlows(t *testing.T) {
	for _, change := range []string{"interface", "system", "domain block", "app block", "address block"} {
		t.Run(change, func(t *testing.T) {
			scene := routeScene("example0")
			x := newFixture(t, echoDial(t), scene, reloadInterfaces)
			ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			defer cancel()
			conn, err := x.dialTCP(ctx, "192.0.2.1:443")
			if err != nil {
				t.Fatal(err)
			}
			defer conn.Close()
			exchange(t, conn, "GET / HTTP/1.1\r\nHost: example.com\r\n\r\n")
			switch change {
			case "interface":
				scene.Rules[0].Via[0].Name = "example1"
			case "system":
				scene.Rules = nil
			default:
				block := api.Rule{ID: "block", Enabled: true, Protocol: api.TCP, Action: api.Block}
				switch change {
				case "domain block":
					block.Targets = []string{"example.com"}
				case "app block":
					block.Apps = []api.AppRef{{Path: "/Applications/Example.app"}}
				case "address block":
					block.Targets = []string{"192.0.2.0/24"}
				}
				scene.Rules = append(scene.Rules, block)
			}
			if err := x.f.Reload(scene, api.DefaultSettings()); err != nil {
				t.Fatal(err)
			}
			if _, err := conn.Read(make([]byte, 1)); err == nil || !strings.Contains(err.Error(), "reset") {
				t.Fatal("changed TCP flow did not reset", err)
			}
			rows := x.store.Connections(api.ConnectionQuery{})
			if len(rows) != 1 || rows[0].Ended.IsZero() || rows[0].EndReason != api.EndRulesChanged || rows[0].Problem != "" {
				t.Fatal("missing synchronous reload termination", rows)
			}
		})
	}
}

func TestReloadWhileSniffingCannotDialOldPolicy(t *testing.T) {
	var dials atomic.Int32
	x := newFixture(t, func(context.Context, string, string, string) (net.Conn, error) {
		dials.Add(1)
		return nil, errors.New("unexpected dial")
	}, api.Scene{ID: "test"})
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	conn, err := x.dialTCP(ctx, "192.0.2.1:443")
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	conn.SetDeadline(time.Now().Add(time.Second))
	// No payload is sent: the endpoint is in its bounded ClientHello wait.
	if err := x.f.Reload(api.Scene{ID: "test", Rules: []api.Rule{{ID: "block", Enabled: true, Protocol: api.TCP, Action: api.Block, Targets: []string{"example.com"}}}}, api.DefaultSettings()); err != nil {
		t.Fatal(err)
	}
	if _, err := conn.Read(make([]byte, 1)); err == nil || !strings.Contains(err.Error(), "reset") {
		t.Fatal("pending sniff was not reset", err)
	}
	waitNoFlows(t, x)
	rows := x.store.Connections(api.ConnectionQuery{})
	if len(rows) != 0 || dials.Load() != 0 {
		t.Fatal("pending sniff escaped the reload", rows, dials.Load())
	}
}

func TestReloadCancelsDialAndRejectsLateSocket(t *testing.T) {
	entered, released := make(chan struct{}), make(chan struct{})
	defer func() {
		select {
		case <-released:
		default:
			close(released)
		}
	}()
	a, b := net.Pipe()
	defer b.Close()
	defer a.Close()
	var canceled atomic.Bool
	x := newFixture(t, func(ctx context.Context, _, _, _ string) (net.Conn, error) {
		close(entered)
		<-ctx.Done()
		canceled.Store(true)
		<-released
		// Deliberately return success after cancellation to exercise publication.
		return tupleConn{a}, nil
	}, api.Scene{ID: "test"})
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	conn, err := x.dialTCP(ctx, "192.0.2.1:443")
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	conn.SetDeadline(time.Now().Add(time.Second))
	io.WriteString(conn, "GET / HTTP/1.1\r\nHost: example.com\r\n\r\n")
	select {
	case <-entered:
	case <-time.After(time.Second):
		t.Fatal("dial did not begin")
	}
	if err := x.f.Reload(api.Scene{ID: "test", Rules: []api.Rule{{ID: "block", Enabled: true, Protocol: api.TCP, Action: api.Block}}}, api.DefaultSettings()); err != nil {
		t.Fatal(err)
	}
	close(released)
	b.SetReadDeadline(time.Now().Add(time.Second))
	if n, err := b.Read(make([]byte, 1)); n != 0 || err == nil {
		t.Fatal("old dial forwarded buffered application data", n, err)
	}
	waitNoFlows(t, x)
	rows := x.store.Connections(api.ConnectionQuery{})
	if !canceled.Load() || len(rows) != 0 {
		t.Fatal("late socket survived cancellation", rows)
	}
}

func TestReloadUDPUsesNewInterfaceOnNextDatagram(t *testing.T) {
	scene := routeScene("example0")
	x := newFixture(t, echoDial(t), scene, reloadInterfaces)
	conn, err := gonet.DialUDP(x.app, nil, &tcpip.FullAddress{NIC: 1, Addr: tcpip.AddrFrom4([4]byte{192, 0, 2, 1}), Port: 443}, ipv4.ProtocolNumber)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	exchange(t, conn, "first datagram")
	if err := x.f.Reload(routeScene("example1"), api.DefaultSettings()); err != nil {
		t.Fatal(err)
	}
	exchange(t, conn, "second datagram")
	rows := x.store.Connections(api.ConnectionQuery{})
	if len(rows) != 2 || rows[0].Interface != "example1" || !rows[0].Ended.IsZero() || rows[1].Interface != "example0" || rows[1].Ended.IsZero() || rows[1].EndReason != api.EndRulesChanged || rows[1].Problem != "" {
		t.Fatal("UDP did not replace its registered tuple on reload", rows)
	}
}

func TestSameRouteIgnoresOnlyNonPrimaryChanges(t *testing.T) {
	base := rules.Decision{RuleID: "one", Action: api.Route, Via: []api.InterfaceRef{{Name: "example0", HardwareAddr: "02:aa:bb:cc:dd:01"}}}
	for _, tc := range []struct {
		name string
		next rules.Decision
		same bool
	}{
		{"rule ID", rules.Decision{RuleID: "two", Action: api.Route, Via: base.Via}, true},
		{"MAC lookup changes", rules.Decision{RuleID: "one", Action: api.Route, Via: []api.InterfaceRef{{Name: "example0", HardwareAddr: "02:AA:BB:CC:DD:01"}}}, false},
		{"block", rules.Decision{RuleID: "one", Action: api.Block}, false},
		{"system", rules.Decision{}, false},
		{"replacement adapter", rules.Decision{RuleID: "one", Action: api.Route, Via: []api.InterfaceRef{{Name: "example0", HardwareAddr: "02:00:00:00:00:02"}}}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := sameRoute(base, tc.next); got != tc.same {
				t.Fatal("sameRoute:", got)
			}
		})
	}
}

func quicHello(t *testing.T) []byte {
	t.Helper()
	// A synthetic TLS ClientHello containing only an example.com SNI, encrypted
	// with the public QUIC v1 Initial keys from RFC 9001 Appendix A.1.
	body := append([]byte{3, 3}, make([]byte, 32)...)
	body = append(body, 0, 0, 2, 0x13, 1, 1, 0)
	sni := append([]byte{0, 14, 0, 0, 11}, "example.com"...)
	body = append(body, 0, byte(len(sni)+4), 0, 0, 0, byte(len(sni)))
	body = append(body, sni...)
	hello := append([]byte{1, 0, 0, byte(len(body))}, body...)
	plain := append([]byte{6, 0, 0x40, byte(len(hello))}, hello...)
	cid, _ := hex.DecodeString("8394c8f03e515708")
	header := append([]byte{0xc3, 0, 0, 0, 1, 8}, cid...)
	header = append(header, 0, 0, 0x40, byte(4+len(plain)+16))
	pn := len(header)
	header = append(header, 0, 0, 0, 2)
	key, _ := hex.DecodeString("1f369613dd76d5467730efcbe3b1a22d")
	iv, _ := hex.DecodeString("fa044b2f42a3fd3b46fb255c")
	hp, _ := hex.DecodeString("9f50449e04a0e810283a1e9933adedd2")
	iv[len(iv)-1] ^= 2
	block, _ := aes.NewCipher(key)
	aead, _ := cipher.NewGCM(block)
	packet := aead.Seal(append([]byte(nil), header...), iv, plain, header)
	protect, _ := aes.NewCipher(hp)
	var mask [aes.BlockSize]byte
	protect.Encrypt(mask[:], packet[pn+4:pn+20])
	packet[0] ^= mask[0] & 15
	for i := range 4 {
		packet[pn+i] ^= mask[1+i]
	}
	if name := domains.QUIC(packet); name != "example.com" {
		t.Fatal("invalid synthetic QUIC Initial", name)
	}
	return packet
}

func TestReloadUDPQUICNameSurvivesSessionReplacement(t *testing.T) {
	for _, block := range []bool{false, true} {
		t.Run(map[bool]string{false: "new interface", true: "new block"}[block], func(t *testing.T) {
			scene := routeScene("example0")
			scene.Rules[0].Targets = []string{"example.com"}
			var dials atomic.Int32
			echo := echoDial(t)
			x := newFixture(t, func(ctx context.Context, iface, proto, address string) (net.Conn, error) {
				dials.Add(1)
				return echo(ctx, iface, proto, address)
			}, scene, reloadInterfaces)
			conn, err := gonet.DialUDP(x.app, nil, &tcpip.FullAddress{NIC: 1, Addr: tcpip.AddrFrom4([4]byte{192, 0, 2, 1}), Port: 443}, ipv4.ProtocolNumber)
			if err != nil {
				t.Fatal(err)
			}
			defer conn.Close()
			exchange(t, conn, string(quicHello(t)))
			if name := x.f.cache.Lookup(netip.MustParseAddr("192.0.2.1"), time.Now()); name != "" {
				t.Fatal("QUIC name leaked into the shared address cache", name)
			}
			scene.Rules[0].Via[0].Name = "example1"
			if block {
				scene.Rules = []api.Rule{{ID: "block", Enabled: true, Protocol: api.UDP, Action: api.Block, Targets: []string{"example.com"}}}
			}
			if err := x.f.Reload(scene, api.DefaultSettings()); err != nil {
				t.Fatal(err)
			}
			tail := string([]byte{0x40, 1, 2, 3, 4, 5})
			if !block {
				exchange(t, conn, tail)
			} else if _, err := io.WriteString(conn, tail); err != nil {
				t.Fatal(err)
			}
			rows := waitRecords(t, x, func(rows []api.Connection) bool { return len(rows) == 2 })
			if rows[0].Domain != "example.com" || rows[1].EndReason != api.EndRulesChanged || rows[1].Problem != "" {
				t.Fatal("QUIC continuation lost its tuple-specific domain", rows)
			}
			if block && (rows[0].Outcome != api.Blocked || dials.Load() != 1) {
				t.Fatal("QUIC short header escaped the new domain block", rows, dials.Load())
			}
			if !block && (rows[0].Interface != "example1" || dials.Load() != 2) {
				t.Fatal("QUIC short header fell back to the system route", rows, dials.Load())
			}
		})
	}
}

func TestReloadUDPRequiresFreshSocketLifetime(t *testing.T) {
	for _, gone := range []bool{false, true} {
		t.Run(map[bool]string{false: "reused port", true: "unknown socket"}[gone], func(t *testing.T) {
			scene := routeScene("example0")
			scene.Rules[0].Targets = []string{"example.com"}
			var fresh, dials atomic.Int32
			var changed atomic.Bool
			echo := echoDial(t)
			x := newFixture(t, func(ctx context.Context, iface, proto, address string) (net.Conn, error) {
				dials.Add(1)
				return echo(ctx, iface, proto, address)
			}, scene, reloadInterfaces, func(opts *Options) {
				cached := opts.Lookup
				opts.LookupFresh = func(ctx context.Context, proto api.Protocol, local, remote netip.AddrPort) (attribution.Attribution, error) {
					if !changed.Load() {
						return cached(ctx, proto, local, remote)
					}
					fresh.Add(1)
					if gone {
						return attribution.Attribution{}, attribution.ErrNotFound
					}
					a, err := cached(ctx, proto, local, remote)
					a.SocketGeneration++
					return a, err
				}
			})
			conn, err := gonet.DialUDP(x.app, nil, &tcpip.FullAddress{NIC: 1, Addr: tcpip.AddrFrom4([4]byte{192, 0, 2, 1}), Port: 443}, ipv4.ProtocolNumber)
			if err != nil {
				t.Fatal(err)
			}
			defer conn.Close()
			exchange(t, conn, string(quicHello(t)))
			changed.Store(true)
			scene.Rules = []api.Rule{{ID: "block", Enabled: true, Protocol: api.UDP, Action: api.Block, Targets: []string{"example.com"}}}
			if err := x.f.Reload(scene, api.DefaultSettings()); err != nil {
				t.Fatal(err)
			}
			tail := string([]byte{0x40, 1, 2, 3})
			if !gone {
				exchange(t, conn, tail)
				rows := x.store.Connections(api.ConnectionQuery{})
				if len(rows) != 2 || rows[0].Domain != "" || rows[0].Outcome != api.System || fresh.Load() != 1 {
					t.Fatal("a reused native port inherited the previous socket's name", rows, fresh.Load())
				}
				return
			}
			if _, err := io.WriteString(conn, tail); err != nil {
				t.Fatal(err)
			}
			deadline := time.Now().Add(time.Second)
			for fresh.Load() == 0 && time.Now().Before(deadline) {
				time.Sleep(time.Millisecond)
			}
			waitNoFlows(t, x)
			// Freeze the already-armed retry deadline so this burst deterministically
			// exercises admission throttling even under a paused race-test process.
			x.f.mu.Lock()
			x.f.udpFreshAt = time.Now().Add(time.Minute)
			x.f.mu.Unlock()
			for range 32 {
				if _, err := io.WriteString(conn, tail); err != nil {
					t.Fatal(err)
				}
			}
			time.Sleep(20 * time.Millisecond)
			if fresh.Load() != 1 || dials.Load() != 1 {
				t.Fatal("unknown UDP tails retried attribution per packet or escaped the hold", fresh.Load(), dials.Load())
			}
		})
	}
}

func TestReloadBlocksKnownRealFakeIPDestination(t *testing.T) {
	scene := routeScene("example0")
	x := newFixture(t, echoDial(t), scene, reloadInterfaces)
	x.router.Resolve = func(context.Context, api.Interface, string) ([]netip.Addr, error) {
		return []netip.Addr{netip.MustParseAddr("192.0.2.80")}, nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	conn, err := x.dialTCP(ctx, "198.18.0.7:443")
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	exchange(t, conn, "GET / HTTP/1.1\r\nHost: example.com\r\n\r\n")
	scene.Rules = append(scene.Rules, api.Rule{ID: "real-block", Enabled: true, Protocol: api.TCP, Action: api.Block, Targets: []string{"192.0.2.80"}})
	if err := x.f.Reload(scene, api.DefaultSettings()); err != nil {
		t.Fatal(err)
	}
	if _, err := conn.Read(make([]byte, 1)); err == nil || !strings.Contains(err.Error(), "reset") {
		t.Fatal("resolved destination escaped its new block", err)
	}
}

func TestInvalidReloadPreservesGenerationAndOpenFlow(t *testing.T) {
	x := newFixture(t, echoDial(t), routeScene("example0"), reloadInterfaces)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	conn, err := x.dialTCP(ctx, "192.0.2.1:443")
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	exchange(t, conn, "GET / HTTP/1.1\r\nHost: example.com\r\n\r\n")
	policy := x.router.Policy()
	invalid := routeScene("example1")
	invalid.Rules[0].ID = ""
	if err := x.f.Reload(invalid, api.DefaultSettings()); err == nil || x.router.Policy() != policy {
		t.Fatal("invalid reload published a generation", err)
	}
	exchange(t, conn, "still open")
}

func TestReloadDuringAttributionCannotRegisterAnOldFlow(t *testing.T) {
	entered, release := make(chan struct{}), make(chan struct{})
	defer func() {
		select {
		case <-release:
		default:
			close(release)
		}
	}()
	var dials atomic.Int32
	x := newFixture(t, func(context.Context, string, string, string) (net.Conn, error) {
		dials.Add(1)
		return nil, errors.New("unexpected dial")
	}, routeScene("example0"), reloadInterfaces, func(opts *Options) {
		lookup := opts.Lookup
		opts.Lookup = func(ctx context.Context, proto api.Protocol, local, remote netip.AddrPort) (attribution.Attribution, error) {
			close(entered)
			<-release
			return lookup(ctx, proto, local, remote)
		}
	})
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	dialed := make(chan error, 1)
	go func() {
		conn, err := x.dialTCP(ctx, "192.0.2.1:443")
		if conn != nil {
			conn.Close()
		}
		dialed <- err
	}()
	select {
	case <-entered:
	case <-ctx.Done():
		t.Fatal("attribution did not start")
	}
	if err := x.f.Reload(api.Scene{ID: "test", Rules: []api.Rule{{ID: "block", Enabled: true, Protocol: api.TCP, Action: api.Block}}}, api.DefaultSettings()); err != nil {
		t.Fatal(err)
	}
	close(release)
	select {
	case err := <-dialed:
		if err == nil {
			t.Fatal("old pending attribution accepted the SYN")
		}
	case <-ctx.Done():
		t.Fatal("revoked attribution did not finish")
	}
	waitNoFlows(t, x)
	rows := x.store.Connections(api.ConnectionQuery{})
	if dials.Load() != 0 || len(rows) != 0 {
		t.Fatal("old attribution escaped the generation check", rows, dials.Load())
	}
}

func TestReloadUDPContextIsBoundedWithoutEvictingLiveNames(t *testing.T) {
	scene := routeScene("example0")
	scene.Rules[0].Targets = []string{"example.com"}
	x := newFixture(t, echoDial(t), scene, reloadInterfaces, func(opts *Options) { opts.MaxFlows = 2 })
	conns := make([]net.Conn, 2)
	for i := range conns {
		conn, err := gonet.DialUDP(x.app, nil, &tcpip.FullAddress{NIC: 1, Addr: tcpip.AddrFrom4([4]byte{192, 0, 2, byte(i + 1)}), Port: 443}, ipv4.ProtocolNumber)
		if err != nil {
			t.Fatal(err)
		}
		defer conn.Close()
		conns[i] = conn
		exchange(t, conn, string(quicHello(t)))
	}
	if err := x.f.Reload(api.Scene{ID: "test", Rules: []api.Rule{{ID: "block", Enabled: true, Protocol: api.UDP, Action: api.Block, Targets: []string{"example.com"}}}}, api.DefaultSettings()); err != nil {
		t.Fatal(err)
	}
	waitNoFlows(t, x)
	x.f.mu.Lock()
	retained := len(x.f.udpReload)
	x.f.mu.Unlock()
	if retained != 2 {
		t.Fatal("lost retained UDP identity", retained)
	}
	// Capacity cannot evict either known domain and turn a following short header
	// into an unclassified system flow. The original tuple remains admissible.
	if fl := x.f.begin(api.UDP, netip.MustParseAddrPort("192.0.2.123:49999"), netip.MustParseAddrPort("192.0.2.3:443")); fl != nil {
		x.f.end(fl, &api.Connection{})
		t.Fatal("new UDP tuple evicted protected reload context")
	}
	if _, err := conns[0].Write([]byte{0x40, 1, 2, 3}); err != nil {
		t.Fatal(err)
	}
	rows := waitRecords(t, x, func(rows []api.Connection) bool { return len(rows) == 3 })
	if rows[0].Domain != "example.com" || rows[0].Outcome != api.Blocked {
		t.Fatal("capacity pressure lost the retained domain block", rows)
	}
}

func TestReloadUDPContextSurvivesSlowFreshLookup(t *testing.T) {
	scene := routeScene("example0")
	scene.Rules[0].Targets = []string{"example.com"}
	entered, release := make(chan struct{}), make(chan struct{})
	var changed atomic.Bool
	defer func() {
		select {
		case <-release:
		default:
			close(release)
		}
	}()
	x := newFixture(t, echoDial(t), scene, reloadInterfaces, func(opts *Options) {
		opts.MaxFlows = 2
		lookup := opts.Lookup
		opts.LookupFresh = func(ctx context.Context, proto api.Protocol, local, remote netip.AddrPort) (attribution.Attribution, error) {
			if !changed.Load() {
				return lookup(ctx, proto, local, remote)
			}
			close(entered)
			<-release
			return lookup(ctx, proto, local, remote)
		}
	})
	conn, err := gonet.DialUDP(x.app, nil, &tcpip.FullAddress{NIC: 1, Addr: tcpip.AddrFrom4([4]byte{192, 0, 2, 1}), Port: 443}, ipv4.ProtocolNumber)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	exchange(t, conn, string(quicHello(t)))
	changed.Store(true)
	if err := x.f.Reload(api.Scene{ID: "test", Rules: []api.Rule{{ID: "block", Enabled: true, Protocol: api.UDP, Action: api.Block, Targets: []string{"example.com"}}}}, api.DefaultSettings()); err != nil {
		t.Fatal(err)
	}
	waitNoFlows(t, x)
	if _, err := conn.Write([]byte{0x40, 1, 2, 3}); err != nil {
		t.Fatal(err)
	}
	select {
	case <-entered:
	case <-time.After(time.Second):
		t.Fatal("fresh lookup did not start")
	}
	x.f.mu.Lock()
	for _, saved := range x.f.udpReload {
		saved.until = time.Now().Add(-time.Hour)
	}
	x.f.mu.Unlock()
	if fl := x.f.begin(api.UDP, netip.MustParseAddrPort("192.0.2.123:49999"), netip.MustParseAddrPort("192.0.2.3:443")); fl != nil {
		x.f.end(fl, &api.Connection{})
		t.Fatal("capacity pruning removed a context while its lifetime check was pending")
	}
	close(release)
	rows := waitRecords(t, x, func(rows []api.Connection) bool { return len(rows) == 2 })
	if rows[0].Domain != "example.com" || rows[0].Outcome != api.Blocked {
		t.Fatal("slow lifetime evidence lost the domain and bypassed its block", rows)
	}
}
