package forward

import (
	"context"
	"errors"
	"io"
	"net"
	"net/netip"
	"strings"
	"testing"
	"time"

	"github.com/Kel077/pith/internal/api"
	"golang.org/x/net/dns/dnsmessage"
	"gvisor.dev/gvisor/pkg/tcpip"
	"gvisor.dev/gvisor/pkg/tcpip/adapters/gonet"
	"gvisor.dev/gvisor/pkg/tcpip/network/ipv4"
)

func cacheFakeDomain(t *testing.T, x *fixtureStack, address netip.Addr, domain string) {
	t.Helper()
	name := dnsmessage.MustNewName(domain + ".")
	question := dnsmessage.Question{Name: name, Type: dnsmessage.TypeA, Class: dnsmessage.ClassINET}
	query := dnsmessage.Message{Header: dnsmessage.Header{ID: 17}, Questions: []dnsmessage.Question{question}}
	response := dnsmessage.Message{Header: dnsmessage.Header{ID: 17, Response: true}, Questions: []dnsmessage.Question{question}, Answers: []dnsmessage.Resource{{
		Header: dnsmessage.ResourceHeader{Name: name, Type: dnsmessage.TypeA, Class: dnsmessage.ClassINET, TTL: 300},
		Body:   &dnsmessage.AResource{A: address.As4()},
	}}}
	q, err := query.Pack()
	if err != nil {
		t.Fatal(err)
	}
	r, err := response.Pack()
	if err != nil {
		t.Fatal(err)
	}
	x.f.cache.Observe(q, r, time.Now())
	if got := x.f.cache.Lookup(address, time.Now()); got != domain {
		t.Fatal("missing synthetic DNS mapping", got)
	}
}

func TestFakeIPRelayUsesObservedDomainForTCPAndUDP(t *testing.T) {
	for _, proto := range []api.Protocol{api.TCP, api.UDP} {
		t.Run(string(proto), func(t *testing.T) {
			fake := netip.MustParseAddrPort("198.18.0.7:443")
			real := netip.MustParseAddr("192.0.2.80")
			scene := api.Scene{ID: "test", Rules: []api.Rule{{ID: "route", Enabled: true, Protocol: proto, Action: api.Route, Targets: []string{"example.com"}, Via: []api.InterfaceRef{{Name: "en0"}}}}}
			echo := echoDial(t)
			dialed := make(chan string, 1)
			x := newFixture(t, func(ctx context.Context, name, network, target string) (net.Conn, error) {
				if name != "en0" || network != string(proto)+"4" || target != "192.0.2.80:443" {
					return nil, errors.New("forwarder sent the original fake IP to the physical interface")
				}
				dialed <- target
				return echo(ctx, name, network, target)
			}, scene)
			x.router.Resolve = func(_ context.Context, iface api.Interface, domain string) ([]netip.Addr, error) {
				if iface.Name != "en0" || domain != "example.com" {
					return nil, errors.New("forwarder lost the observed domain")
				}
				return []netip.Addr{real}, nil
			}
			ctx, cancel := context.WithTimeout(context.Background(), time.Second)
			defer cancel()
			var conn net.Conn
			var err error
			message := "GET / HTTP/1.1\r\nHost: example.com\r\n\r\n"
			if proto == api.TCP {
				conn, err = x.dialTCP(ctx, fake.String())
			} else {
				// UDP names may come from DNS even when this datagram is not QUIC.
				cacheFakeDomain(t, x, fake.Addr(), "example.com")
				conn, err = gonet.DialUDP(x.app, nil, &tcpip.FullAddress{NIC: 1, Addr: tcpip.AddrFromSlice(fake.Addr().AsSlice()), Port: fake.Port()}, ipv4.ProtocolNumber)
				message = "example UDP payload"
			}
			if err != nil {
				t.Fatal(err)
			}
			defer conn.Close()
			conn.SetDeadline(time.Now().Add(time.Second))
			if _, err := io.WriteString(conn, message); err != nil {
				t.Fatal(err)
			}
			buf := make([]byte, len(message))
			if _, err := io.ReadFull(conn, buf); err != nil || string(buf) != message {
				t.Fatal("fake-IP flow did not reach the real destination", string(buf), err)
			}
			select {
			case <-dialed:
			default:
				t.Fatal("no physical outbound connection")
			}
			rows := x.store.Connections(api.ConnectionQuery{})
			if len(rows) != 1 || rows[0].Domain != "example.com" || rows[0].Remote != fake.String() || rows[0].Outcome != api.Routed || rows[0].RuleID != "route" || rows[0].Problem != "" {
				t.Fatal("lost original connection identity or routing result", rows)
			}
		})
	}
}

