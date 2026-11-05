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
	"syscall"
	"testing"
	"time"

	"github.com/Kel077/pith/internal/api"
	"github.com/Kel077/pith/internal/engine/attribution"
	"gvisor.dev/gvisor/pkg/tcpip"
	"gvisor.dev/gvisor/pkg/tcpip/adapters/gonet"
	"gvisor.dev/gvisor/pkg/tcpip/network/ipv4"
	"gvisor.dev/gvisor/pkg/tcpip/network/ipv6"
)

// A client can close its native UDP socket with final QUIC datagrams still in
// transit. A late reply then generates ICMP port-unreachable before the final
// app datagram reaches the forwarder. That asynchronous error must not release
// the tuple and re-match its tail through another interface without attribution.
func TestUDPClientICMPDoesNotReclassifyQueuedDatagrams(t *testing.T) {
	for _, remote := range []string{"192.0.2.1:443", "[2001:db8::1]:443"} {
		t.Run(remote, func(t *testing.T) { udpClientICMP(t, remote) })
	}
}

func udpClientICMP(t *testing.T, remote string) {
	firstPacket := make(chan []byte, 1)
	icmp := make(chan struct{}, 1)
	dialed := make(chan string, 8)
	peers := make(chan net.Conn, 8)
	var ownerGone atomic.Bool
	appRef := api.AppRef{Path: "/Applications/Example.app"}
	scene := api.Scene{ID: "test", Rules: []api.Rule{{ID: "app-route", Enabled: true, Apps: []api.AppRef{appRef}, Protocol: api.UDP, Action: api.Route, Via: []api.InterfaceRef{{Name: "example0"}}}}}
	x := newFixture(t, func(_ context.Context, iface, _, _ string) (net.Conn, error) {
		a, b := net.Pipe()
		dialed <- iface
		peers <- b
		t.Cleanup(func() { a.Close(); b.Close() })
		return tupleConn{a}, nil
	}, scene, func(opts *Options) {
		opts.UDPIdle = time.Second
		opts.Router.Interfaces([]api.Interface{{Name: "en0", Up: true}, {Name: "example0", Up: true}})
		opts.Lookup = func(context.Context, api.Protocol, netip.AddrPort, netip.AddrPort) (attribution.Attribution, error) {
			if ownerGone.Load() {
				return attribution.Attribution{}, attribution.ErrNotFound
			}
			return attribution.Attribution{App: appRef, Process: api.Process{PID: 42, Path: appRef.Path + "/Contents/MacOS/App"}}, nil
		}
		opts.Packet = func(packet []byte) bool {
			if len(packet) > 20 {
				protocol := packet[9]
				if packet[0]>>4 == 6 {
					protocol = packet[6]
				}
				switch protocol {
				case 17:
					select {
					case firstPacket <- append([]byte(nil), packet...):
					default:
					}
				case 1, 58:
					select {
					case icmp <- struct{}{}:
					default:
					}
				}
			}
			return false
		}
	})
	endpoint := netip.MustParseAddrPort(remote)
	protocol := ipv4.ProtocolNumber
	if endpoint.Addr().Is6() {
		protocol = ipv6.ProtocolNumber
	}
	conn, err := gonet.DialUDP(x.app, nil, &tcpip.FullAddress{NIC: 1, Addr: tcpip.AddrFromSlice(endpoint.Addr().AsSlice()), Port: endpoint.Port()}, protocol)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	if _, err := conn.Write([]byte("queued tail")); err != nil {
		t.Fatal(err)
	}
	var peer net.Conn
	select {
	case peer = <-peers:
	case <-time.After(time.Second):
		t.Fatal("no outbound socket")
	}
	peer.SetDeadline(time.Now().Add(2 * time.Second))
	if _, err := io.ReadFull(peer, make([]byte, len("queued tail"))); err != nil {
		t.Fatal(err)
	}
	packet := <-firstPacket
	if iface := <-dialed; iface != "example0" {
		t.Fatal("initial request used", iface)
	}
	ownerGone.Store(true)
	conn.Close()
	if _, err := peer.Write([]byte("late server response")); err != nil {
		t.Fatal(err)
	}
	select {
	case <-icmp:
	case <-time.After(time.Second):
		t.Fatal("closed UDP socket did not generate ICMP")
	}
	// Wait until gVisor consumes the asynchronous error, without keeping another
	// native app socket open or giving a newly classified flow an owner identity.
	time.Sleep(20 * time.Millisecond)
	rows := x.store.Connections(api.ConnectionQuery{})
	if len(rows) != 1 || !rows[0].Ended.IsZero() {
		t.Fatalf("ICMP ended the pinned UDP session before idle expiry: %+v", rows)
	}
	x.device.in <- packet
	if _, err := io.ReadFull(peer, make([]byte, len("queued tail"))); err != nil {
		t.Fatal("tail did not keep the original outbound socket:", err)
	}
	select {
	case iface := <-dialed:
		t.Fatal("same tuple was reclassified through", iface)
	default:
	}
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		rows = x.store.Connections(api.ConnectionQuery{})
		if len(rows) == 1 && !rows[0].Ended.IsZero() {
			if rows[0].RuleID != "app-route" || rows[0].Interface != "example0" || rows[0].Process.PID != 42 || rows[0].Sent != 2*uint64(len("queued tail")) {
				t.Fatal("pinned identity or tail counters changed", rows)
			}
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("ICMP handling prevented normal UDP idle expiry")
}

type udpReadErrorOnce struct {
	net.Conn
	err   error
	once  sync.Once
	fired chan struct{}
}

func (c *udpReadErrorOnce) Read(data []byte) (int, error) {
	first := false
	c.once.Do(func() { first = true; close(c.fired) })
	if first {
		return 0, c.err
	}
	return c.Conn.Read(data)
}

func TestUDPOutboundICMPKeepsBindingAndFatalErrorsRemainVisible(t *testing.T) {
	for _, fatal := range []bool{false, true} {
		t.Run(map[bool]string{false: "asynchronous ICMP", true: "fatal error"}[fatal], func(t *testing.T) {
			fired := make(chan struct{})
			peers := make(chan net.Conn, 1)
			errOnce := error(&net.OpError{Op: "read", Net: "udp", Err: &os.SyscallError{Syscall: "read", Err: syscall.ECONNREFUSED}})
			if fatal {
				errOnce = errors.New("synthetic UDP transport failure")
			}
			x := newFixture(t, func(context.Context, string, string, string) (net.Conn, error) {
				a, b := net.Pipe()
				peers <- b
				t.Cleanup(func() { a.Close(); b.Close() })
				return &udpReadErrorOnce{Conn: tupleConn{a}, err: errOnce, fired: fired}, nil
			}, api.Scene{ID: "test"})
			conn, err := gonet.DialUDP(x.app, nil, &tcpip.FullAddress{NIC: 1, Addr: tcpip.AddrFrom4([4]byte{192, 0, 2, 1}), Port: 443}, ipv4.ProtocolNumber)
			if err != nil {
				t.Fatal(err)
			}
			defer conn.Close()
			conn.SetDeadline(time.Now().Add(time.Second))
			conn.Write([]byte("first"))
			var peer net.Conn
			select {
			case peer = <-peers:
			case <-time.After(time.Second):
				t.Fatal("missing outbound")
			}
			peer.SetDeadline(time.Now().Add(time.Second))
			if _, err := io.ReadFull(peer, make([]byte, 5)); err != nil {
				t.Fatal(err)
			}
			select {
			case <-fired:
			case <-time.After(time.Second):
				t.Fatal("read error not injected")
			}
			if !fatal {
				if _, err := peer.Write([]byte("reply")); err != nil {
					t.Fatal("ICMP closed native UDP socket", err)
				}
				if _, err := conn.Read(make([]byte, 100)); err != nil {
					t.Fatal(err)
				}
				if _, err := conn.Write([]byte("second")); err != nil {
					t.Fatal(err)
				}
				if _, err := io.ReadFull(peer, make([]byte, 6)); err != nil {
					t.Fatal("ICMP changed UDP binding", err)
				}
			}
			deadline := time.Now().Add(time.Second)
			for time.Now().Before(deadline) {
				rows := x.store.Connections(api.ConnectionQuery{})
				if len(rows) == 1 && !rows[0].Ended.IsZero() {
					if fatal != strings.Contains(rows[0].Problem, "synthetic UDP transport failure") {
						t.Fatal("peer shutdown hid the actual transport error", rows)
					}
					return
				}
				time.Sleep(5 * time.Millisecond)
			}
			t.Fatal("UDP did not finish")
		})
	}
}

func TestUDPPacketErrorRecognizesOnlyExactPortUnreachable(t *testing.T) {
	stackConn := &gonet.UDPConn{}
	refused := (&tcpip.ErrConnectionRefused{}).String()
	for _, tc := range []struct {
		conn net.Conn
		err  error
		want bool
	}{
		{stackConn, &net.OpError{Net: "udp", Err: errors.New(refused)}, true},
		{nil, &net.OpError{Net: "udp", Err: errors.New(refused)}, false},
		{stackConn, &net.OpError{Net: "tcp", Err: errors.New(refused)}, false},
		{stackConn, &net.OpError{Net: "udp", Err: errors.New("other " + refused)}, false},
		{nil, syscall.ECONNREFUSED, true},
		{stackConn, net.ErrClosed, false},
	} {
		if got := udpPacketError(tc.err, tc.conn); got != tc.want {
			t.Errorf("%T %v = %v", tc.conn, tc.err, got)
		}
	}
}
