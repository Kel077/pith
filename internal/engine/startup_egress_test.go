package engine

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"io"
	"math/big"
	"net"
	"net/netip"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Kel077/pith/internal/api"
	"github.com/Kel077/pith/internal/engine/attribution"
	"github.com/Kel077/pith/internal/engine/capture"
	"github.com/Kel077/pith/internal/engine/forward"
	"github.com/Kel077/pith/internal/netif"
	"golang.zx2c4.com/wireguard/tun"
	"gvisor.dev/gvisor/pkg/buffer"
	"gvisor.dev/gvisor/pkg/tcpip"
	"gvisor.dev/gvisor/pkg/tcpip/adapters/gonet"
	"gvisor.dev/gvisor/pkg/tcpip/header"
	"gvisor.dev/gvisor/pkg/tcpip/link/channel"
	"gvisor.dev/gvisor/pkg/tcpip/network/ipv4"
	"gvisor.dev/gvisor/pkg/tcpip/network/ipv6"
	"gvisor.dev/gvisor/pkg/tcpip/stack"
	"gvisor.dev/gvisor/pkg/tcpip/transport/tcp"
)

type startupDriver struct {
	*testDriver
	peer *channel.Endpoint
}

type startupDevice struct {
	*testDevice
	peer *channel.Endpoint
}

func (d *startupDriver) Create(mtu int) (tun.Device, error) {
	v, err := d.testDriver.Create(mtu)
	if err != nil {
		return nil, err
	}
	return &startupDevice{testDevice: v.(*testDevice), peer: d.peer}, nil
}

func (d *startupDevice) Write(bufs [][]byte, offset int) (int, error) {
	for _, b := range bufs {
		packet := stack.NewPacketBuffer(stack.PacketBufferOptions{Payload: buffer.MakeWithData(slices.Clone(b[offset:]))})
		protocol := ipv4.ProtocolNumber
		if b[offset]>>4 == 6 {
			protocol = ipv6.ProtocolNumber
		}
		d.peer.InjectInbound(protocol, packet)
		packet.DecRef()
	}
	return len(bufs), nil
}

func (d *startupDriver) Add(ctx context.Context, prefix netip.Prefix, name string) error {
	if err := d.testDriver.Add(ctx, prefix, name); err != nil {
		return err
	}
	d.changes <- capture.Change{Index: 42, Prefix: prefix, Added: true}
	return nil
}

// Model only the lookup branches used by this fixture, not the whole kernel:
// a bound socket first needs a matching route on its interface, then a scoped
// route, then the longest non-scoped match for the all-zero address. The last
// lookup does not explicitly select a /0. See XNU rt_lookup_common and
// node_lookup_default in bsd/net/route.c, and rn_lookup_args in bsd/net/radix.c.
func startupBoundRoute(routes []capture.Route, name string, target netip.Addr) bool {
	if capture.SystemInterface(routes, target) == name {
		return true
	}
	var scoped []capture.Route
	for _, route := range routes {
		if route.Scoped && route.Interface == name {
			route.Scoped = false
			scoped = append(scoped, route)
		}
	}
	if capture.SystemInterface(scoped, target) == name {
		return true
	}
	zero := netip.IPv4Unspecified()
	if target.Is6() {
		zero = netip.IPv6Unspecified()
	}
	return capture.SystemInterface(routes, zero) == name
}