func TestFakeIPUnknownDomainRecordsSystemBypass(t *testing.T) {
	for _, proto := range []api.Protocol{api.TCP, api.UDP} {
		t.Run(string(proto), func(t *testing.T) {
			fake := netip.MustParseAddrPort("198.18.0.7:443")
			// The requested physical interface differs from the fixture's saved
			// system route. Unknown mappings must preserve the latter regardless
			// of the rule's disabled fallback and report that fact in the log.
			scene := api.Scene{ID: "test", Rules: []api.Rule{{ID: "route", Enabled: true, Protocol: proto, Action: api.Route, Via: []api.InterfaceRef{{Name: "en2"}}}}}
			echo := echoDial(t)
			x := newFixture(t, func(ctx context.Context, name, network, target string) (net.Conn, error) {
				if name != "en0" || target != fake.String() {
					return nil, errors.New("unknown mapping did not retain its pre-capture system route")
				}
				return echo(ctx, name, network, target)
			}, scene)
			x.router.Resolve = func(context.Context, api.Interface, string) ([]netip.Addr, error) {
				return nil, errors.New("must not guess an unknown fake-IP domain")
			}
			ctx, cancel := context.WithTimeout(context.Background(), time.Second)
			defer cancel()
			var conn net.Conn
			var err error
			if proto == api.TCP {
				conn, err = x.dialTCP(ctx, fake.String())
			} else {
				conn, err = gonet.DialUDP(x.app, nil, &tcpip.FullAddress{NIC: 1, Addr: tcpip.AddrFromSlice(fake.Addr().AsSlice()), Port: fake.Port()}, ipv4.ProtocolNumber)
			}
			if err != nil {
				t.Fatal(err)
			}
			defer conn.Close()
			conn.SetDeadline(time.Now().Add(time.Second))
			message := "payload without a name"
			if _, err := io.WriteString(conn, message); err != nil {
				t.Fatal(err)
			}
			buf := make([]byte, len(message))
			if _, err := io.ReadFull(conn, buf); err != nil || string(buf) != message {
				t.Fatal(string(buf), err)
			}
			x.f.Close()
			rows := x.store.Connections(api.ConnectionQuery{})
			if len(rows) != 1 || rows[0].Outcome != api.System || rows[0].RuleID != "route" || rows[0].Domain != "" || !strings.Contains(rows[0].Problem, "no observed domain") || rows[0].Ended.IsZero() {
				t.Fatal("lost fake-IP bypass explanation when the flow ended", rows)
			}
		})
	}
}

func TestFakeIPRelayReloadCancelsPendingResolution(t *testing.T) {
	for _, proto := range []api.Protocol{api.TCP, api.UDP} {
		t.Run(string(proto), func(t *testing.T) {
			fake := netip.MustParseAddrPort("198.18.0.7:443")
			scene := api.Scene{ID: "test", Rules: []api.Rule{
				{ID: "route", Enabled: true, Protocol: proto, Action: api.Route, Targets: []string{"example.com"}, Via: []api.InterfaceRef{{Name: "en0"}}, Fallback: true},
				{ID: "real-block", Enabled: true, Protocol: proto, Action: api.Block, Targets: []string{"192.0.2.0/24"}, Apps: []api.AppRef{{Path: "/Applications/Example.app"}}},
			}}
			x := newFixture(t, func(context.Context, string, string, string) (net.Conn, error) {
				t.Error("relay dialed a resolved block or system fallback")
				return nil, errors.New("unexpected outbound connection")
			}, scene)
			original := x.router.Policy()
			x.router.Resolve = func(context.Context, api.Interface, string) ([]netip.Addr, error) {
				// The resolver may still finish after cancellation. Its immutable
				// snapshot must never publish a relay after the generation changes.
				scene.Rules[1].Enabled = false
				if err := x.f.Reload(scene, api.DefaultSettings()); err != nil {
					return nil, err
				}
				return []netip.Addr{netip.MustParseAddr("192.0.2.80")}, nil
			}
			ctx, cancel := context.WithTimeout(context.Background(), time.Second)
			defer cancel()
			var conn net.Conn
			var err error
			message := "GET / HTTP/1.1\r\nHost: example.com\r\n\r\n"
			if proto == api.TCP {
				conn, err = x.dialTCP(ctx, fake.String())
			} else {
				cacheFakeDomain(t, x, fake.Addr(), "example.com")
				conn, err = gonet.DialUDP(x.app, nil, &tcpip.FullAddress{NIC: 1, Addr: tcpip.AddrFromSlice(fake.Addr().AsSlice()), Port: fake.Port()}, ipv4.ProtocolNumber)
				message = "example UDP payload"
			}
			if err != nil {
				t.Fatal(err)
			}
			defer conn.Close()
			conn.SetDeadline(time.Now().Add(time.Second))
			if _, err := io.WriteString(conn, message); err != nil {
				t.Fatal(err)
			}
			if proto == api.TCP {
				if _, err := conn.Read(make([]byte, 1)); err == nil || !strings.Contains(err.Error(), "reset") {
					t.Fatal("blocked resolved TCP destination did not reset", err)
				}
			}
			deadline := time.Now().Add(time.Second)
			for x.router.Policy() == original && time.Now().Before(deadline) {
				time.Sleep(time.Millisecond)
			}
			if x.router.Policy() == original {
				t.Fatal("test did not replace the policy during resolution")
			}
			waitNoFlows(t, x)
			if rows := x.store.Connections(api.ConnectionQuery{}); len(rows) != 0 {
				t.Fatal("canceled unpublished resolution invented a connection failure", rows)
			}
		})
	}
}
