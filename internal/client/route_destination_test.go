package client

import (
	"context"
	"errors"
	"net"
	"net/netip"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/Kel077/pith/internal/api"
	"github.com/Kel077/pith/internal/proxy"
	"github.com/Kel077/pith/internal/proxy/egress"
	"golang.org/x/net/dns/dnsmessage"
)

// Keep the test transport on loopback while representing a remote proxy target.
// The proxy's post-connect guard must observe that target, not the test server.
type proxyPeerConn struct{ *net.TCPConn }

func (*proxyPeerConn) RemoteAddr() net.Addr {
	return &net.TCPAddr{IP: net.ParseIP("203.0.113.80"), Port: 443}
}

func dialProxyPeer(ctx context.Context, address string) (net.Conn, error) {
	conn, err := (&net.Dialer{}).DialContext(ctx, "tcp4", address)
	if err != nil {
		return nil, err
	}
	return &proxyPeerConn{conn.(*net.TCPConn)}, nil
}

func routeOwnAddresses() ([]net.Addr, error) {
	return []net.Addr{
		&net.IPAddr{IP: net.ParseIP("198.51.100.7")},
		&net.IPAddr{IP: net.ParseIP("2001:db8::7")},
	}, nil
}

// The actual Go resolver receives synthetic DNS answers over loopback. The
// socket control callback always returns an error, even if the guard regresses:
// no test can connect to the documentation addresses it resolves.
func TestSceneSystemExitChecksResolvedOwnAddresses(t *testing.T) {
	for _, mode := range []string{"unmatched-name", "rule-fallback", "address-rules", "literal"} {
		t.Run(mode, func(t *testing.T) {
			server, err := net.ListenPacket("udp4", "127.0.0.1:0")
			if err != nil {
				t.Fatal(err)
			}
			var queries atomic.Int64
			done := make(chan struct{})
			go func() {
				defer close(done)
				var buf [1500]byte
				for {
					n, peer, err := server.ReadFrom(buf[:])
					if err != nil {
						return
					}
					var q dnsmessage.Message
					if err := q.Unpack(buf[:n]); err != nil || len(q.Questions) != 1 {
						t.Error("invalid test DNS question", err)
						return
					}
					queries.Add(1)
					question := q.Questions[0]
					r := dnsmessage.Message{Header: dnsmessage.Header{ID: q.ID, Response: true, RecursionAvailable: true}, Questions: q.Questions}
					var body dnsmessage.ResourceBody
					switch question.Type {
					case dnsmessage.TypeA:
						body = &dnsmessage.AResource{A: [4]byte{198, 51, 100, 7}}
					case dnsmessage.TypeAAAA:
						body = &dnsmessage.AAAAResource{AAAA: netip.MustParseAddr("2001:db8::7").As16()}
					}
					if body != nil {
						r.Answers = []dnsmessage.Resource{{Header: dnsmessage.ResourceHeader{Name: question.Name, Type: question.Type, Class: dnsmessage.ClassINET}, Body: body}}
					}
					packed, err := r.Pack()
					if err != nil {
						t.Error(err)
						return
					}
					if _, err := server.WriteTo(packed, peer); err != nil {
						return
					}
				}
			}()
			t.Cleanup(func() { server.Close(); <-done })
			dialer := *systemDialer
			guard := dialer.ControlContext
			if guard == nil {
				t.Fatal("the system dialer has no destination guard")
			}
			var checked atomic.Int64
			dialer.ControlContext = func(ctx context.Context, network, address string, raw syscall.RawConn) error {
				checked.Add(1)
				if err := guard(ctx, network, address, raw); err != nil {
					return err
				}
				return errors.New("the guard allowed an own destination; do not actually connect")
			}
			dialer.Resolver = &net.Resolver{PreferGo: true, Dial: func(ctx context.Context, _, _ string) (net.Conn, error) {
				return (&net.Dialer{}).DialContext(ctx, "udp4", server.LocalAddr().String())
			}}
			c := testClient(t)
			c.systemDial = dialer.DialContext
			var lookups atomic.Int64
			c.lookup = func(context.Context, string, string) ([]netip.Addr, error) {
				lookups.Add(1)
				return []netip.Addr{netip.MustParseAddr("198.51.100.7"), netip.MustParseAddr("2001:db8::7")}, nil
			}
			cfg := api.DefaultConfig()
			cfg.Proxies = []api.Proxy{{Port: 18080, Scene: cfg.Selected, Enabled: true}}
			switch mode {
			case "rule-fallback":
				cfg.Scenes[0].Rules = []api.Rule{{ID: "fallback", Enabled: true, Targets: []string{"*.example.com"}, Protocol: api.TCP, Action: api.Route, Via: []api.InterfaceRef{ruleRef("example0")}, Fallback: true}}
			case "address-rules":
				cfg.Scenes[0].Rules = []api.Rule{{ID: "unrelated", Enabled: true, Targets: []string{"203.0.113.0/24"}, Protocol: api.TCP, Action: api.Block}}
			}
			if err := c.compileScenes(cfg); err != nil {
				t.Fatal(err)
			}
			var bound atomic.Int64
			route := c.routeByRules(func(context.Context, api.InterfaceRef, proxy.Target) (net.Conn, error) {
				bound.Add(1)
				return nil, syscall.ENETUNREACH
			})
			ctx, cancel := context.WithTimeout(proxy.WithInterfaceAddrs(context.Background(), routeOwnAddresses), 2*time.Second)
			defer cancel()
			target := proxy.Target{Host: "rebind.example.com", Port: 443, IsDomain: true}
			if mode == "literal" {
				target = proxy.Target{Host: "198.51.100.7", Port: 443}
			}
			conn, err := route(ctx, cfg.Selected, target)
			if conn != nil {
				conn.Close()
			}
			if conn != nil || !errors.Is(err, syscall.EACCES) || checked.Load() == 0 {
				t.Fatalf("conn=%v err=%v checked=%d", conn, err, checked.Load())
			}
			if mode == "rule-fallback" && bound.Load() != 1 || mode != "rule-fallback" && bound.Load() != 0 {
				t.Fatal("unexpected interface attempts", bound.Load())
			}
			if mode == "address-rules" {
				if lookups.Load() != 1 || queries.Load() != 0 {
					t.Fatal("an address matched by the rules was resolved again", lookups.Load(), queries.Load())
				}
			} else if mode == "literal" {
				if lookups.Load() != 0 || queries.Load() != 0 {
					t.Fatal("a literal was resolved", lookups.Load(), queries.Load())
				}
			} else if lookups.Load() != 0 || queries.Load() != 2 {
				t.Fatal("the system resolver did not do exactly the A and AAAA queries", lookups.Load(), queries.Load())
			}
		})
	}
}

