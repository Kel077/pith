package proxy

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/Kel077/pith/internal/api"
)

// Test peers communicate over ephemeral loopback sockets while representing
// remote targets. Keep TCP's half-close methods and its actual local address.
func dialRemotePeer(ctx context.Context, address string) (net.Conn, error) {
	conn, err := (&net.Dialer{}).DialContext(ctx, "tcp4", address)
	if err != nil {
		return nil, err
	}
	return &deviceConn{TCPConn: conn.(*net.TCPConn), remote: &net.TCPAddr{IP: net.ParseIP("203.0.113.80"), Port: 443}}, nil
}

func ownAddresses() ([]net.Addr, error) {
	return []net.Addr{
		&net.IPNet{IP: net.ParseIP("192.0.2.7"), Mask: net.CIDRMask(24, 32)},
		&net.IPNet{IP: net.ParseIP("198.51.100.7"), Mask: net.CIDRMask(32, 32)},
		&net.IPNet{IP: net.ParseIP("2001:db8::7"), Mask: net.CIDRMask(64, 128)},
		&net.IPAddr{IP: net.ParseIP("2001:db8::8")}, // another, temporary address
		&net.IPAddr{IP: net.ParseIP("fe80::7"), Zone: "example1"},
	}, nil
}

func TestCheckDestinationUsesAllOwnAddresses(t *testing.T) {
	ctx := WithInterfaceAddrs(context.Background(), ownAddresses)
	for host, refused := range map[string]bool{
		"192.0.2.7": true, "198.51.100.7": true, "::ffff:192.0.2.7": true,
		"2001:db8::7": true, "2001:db8::8": true, "fe80::7": true,
		"fe80::7%example0": true, "fe80::7%example1": true,
		"0.0.0.0": true, "0.1.2.3": true, "0.255.255.255": true, "::ffff:0.1.2.3": true,
		"127.0.0.1": true, "::1": true, "::": true, "224.0.0.1": true, "ff02::1": true,
		"192.0.2.8": false, "198.51.100.8": false, "2001:db8::9": false, "fe80::8%example0": false,
	} {
		if err := CheckDestination(ctx, netip.MustParseAddr(host)); errors.Is(err, errOwnTarget) != refused || !refused && err != nil {
			t.Errorf("%s: %v, refused %v", host, err, refused)
		}
	}
	if err := CheckDestination(ctx, netip.Addr{}); !errors.Is(err, errOwnTarget) || !errors.Is(err, syscall.EACCES) {
		t.Fatal("an invalid address was allowed", err)
	}
}

func TestCheckDestinationRefreshesAndFailsClosed(t *testing.T) {
	var current []net.Addr
	var failure error
	ctx := WithInterfaceAddrs(context.Background(), func() ([]net.Addr, error) { return current, failure })
	addr := netip.MustParseAddr("192.0.2.7")
	if err := CheckDestination(ctx, addr); err != nil {
		t.Fatal(err)
	}
	current, _ = ownAddresses()
	if err := CheckDestination(ctx, addr); !errors.Is(err, errOwnTarget) {
		t.Fatal("an address added since the last check was allowed", err)
	}
	current = nil
	if err := CheckDestination(ctx, addr); err != nil {
		t.Fatal("a removed address was remembered", err)
	}
	failure = errors.New("address list unavailable")
	if err := CheckDestination(ctx, addr); !errors.Is(err, errOwnTarget) {
		t.Fatal("an unreadable list was allowed", err)
	}
	failure = nil
	for _, invalid := range []net.Addr{nil, (*net.IPNet)(nil), &net.IPNet{}, &net.IPAddr{}, &net.TCPAddr{}} {
		current = []net.Addr{invalid}
		if err := CheckDestination(ctx, addr); !errors.Is(err, errOwnTarget) {
			t.Fatalf("an incomplete list (%T) was allowed: %v", invalid, err)
		}
	}
	canceled, cancel := context.WithCancel(ctx)
	cancel()
	if err := CheckDestination(canceled, addr); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
}

func TestEveryExitRefusesOwnLiteralTargets(t *testing.T) {
	var dials atomic.Int64
	refuse := func() (net.Conn, error) { dials.Add(1); return nil, errors.New("must not dial") }
	m, f := newTestManager(t, Options{
		InterfaceAddrs: ownAddresses,
		Dial:           func(context.Context, api.InterfaceRef, Target) (net.Conn, error) { return refuse() },
		Route:          func(context.Context, string, Target) (net.Conn, error) { return refuse() },
		Upstream:       func(context.Context, string, string) (net.Conn, error) { return refuse() },
	})
	applyTest(t, m, testProxy(18080), sceneProxy(18081, "office"), upstreamProxy(18082, "203.0.113.9:18083"))
	for _, port := range []int{18080, 18081, 18082} {
		for _, host := range []string{"192.0.2.7", "198.51.100.7", "2001:db8::7", "2001:db8::8", "fe80::7", "::ffff:192.0.2.7", "0.1.2.3", "::ffff:0.1.2.3"} {
			t.Run(fmt.Sprintf("%d/%s", port, host), func(t *testing.T) {
				if code, _ := ask(t, f.dial(t, port), Target{Host: host, Port: 80}); code != replyDenied {
					t.Fatalf("SOCKS reply = %d", code)
				}
				authority := net.JoinHostPort(host, "80")
				for _, wire := range []string{
					fmt.Sprintf("CONNECT %s HTTP/1.1\r\nHost: %s\r\n\r\n", authority, authority),
					fmt.Sprintf("GET http://%s/ HTTP/1.1\r\nHost: %s\r\n\r\n", authority, authority),
				} {
					conn := f.dial(t, port)
					fmt.Fprint(conn, wire)
					if answer := httpAnswer(t, conn); answer.StatusCode != 403 {
						t.Fatalf("HTTP status = %d", answer.StatusCode)
					}
				}
			})
		}
		if code, _ := ask(t, f.dial(t, port), Target{Host: "192.0.2.7.", Port: 80, IsDomain: true}); code != replyDenied {
			t.Fatal("an own address spelled as a name was allowed", code)
		}
	}
	if dials.Load() != 0 {
		t.Fatal("a forbidden target reached a dialer", dials.Load())
	}
}

