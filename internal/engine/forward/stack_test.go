package forward

import (
	"context"
	"errors"
	"io"
	"net"
	"net/netip"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Kel077/pith/internal/api"
	"github.com/Kel077/pith/internal/engine/attribution"
	"github.com/Kel077/pith/internal/engine/capture"
	"github.com/Kel077/pith/internal/engine/egress"
	"github.com/Kel077/pith/internal/engine/telemetry"
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
	"gvisor.dev/gvisor/pkg/tcpip/transport/udp"
)

type memoryTUN struct {
	in     chan []byte
	done   chan struct{}
	events chan tun.Event
	once   sync.Once
	peer   *channel.Endpoint
	drop   atomic.Bool
}

func (d *memoryTUN) File() *os.File           { return nil }
func (d *memoryTUN) Name() (string, error)    { return "utun99", nil }
func (d *memoryTUN) MTU() (int, error)        { return 1500, nil }
func (d *memoryTUN) BatchSize() int           { return 1 }
func (d *memoryTUN) Events() <-chan tun.Event { return d.events }
func (d *memoryTUN) Close() error             { d.once.Do(func() { close(d.done); close(d.events) }); return nil }
func (d *memoryTUN) Read(bufs [][]byte, sizes []int, offset int) (int, error) {
	select {
	case <-d.done:
		return 0, net.ErrClosed
	case data := <-d.in:
		sizes[0] = copy(bufs[0][offset:], data)
		return 1, nil
	}
}
func (d *memoryTUN) Write(bufs [][]byte, offset int) (int, error) {
	select {
	case <-d.done:
		return 0, net.ErrClosed
	default:
	}
	if d.drop.Load() {
		return len(bufs), nil
	}
	for _, buf := range bufs {
		data := buf[offset:]
		proto := ipv4.ProtocolNumber
		if data[0]>>4 == 6 {
			proto = ipv6.ProtocolNumber
		}
		pkt := stack.NewPacketBuffer(stack.PacketBufferOptions{Payload: buffer.MakeWithData(append([]byte(nil), data...))})
		d.peer.InjectInbound(proto, pkt)
		pkt.DecRef()
	}
	return len(bufs), nil
}

type fixtureStack struct {
	f      *Forwarder
	app    *stack.Stack
	router *egress.Router
	store  *telemetry.Store
	device *memoryTUN
}

func newFixture(t *testing.T, dial egress.DialFunc, scene api.Scene) *fixtureStack {
	t.Helper()
	app := stack.New(stack.Options{NetworkProtocols: []stack.NetworkProtocolFactory{ipv4.NewProtocol, ipv6.NewProtocol}, TransportProtocols: []stack.TransportProtocolFactory{tcp.NewProtocol, udp.NewProtocol}})
	link := channel.New(1024, 1500, "")
	if err := app.CreateNIC(1, link); err != nil {
		t.Fatal(err)
	}
	for _, raw := range []string{"192.0.2.123", "2001:db8::123"} {
		addr := netip.MustParseAddr(raw)
		proto := ipv4.ProtocolNumber
		if addr.Is6() {
			proto = ipv6.ProtocolNumber
		}
		if err := app.AddProtocolAddress(1, tcpip.ProtocolAddress{Protocol: proto, AddressWithPrefix: tcpip.AddressWithPrefix{Address: tcpip.AddrFromSlice(addr.AsSlice()), PrefixLen: addr.BitLen()}}, stack.AddressProperties{}); err != nil {
			t.Fatal(err)
		}
	}
	app.SetRouteTable([]tcpip.Route{{Destination: header.IPv4EmptySubnet, NIC: 1}, {Destination: header.IPv6EmptySubnet, NIC: 1}})
	device := &memoryTUN{in: make(chan []byte, 128), done: make(chan struct{}), events: make(chan tun.Event), peer: link}
	ctx, cancel := context.WithCancel(context.Background())
	pumpDone := make(chan struct{})
	go func() {
		defer close(pumpDone)
		for {
			p := link.ReadContext(ctx)
			if p == nil {
				return
			}
			v := p.ToView()
			data := append([]byte(nil), v.AsSlice()...)
			v.Release()
			p.DecRef()
			select {
			case device.in <- data:
			case <-ctx.Done():
				return
			}
		}
	}()
	store, err := telemetry.Open(telemetry.Options{})
	if err != nil {
		t.Fatal(err)
	}
	router := egress.New([]capture.Route{{Prefix: netip.MustParsePrefix("0.0.0.0/0"), Interface: "en0"}, {Prefix: netip.MustParsePrefix("::/0"), Interface: "en0"}}, []api.Interface{{Name: "en0", Up: true}}, dial)
	if err := router.Reload(scene, api.DefaultSettings()); err != nil {
		t.Fatal(err)
	}
	f, err := New(Options{Device: device, Router: router, Store: store, IPv6: true, UDPIdle: 500 * time.Millisecond, DNSIdle: 100 * time.Millisecond, Lookup: func(context.Context, api.Protocol, netip.AddrPort, netip.AddrPort) (attribution.Attribution, error) {
		return attribution.Attribution{App: api.AppRef{Path: "/Applications/Example.app"}, Process: api.Process{PID: 42, Path: "/Applications/Example.app/Contents/MacOS/App"}}, nil
	}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		f.Close()
		cancel()
		<-pumpDone
		link.Close()
		app.Close()
		app.Wait()
	})
	return &fixtureStack{f, app, router, store, device}
}