// Exercise the real startup order, TCP forwarder, hostname sniffing and guarded
// system dial. Both the native side and remote TLS server are memory endpoints.
func TestSafeStartImmediatelyUsesSystemEgressAfterActivation(t *testing.T) {
	for _, family := range []string{"ipv4", "ipv6"} {
		for _, mode := range []string{"host", "full", "legacy", "legacy-scoped-default"} {
			t.Run(family+"/"+mode, func(t *testing.T) {
				ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
				defer cancel()
				is6 := family == "ipv6"
				target := netip.MustParseAddrPort("203.0.113.20:443")
				source := netip.MustParseAddr("192.0.2.123")
				protocol, network := ipv4.ProtocolNumber, "tcp4"
				if is6 {
					target, source = netip.MustParseAddrPort("[2001:db8::20]:443"), netip.MustParseAddr("2001:db8:1::123")
					protocol, network = ipv6.ProtocolNumber, "tcp6"
				}
				app := stack.New(stack.Options{NetworkProtocols: []stack.NetworkProtocolFactory{ipv4.NewProtocol, ipv6.NewProtocol}, TransportProtocols: []stack.TransportProtocolFactory{tcp.NewProtocol}})
				link := channel.New(128, 1500, "")
				defer func() { link.Close(); app.Close(); app.Wait() }()
				if err := app.CreateNIC(1, link); err != nil {
					t.Fatal(err)
				}
				if err := app.AddProtocolAddress(1, tcpip.ProtocolAddress{Protocol: protocol, AddressWithPrefix: tcpip.AddrFromSlice(source.AsSlice()).WithPrefix()}, stack.AddressProperties{}); err != nil {
					t.Fatal(err)
				}
				app.SetRouteTable([]tcpip.Route{{Destination: header.IPv4EmptySubnet, NIC: 1}, {Destination: header.IPv6EmptySubnet, NIC: 1}})
				routes := []capture.Route{
					{Prefix: netip.MustParsePrefix("0.0.0.0/0"), Interface: "example0", Index: 1, Gateway: netip.MustParseAddr("192.0.2.1")},
					{Prefix: netip.MustParsePrefix("0.0.0.0/0"), Interface: "example1", Index: 2, Gateway: netip.MustParseAddr("192.0.2.1"), Scoped: true},
					{Prefix: netip.MustParsePrefix("192.0.2.0/24"), Interface: "example0", Index: 1},
					{Prefix: netip.MustParsePrefix("192.0.2.1/32"), Interface: "example0", Index: 1},
					{Prefix: netip.MustParsePrefix("192.0.2.1/32"), Interface: "example1", Index: 2, Scoped: true},
					{Prefix: netip.MustParsePrefix("::/0"), Interface: "utun70", Index: 70, Scoped: true},
					{Prefix: netip.MustParsePrefix("::/0"), Interface: "example0", Index: 1, Gateway: netip.MustParseAddr("2001:db8:1::1")},
					{Prefix: netip.MustParsePrefix("::/0"), Interface: "example1", Index: 2, Scoped: true},
				}
				if mode == "legacy-scoped-default" {
					for _, raw := range []string{"0.0.0.0/0", "::/0"} {
						routes = append(routes, capture.Route{Prefix: netip.MustParsePrefix(raw), Interface: "example0", Index: 1, Scoped: true})
					}
				}
				base, _ := newTestEngine(t, nil)
				d := &startupDriver{testDriver: &testDriver{routes: slices.Clone(routes)}, peer: link}
				opts := base.opts
				opts.DataDir, opts.Driver = filepath.Join(t.TempDir(), "startup"), d
				opts.Scopes = []netip.Prefix{netip.PrefixFrom(target.Addr(), target.Addr().BitLen())}
				if mode != "host" {
					opts.Scopes = nil
				}
				opts.WatchRoutes = func(ctx context.Context) (<-chan capture.Change, error) {
					d.changes = make(chan capture.Change, 64)
					go func() { <-ctx.Done(); close(d.changes) }()
					return d.changes, nil
				}
				opts.Interfaces = func(context.Context) ([]api.Interface, error) {
					return []api.Interface{{Name: "example0", Up: true, DNS: []string{"192.0.2.1"}}, {Name: "example1", Up: true}, {Name: "utun70"}}, nil
				}
				opts.Lookup = func(context.Context, api.Protocol, netip.AddrPort, netip.AddrPort) (attribution.Attribution, error) {
					return attribution.Attribution{Process: api.Process{PID: 123, Path: "/tmp/example/pithd"}}, nil
				}
				public, private, err := ed25519.GenerateKey(rand.Reader)
				if err != nil {
					t.Fatal(err)
				}
				template := &x509.Certificate{SerialNumber: big.NewInt(1), DNSNames: []string{"example.com"}, NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour), ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}}
				der, err := x509.CreateCertificate(rand.Reader, template, template, public, private)
				if err != nil {
					t.Fatal(err)
				}
				cert, err := x509.ParseCertificate(der)
				if err != nil {
					t.Fatal(err)
				}
				roots := x509.NewCertPool()
				roots.AddCert(cert)
				serverDone := make(chan error, 1)
				var dialed atomic.Bool
				var attempts atomic.Int32
				var servers sync.WaitGroup
				defer servers.Wait()
				opts.Dial = func(dialCtx context.Context, name, dialNetwork, dialTarget string) (net.Conn, error) {
					if attempts.Add(1) != 1 {
						return nil, errors.New("SafeStart created more than one outbound connection")
					}
					if name != "example0" || dialNetwork != network || dialTarget != target.String() {
						t.Errorf("wrong startup egress: %s %s %s", name, dialNetwork, dialTarget)
					}
					if err := netif.ValidateBinding(dialCtx, 1); err != nil {
						return nil, err
					}
					live, _ := d.Snapshot()
					if strings.HasPrefix(mode, "legacy") {
						// Replay the former capture prefixes in the native lookup model,
						// without weakening the production planner or ownership guard.
						live = slices.DeleteFunc(live, func(r capture.Route) bool { return r.Index == 42 })
						old := []string{"0.0.0.0/1", "128.0.0.0/1"}
						if is6 {
							old = []string{"::/1", "8000::/1"}
						}
						for _, raw := range old {
							live = append(live, capture.Route{Prefix: netip.MustParsePrefix(raw), Interface: "utun42", Index: 42})
						}
					}
					if !startupBoundRoute(live, name, target.Addr()) {
						return nil, errors.New("synthetic bound default lookup: network is unreachable")
					}
					dialed.Store(true)
					a, b := net.Pipe()
					servers.Add(1)
					go func() {
						defer servers.Done()
						defer b.Close()
						b.SetDeadline(time.Now().Add(2 * time.Second))
						server := tls.Server(b, &tls.Config{Certificates: []tls.Certificate{{Certificate: [][]byte{der}, PrivateKey: private}}, MinVersion: tls.VersionTLS12})
						if err := server.HandshakeContext(ctx); err != nil {
							serverDone <- err
							return
						}
						_, err := server.Write([]byte("verified"))
						serverDone <- err
					}()
					return a, nil
				}
				opts.Verify = func(ctx context.Context, c *capture.Capture, f *forward.Forwarder) error {
					pumpCtx, stop := context.WithCancel(ctx)
					done := make(chan struct{})
					go func() {
						defer close(done)
						for {
							packet := link.ReadContext(pumpCtx)
							if packet == nil {
								return
							}
							view := packet.ToView()
							data := slices.Clone(view.AsSlice())
							view.Release()
							packet.DecRef()
							select {
							case d.devices[0].packets <- data:
							case <-pumpCtx.Done():
								return
							}
						}
					}()
					defer func() { stop(); <-done }()
					conn, err := gonet.DialContextTCP(ctx, app, tcpip.FullAddress{NIC: 1, Addr: tcpip.AddrFromSlice(target.Addr().AsSlice()), Port: target.Port()}, protocol)
					if err != nil {
						return err
					}
					defer conn.Close()
					conn.SetDeadline(time.Now().Add(2 * time.Second))
					client := tls.Client(conn, &tls.Config{RootCAs: roots, ServerName: "example.com", MinVersion: tls.VersionTLS12})
					if err := client.HandshakeContext(ctx); err != nil {
						return err
					}
					response := make([]byte, len("verified"))
					if _, err := io.ReadFull(client, response); err != nil {
						return err
					}
					local, _ := netip.ParseAddrPort(conn.LocalAddr().String())
					if string(response) != "verified" || !f.SawTCP(local, target) {
						t.Error("SafeStart did not prove its exact captured socket")
					}
					return nil
				}
				e, err := New(opts)
				if err != nil {
					t.Fatal(err)
				}
				defer e.Close()
				events, err := e.Subscribe(ctx)
				if err != nil {
					t.Fatal(err)
				}
				cfg := fullTestConfig()
				cfg.Settings.IPv6 = is6
				err = e.Start(ctx, cfg.Scenes[0], cfg.Settings)
				if mode == "legacy" {
					if api.Code(err) != api.ErrNetworkLost || dialed.Load() {
						t.Fatal("legacy split capture did not reproduce the bound egress failure", err)
					}
					found := false
					for !found {
						select {
						case event := <-events:
							if event.Connection != nil && event.Connection.Remote == target.String() && event.Connection.Outcome == api.Unreachable && strings.Contains(event.Connection.Problem, "synthetic bound default lookup") {
								found = true
							}
							if event.Status != nil && event.Status.State == api.Failed && !found {
								t.Fatal("SafeStart discarded its outbound failure event")
							}
						case <-ctx.Done():
							t.Fatal("missing startup failure diagnostic")
						}
					}
				} else if err != nil {
					t.Fatalf("immediate SafeStart failed: %v; flows: %+v", err, e.store.Connections(api.ConnectionQuery{}))
				} else {
					if !dialed.Load() {
						t.Fatal("SafeStart did not use the outbound dialer")
					}
					if err := <-serverDone; err != nil {
						t.Fatal(err)
					}
				}
				if err := e.Stop(ctx); err != nil {
					t.Fatal(err)
				}
				flows := e.store.Connections(api.ConnectionQuery{})
				if attempts.Load() != 1 || len(flows) != 1 {
					t.Fatalf("SafeStart must create one captured helper flow and one bound outbound attempt: attempts=%d flows=%d", attempts.Load(), len(flows))
				}
				flow := flows[0]
				if flow.Process.PID != 123 || flow.Process.Path != "/tmp/example/pithd" || flow.Remote != target.String() || flow.Domain != "example.com" {
					t.Fatalf("SafeStart did not retain the captured helper attribution and TLS target: %+v", flow)
				}
				// Stop may interrupt the in-memory FIN exchange. Its diagnostic must
				// not change the selected route or leave another forwarding attempt.
				if mode != "legacy" && (flow.Outcome != api.System || flow.Interface != "example0" || flow.Ended.IsZero() || flow.Sent == 0 || flow.Received == 0) {
					t.Fatalf("the captured SafeStart flow lost its system exit or TLS byte accounting: %+v", flow)
				}
				after, _ := d.Snapshot()
				if !slices.Equal(after, routes) {
					t.Fatal("startup egress changed external routes")
				}
			})
		}
	}
}