// Benchmark only reads the native address list. It neither opens sockets nor
// reports any of the machine's actual addresses.
func BenchmarkCheckDestination(b *testing.B) {
	ctx := context.Background()
	addr := netip.MustParseAddr("203.0.113.80")
	for b.Loop() {
		if err := CheckDestination(ctx, addr); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkInterfaceAddrs(b *testing.B) {
	for b.Loop() {
		if _, err := net.InterfaceAddrs(); err != nil {
			b.Fatal(err)
		}
	}
}

type guardedTestConn struct {
	remote net.Addr
	closed atomic.Bool
	writes atomic.Int64
}

func (*guardedTestConn) Read([]byte) (int, error) { return 0, net.ErrClosed }
func (c *guardedTestConn) Write(p []byte) (int, error) {
	c.writes.Add(int64(len(p)))
	return len(p), nil
}
func (c *guardedTestConn) Close() error { c.closed.Store(true); return nil }
func (*guardedTestConn) LocalAddr() net.Addr {
	return &net.TCPAddr{IP: net.ParseIP("192.0.2.2"), Port: 50000}
}
func (c *guardedTestConn) RemoteAddr() net.Addr           { return c.remote }
func (*guardedTestConn) SetDeadline(time.Time) error      { return nil }
func (*guardedTestConn) SetReadDeadline(time.Time) error  { return nil }
func (*guardedTestConn) SetWriteDeadline(time.Time) error { return nil }

func TestDirectExitsRecheckRemoteBeforeSending(t *testing.T) {
	for _, exit := range []string{"interface", "scene"} {
		for _, mode := range []string{"resolved-own", "new-address", "mapped-new-address", "loopback", "unreadable", "unknown-peer"} {
			for _, protocol := range []string{"socks", "connect", "absolute"} {
				t.Run(exit+"/"+mode+"/"+protocol, func(t *testing.T) {
					var dialed atomic.Bool
					var reads atomic.Int64
					raw := &guardedTestConn{remote: &net.TCPAddr{IP: net.ParseIP("192.0.2.7"), Port: 80}}
					if mode == "mapped-new-address" {
						raw.remote = &net.TCPAddr{IP: net.ParseIP("::ffff:192.0.2.7"), Port: 80}
					} else if mode == "loopback" {
						raw.remote = &net.TCPAddr{IP: net.ParseIP("127.0.0.1"), Port: 80}
					} else if mode == "unknown-peer" {
						raw.remote = nil
					}
					source := func() ([]net.Addr, error) {
						reads.Add(1)
						if mode == "unreadable" && dialed.Load() {
							return nil, errors.New("address list changed while reading")
						}
						if mode == "resolved-own" || dialed.Load() {
							return ownAddresses()
						}
						return nil, nil
					}
					dial := func(context.Context, api.InterfaceRef, Target) (net.Conn, error) {
						dialed.Store(true) // the address is acquired while connect is in flight
						return raw, nil
					}
					m, f := newTestManager(t, Options{InterfaceAddrs: source, Dial: dial,
						Route: func(ctx context.Context, _ string, target Target) (net.Conn, error) {
							return dial(ctx, api.InterfaceRef{}, target)
						},
					})
					p := testProxy(18080)
					if exit == "scene" {
						p = sceneProxy(18080, "office")
					}
					applyTest(t, m, p)
					target := Target{Host: "192.0.2.7", Port: 80}
					if mode == "resolved-own" {
						target = Target{Host: "example.com", Port: 80, IsDomain: true}
					}
					conn := f.dial(t, p.Port)
					switch protocol {
					case "socks":
						wire := append([]byte{5, 1, 0}, requestBytes(target)...)
						conn.Write(append(wire, []byte("early bytes")...))
						readMethod(t, conn, 0)
						if code, _ := readReply(t, conn); code != replyDenied {
							t.Fatal("SOCKS reply", code)
						}
					case "connect", "absolute":
						if protocol == "connect" {
							fmt.Fprintf(conn, "CONNECT %s HTTP/1.1\r\nHost: %s\r\n\r\nearly bytes", target.String(), target.String())
						} else {
							fmt.Fprintf(conn, "POST http://%s/ HTTP/1.1\r\nHost: %s\r\nContent-Length: 11\r\n\r\nearly bytes", target.String(), target.String())
						}
						if answer := httpAnswer(t, conn); answer.StatusCode != 403 {
							t.Fatal("HTTP status", answer.StatusCode)
						}
					}
					if !dialed.Load() || !raw.closed.Load() || raw.writes.Load() != 0 {
						t.Fatalf("dialed=%v closed=%v bytes sent=%d", dialed.Load(), raw.closed.Load(), raw.writes.Load())
					}
					if mode == "new-address" || mode == "mapped-new-address" || mode == "unreadable" {
						if reads.Load() != 2 {
							t.Fatalf("%d address reads, want a fresh read after connect", reads.Load())
						}
					}
					waitFor(t, func() bool { m.mu.Lock(); defer m.mu.Unlock(); return m.total == 0 })
					if st := m.Status(context.Background())[0]; st.Open != 0 || st.Sent != 0 || st.Received != 0 || st.Error != nil {
						t.Fatal("a refused target changed the listener or counters", st)
					}
				})
			}
		}
	}
}