func (x *fixtureStack) dialTCP(ctx context.Context, remote string) (*gonet.TCPConn, error) {
	ap := netip.MustParseAddrPort(remote)
	proto := ipv4.ProtocolNumber
	if ap.Addr().Is6() {
		proto = ipv6.ProtocolNumber
	}
	return gonet.DialContextTCP(ctx, x.app, tcpip.FullAddress{NIC: 1, Addr: tcpip.AddrFromSlice(ap.Addr().AsSlice()), Port: ap.Port()}, proto)
}

func echoDial(t *testing.T) egress.DialFunc {
	return func(ctx context.Context, iface, network, address string) (net.Conn, error) {
		a, b := net.Pipe()
		done := make(chan struct{})
		go func() { defer close(done); defer b.Close(); io.Copy(b, b) }()
		t.Cleanup(func() { a.Close(); b.Close(); <-done })
		return a, nil
	}
}

func TestTCPRelayIPv4IPv6AndHotRules(t *testing.T) {
	for _, remote := range []string{"192.0.2.1:443", "[2001:db8::1]:443"} {
		t.Run(remote, func(t *testing.T) {
			x := newFixture(t, echoDial(t), api.Scene{ID: "test"})
			ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			defer cancel()
			conn, err := x.dialTCP(ctx, remote)
			if err != nil {
				t.Fatal(err)
			}
			defer conn.Close()
			conn.SetDeadline(time.Now().Add(2 * time.Second))
			request := "GET / HTTP/1.1\r\nHost: example.com\r\n\r\n"
			if _, err := io.WriteString(conn, request); err != nil {
				t.Fatal(err)
			}
			buf := make([]byte, len(request))
			if _, err := io.ReadFull(conn, buf); err != nil || string(buf) != request {
				t.Fatal(string(buf), err)
			}
			scene := api.Scene{ID: "test", Rules: []api.Rule{{ID: "block", Enabled: true, Protocol: api.TCP, Action: api.Block}}}
			if err := x.router.Reload(scene, api.DefaultSettings()); err != nil {
				t.Fatal(err)
			}
			if _, err := io.WriteString(conn, "open stays open"); err != nil {
				t.Fatal(err)
			}
			buf = make([]byte, len("open stays open"))
			if _, err := io.ReadFull(conn, buf); err != nil || string(buf) != "open stays open" {
				t.Fatal(string(buf), err)
			}
			if denied, err := x.dialTCP(ctx, remote); err == nil {
				denied.Close()
				t.Fatal("new connection escaped block")
			}
			found := false
			for _, c := range x.store.Connections(api.ConnectionQuery{}) {
				if c.Outcome == api.System && c.Domain == "example.com" && c.Sent > 0 && c.Received > 0 && c.Interface == "en0" {
					found = true
				}
			}
			if !found {
				t.Fatal("missing attribution, name or traffic", x.store.Connections(api.ConnectionQuery{}))
			}
		})
	}
}

func TestDomainBlockDoesNotDial(t *testing.T) {
	scene := api.Scene{ID: "test", Rules: []api.Rule{{ID: "block", Enabled: true, Protocol: api.TCP, Action: api.Block, Targets: []string{"example.com"}}}}
	x := newFixture(t, func(context.Context, string, string, string) (net.Conn, error) {
		t.Error("dialed blocked host")
		return nil, errors.New("unexpected dial")
	}, scene)
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	conn, err := x.dialTCP(ctx, "192.0.2.1:80")
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	conn.SetDeadline(time.Now().Add(time.Second))
	io.WriteString(conn, "GET / HTTP/1.1\r\nHost: example.com\r\n\r\n")
	if _, err := conn.Read(make([]byte, 1)); err == nil || !strings.Contains(err.Error(), "reset") {
		t.Fatal("expected TCP reset", err)
	}
	rows := x.store.Connections(api.ConnectionQuery{})
	if len(rows) != 1 || rows[0].Outcome != api.Blocked || rows[0].Domain != "example.com" {
		t.Fatal(rows)
	}
}

func TestUDPRelayAndIdleExpiry(t *testing.T) {
	x := newFixture(t, echoDial(t), api.Scene{ID: "test"})
	conn, err := gonet.DialUDP(x.app, nil, &tcpip.FullAddress{NIC: 1, Addr: tcpip.AddrFrom4([4]byte{192, 0, 2, 1}), Port: 53}, ipv4.ProtocolNumber)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	conn.SetDeadline(time.Now().Add(time.Second))
	for _, message := range []string{"first datagram", "second datagram"} {
		if _, err := conn.Write([]byte(message)); err != nil {
			t.Fatal(err)
		}
		buf := make([]byte, 100)
		n, err := conn.Read(buf)
		if err != nil || string(buf[:n]) != message {
			t.Fatal(string(buf[:n]), err)
		}
	}
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		rows := x.store.Connections(api.ConnectionQuery{})
		if len(rows) == 1 && !rows[0].Ended.IsZero() {
			if rows[0].Sent != 29 || rows[0].Received != 29 {
				t.Fatal(rows)
			}
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("UDP idle session did not close")
}

func TestCloseAbortsUnansweredHandshake(t *testing.T) {
	x := newFixture(t, echoDial(t), api.Scene{ID: "test"})
	x.device.drop.Store(true)
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	dialed := make(chan struct{})
	go func() {
		defer close(dialed)
		c, _ := x.dialTCP(ctx, "192.0.2.1:443")
		if c != nil {
			c.Close()
		}
	}()
	deadline := time.Now().Add(time.Second)
	for {
		x.f.mu.Lock()
		active := x.f.active
		x.f.mu.Unlock()
		if active > 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("handshake did not begin")
		}
		time.Sleep(time.Millisecond)
	}
	done := make(chan struct{})
	go func() { x.f.Close(); close(done) }()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("shutdown left a handshake blocked")
	}
	cancel()
	<-dialed
}