func TestSceneRuleInterfaceRefusesOwnResolvedAddress(t *testing.T) {
	c := testClient(t)
	c.lookup = func(context.Context, string, string) ([]netip.Addr, error) {
		return []netip.Addr{netip.MustParseAddr("198.51.100.7")}, nil
	}
	cfg := api.DefaultConfig()
	cfg.Proxies = []api.Proxy{{Port: 18080, Scene: cfg.Selected, Enabled: true}}
	cfg.Scenes[0].Rules = []api.Rule{{ID: "route", Enabled: true, Targets: []string{"198.51.100.0/24"}, Protocol: api.TCP, Action: api.Route, Via: []api.InterfaceRef{ruleRef("example0")}}}
	if err := c.compileScenes(cfg); err != nil {
		t.Fatal(err)
	}
	var sockets atomic.Int64
	link := net.Interface{Name: "example0", Index: 7, Flags: net.FlagUp | net.FlagRunning}
	native := egress.New(egress.Options{
		Interfaces: func(context.Context) ([]api.Interface, error) {
			return []api.Interface{{Name: link.Name, Kind: api.KindEthernet, Up: true, IPv4: []string{"192.0.2.2"}}}, nil
		},
		InterfaceByName:  func(string) (*net.Interface, error) { return &link, nil },
		InterfaceByIndex: func(int) (*net.Interface, error) { return &link, nil },
		SystemDNS:        func() []netip.Addr { return nil },
		DialContext: func(context.Context, string, string, string) (net.Conn, error) {
			sockets.Add(1)
			return nil, errors.New("must not open a socket")
		},
	})
	ctx := proxy.WithInterfaceAddrs(context.Background(), routeOwnAddresses)
	conn, err := c.routeByRules(native.Dial)(ctx, cfg.Selected, proxy.Target{Host: "example.com", Port: 443, IsDomain: true})
	if conn != nil || !errors.Is(err, syscall.EACCES) || sockets.Load() != 0 {
		t.Fatalf("conn=%v err=%v sockets=%d", conn, err, sockets.Load())
	}
}
