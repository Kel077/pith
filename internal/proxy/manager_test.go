package proxy

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"io"
	"net"
	"net/netip"
	"strconv"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/Kel077/pith/internal/api"
)

var testVia = api.InterfaceRef{Name: "example0", HardwareAddr: "02:00:00:00:00:01"}

type listenerFixture struct {
	mu    sync.Mutex
	addrs map[int]string
}

func (f *listenerFixture) listen(ctx context.Context, network, address string) (net.Listener, error) {
	if network != "tcp4" {
		return nil, errors.New("listener was not IPv4-only")
	}
	host, portText, err := net.SplitHostPort(address)
	if err != nil || host != "127.0.0.1" {
		return nil, errors.New("listener was not bound to IPv4 loopback")
	}
	port, err := strconv.Atoi(portText)
	if err != nil || port < 1024 || port > 65535 {
		return nil, errors.New("listener used an invalid configured port")
	}
	ln, err := (&net.ListenConfig{}).Listen(ctx, network, "127.0.0.1:0")
	if err == nil {
		f.mu.Lock()
		f.addrs[port] = ln.Addr().String()
		f.mu.Unlock()
	}
	return ln, err
}

func (f *listenerFixture) dial(t *testing.T, port int) *net.TCPConn {
	t.Helper()
	f.mu.Lock()
	address := f.addrs[port]
	f.mu.Unlock()
	conn, err := net.DialTimeout("tcp4", address, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	if err := conn.SetDeadline(time.Now().Add(3 * time.Second)); err != nil {
		t.Fatal(err)
	}
	return conn.(*net.TCPConn)
}

func newTestManager(t *testing.T, opts Options) (*Manager, *listenerFixture) {
	t.Helper()
	f := &listenerFixture{addrs: make(map[int]string)}
	opts.Listen = f.listen
	m, err := New(opts)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := m.Close(); err != nil {
			t.Error(err)
		}
	})
	return m, f
}

func applyTest(t *testing.T, m *Manager, proxies ...api.Proxy) {
	t.Helper()
	if err := m.Apply(context.Background(), proxies); err != nil {
		t.Fatal(err)
	}
}

func testProxy(port int) api.Proxy {
	return api.Proxy{Port: port, Via: testVia, Enabled: true}
}

type loopbackPeer struct {
	ln    net.Listener
	done  chan struct{}
	mu    sync.Mutex
	conns []net.Conn
	wg    sync.WaitGroup
}

func newPeer(t *testing.T, handle func(*net.TCPConn)) *loopbackPeer {
	t.Helper()
	ln, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	p := &loopbackPeer{ln: ln, done: make(chan struct{})}
	go func() {
		defer close(p.done)
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			p.mu.Lock()
			p.conns = append(p.conns, conn)
			p.mu.Unlock()
			p.wg.Add(1)
			go func() {
				defer p.wg.Done()
				defer conn.Close()
				_ = conn.SetDeadline(time.Now().Add(5 * time.Second))
				handle(conn.(*net.TCPConn))
			}()
		}
	}()
	t.Cleanup(func() {
		_ = p.ln.Close()
		<-p.done
		p.mu.Lock()
		for _, c := range p.conns {
			_ = c.Close()
		}
		p.mu.Unlock()
		p.wg.Wait()
	})
	return p
}

func (p *loopbackPeer) dial(ctx context.Context, _ api.InterfaceRef, _ Target) (net.Conn, error) {
	return (&net.Dialer{}).DialContext(ctx, "tcp4", p.ln.Addr().String())
}

func echoPeer(t *testing.T) *loopbackPeer {
	return newPeer(t, func(c *net.TCPConn) {
		_, _ = io.Copy(c, c)
		_ = c.CloseWrite()
	})
}

func requestBytes(target Target) []byte {
	b := []byte{5, 1, 0}
	if target.IsDomain {
		b = append(b, 3, byte(len(target.Host)))
		b = append(b, target.Host...)
	} else {
		ip := netip.MustParseAddr(target.Host)
		if ip.Is4() {
			b = append(b, 1)
			a := ip.As4()
			b = append(b, a[:]...)
		} else {
			b = append(b, 4)
			a := ip.As16()
			b = append(b, a[:]...)
		}
	}
	return binary.BigEndian.AppendUint16(b, target.Port)
}

func readReply(t *testing.T, conn net.Conn) (byte, netip.AddrPort) {
	t.Helper()
	var header [4]byte
	if _, err := io.ReadFull(conn, header[:]); err != nil {
		t.Fatal(err)
	}
	if header[0] != 5 || header[2] != 0 {
		t.Fatalf("bad reply header %x", header)
	}
	n := 4
	if header[3] == 4 {
		n = 16
	} else if header[3] != 1 {
		t.Fatalf("bad reply ATYP %d", header[3])
	}
	b := make([]byte, n+2)
	if _, err := io.ReadFull(conn, b); err != nil {
		t.Fatal(err)
	}
	ip, _ := netip.AddrFromSlice(b[:n])
	return header[1], netip.AddrPortFrom(ip, binary.BigEndian.Uint16(b[n:]))
}

func readMethod(t *testing.T, c net.Conn, want byte) {
	t.Helper()
	var response [2]byte
	if _, err := io.ReadFull(c, response[:]); err != nil || response != [2]byte{5, want} {
		t.Fatalf("authentication reply = %x, %v", response, err)
	}
}

func connectTest(t *testing.T, f *listenerFixture, port int) *net.TCPConn {
	t.Helper()
	c := f.dial(t, port)
	wire := append([]byte{5, 1, 0}, requestBytes(Target{Host: "192.0.2.9", Port: 443})...)
	if _, err := c.Write(wire); err != nil {
		t.Fatal(err)
	}
	readMethod(t, c, 0)
	if code, _ := readReply(t, c); code != 0 {
		t.Fatalf("CONNECT reply = %d", code)
	}
	return c
}

func roundTrip(t *testing.T, c net.Conn, body string) {
	t.Helper()
	if _, err := io.WriteString(c, body); err != nil {
		t.Fatal(err)
	}
	got := make([]byte, len(body))
	if _, err := io.ReadFull(c, got); err != nil || string(got) != body {
		t.Fatalf("relay = %q, %v", got, err)
	}
}

func waitFor(t *testing.T, condition func() bool) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for !condition() {
		if time.Now().After(deadline) {
			t.Fatal("timed out waiting for bounded worker state")
		}
		time.Sleep(time.Millisecond)
	}
}

func TestSOCKSTargetsAndFraming(t *testing.T) {
	for _, target := range []Target{
		{Host: "192.0.2.7", Port: 443},
		{Host: "2001:db8::7", Port: 8443},
		{Host: "example.com", Port: 443, IsDomain: true},
	} {
		for _, fragmented := range []bool{false, true} {
			t.Run(target.String()+"/fragmented="+strconv.FormatBool(fragmented), func(t *testing.T) {
				peer := echoPeer(t)
				type observation struct {
					target Target
					via    api.InterfaceRef
					local  netip.AddrPort
				}
				observed := make(chan observation, 1)
				m, f := newTestManager(t, Options{Dial: func(ctx context.Context, via api.InterfaceRef, got Target) (net.Conn, error) {
					out, err := peer.dial(ctx, via, got)
					if err == nil {
						observed <- observation{target: got, via: via, local: out.LocalAddr().(*net.TCPAddr).AddrPort()}
					}
					return out, err
				}})
				applyTest(t, m, testProxy(1080))
				c := f.dial(t, 1080)
				wire := append([]byte{5, 3, 2, 0, 1}, requestBytes(target)...)
				wire = append(wire, "coalesced application payload"...)
				if fragmented {
					for _, b := range wire {
						if _, err := c.Write([]byte{b}); err != nil {
							t.Fatal(err)
						}
					}
				} else if _, err := c.Write(wire); err != nil {
					t.Fatal(err)
				}
				readMethod(t, c, 0)
				code, bound := readReply(t, c)
				got := <-observed
				if code != 0 || bound != got.local || got.target != target || got.via != testVia {
					t.Fatalf("CONNECT result code=%d bound=%v observation=%+v", code, bound, got)
				}
				body := make([]byte, len("coalesced application payload"))
				if _, err := io.ReadFull(c, body); err != nil || string(body) != "coalesced application payload" {
					t.Fatalf("application payload consumed by handshake: %q %v", body, err)
				}
				roundTrip(t, c, "second payload")
			})
		}
	}
}

func TestSOCKSRejectsUnsupportedAndMalformedRequests(t *testing.T) {
	var dials atomic.Int64
	m, f := newTestManager(t, Options{Dial: func(context.Context, api.InterfaceRef, Target) (net.Conn, error) {
		dials.Add(1)
		return nil, errors.New("must not dial")
	}})
	applyTest(t, m, testProxy(1080))
	for _, tc := range []struct {
		name string
		wire []byte
		code byte
	}{
		{"bind", []byte{5, 2, 0, 1}, 7},
		{"udp", []byte{5, 3, 0, 1}, 7},
		{"unknown command", []byte{5, 9, 0, 1}, 7},
		{"unknown address", []byte{5, 1, 0, 9}, 8},
		{"reserved", []byte{5, 1, 1, 1}, 1},
		{"wrong version", []byte{4, 1, 0, 1}, 1},
		{"empty domain", []byte{5, 1, 0, 3, 0}, 4},
		{"domain NUL", []byte{5, 1, 0, 3, 2, 'a', 0}, 4},
		{"domain path", []byte{5, 1, 0, 3, 3, 'a', '/', 'b'}, 4},
		{"zero port", requestBytes(Target{Host: "192.0.2.7"}), 4},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := f.dial(t, 1080)
			if _, err := c.Write(append([]byte{5, 1, 0}, tc.wire...)); err != nil {
				t.Fatal(err)
			}
			readMethod(t, c, 0)
			if code, _ := readReply(t, c); code != tc.code {
				t.Fatalf("reply=%d want=%d", code, tc.code)
			}
			var b [1]byte
			if _, err := c.Read(b[:]); err == nil {
				t.Fatal("failed request remained open")
			}
		})
	}
	for _, greeting := range [][]byte{{5, 0}, {5, 2, 1, 2}} {
		c := f.dial(t, 1080)
		if _, err := c.Write(greeting); err != nil {
			t.Fatal(err)
		}
		readMethod(t, c, 0xff)
	}
	if dials.Load() != 0 {
		t.Fatal("malformed request reached dialer")
	}
}

func TestHandshakeAndConnectTimeouts(t *testing.T) {
	m, f := newTestManager(t, Options{HandshakeTimeout: 40 * time.Millisecond, ConnectTimeout: 100 * time.Millisecond, Dial: func(ctx context.Context, _ api.InterfaceRef, _ Target) (net.Conn, error) {
		<-ctx.Done()
		return nil, ctx.Err()
	}})
	applyTest(t, m, testProxy(1080))
	c := f.dial(t, 1080)
	start := time.Now()
	if _, err := c.Write([]byte{5, 2, 0}); err != nil {
		t.Fatal(err)
	}
	var b [1]byte
	if _, err := c.Read(b[:]); err == nil || time.Since(start) > time.Second {
		t.Fatal("incomplete method list was not bounded", err)
	}
	c = f.dial(t, 1080)
	if _, err := c.Write(append([]byte{5, 1, 0}, requestBytes(Target{Host: "192.0.2.7", Port: 443})...)); err != nil {
		t.Fatal(err)
	}
	readMethod(t, c, 0)
	// The CONNECT deadline also bounds the reply socket. Either a host-unreachable
	// reply or a closed socket is valid when the entire deadline has expired.
	var header [4]byte
	_, err := io.ReadFull(c, header[:])
	if err == nil && header[1] != 4 {
		t.Fatalf("connect timeout reply = %x", header)
	}
	waitFor(t, func() bool { m.mu.Lock(); defer m.mu.Unlock(); return m.total == 0 })
}

func TestConnectionLimitsIncludePendingHandshakes(t *testing.T) {
	peer := echoPeer(t)
	m, f := newTestManager(t, Options{Dial: peer.dial, MaxPerPort: 1, MaxTotal: 2})
	applyTest(t, m, testProxy(1080), testProxy(1081), testProxy(1082))
	pending := f.dial(t, 1080)
	waitFor(t, func() bool { m.mu.Lock(); defer m.mu.Unlock(); return m.total == 1 })
	rejected := f.dial(t, 1080)
	var b [1]byte
	if _, err := rejected.Read(b[:]); err == nil {
		t.Fatal("per-port cap did not reject handshake")
	}
	other := f.dial(t, 1081)
	waitFor(t, func() bool { m.mu.Lock(); defer m.mu.Unlock(); return m.total == 2 })
	rejected = f.dial(t, 1082)
	if _, err := rejected.Read(b[:]); err == nil {
		t.Fatal("global cap did not reject handshake")
	}
	_ = pending.Close()
	_ = other.Close()
	waitFor(t, func() bool { m.mu.Lock(); defer m.mu.Unlock(); return m.total == 0 })
	c := connectTest(t, f, 1082)
	roundTrip(t, c, "capacity returned")
}

func TestHalfCloseAndApplicationByteCounters(t *testing.T) {
	peer := newPeer(t, func(c *net.TCPConn) {
		b, err := io.ReadAll(c)
		if err == nil {
			_, _ = c.Write(append([]byte("reply:"), b...))
			_ = c.CloseWrite()
		}
	})
	m, f := newTestManager(t, Options{Dial: peer.dial})
	applyTest(t, m, testProxy(1080))
	c := connectTest(t, f, 1080)
	body := bytes.Repeat([]byte("payload"), 10000)
	if _, err := c.Write(body); err != nil {
		t.Fatal(err)
	}
	if err := c.CloseWrite(); err != nil {
		t.Fatal(err)
	}
	got, err := io.ReadAll(c)
	if err != nil || !bytes.Equal(got, append([]byte("reply:"), body...)) {
		t.Fatal("client half-close lost response", len(got), err)
	}
	waitFor(t, func() bool {
		st := m.Status(context.Background())[0]
		return st.Open == 0 && st.Sent == uint64(len(body)) && st.Received == uint64(len(got))
	})
	st := m.Status(context.Background())[0]
	if st.Since.IsZero() || !st.Listening {
		t.Fatal("closed stream stopped listener", st)
	}
}

func TestRemoteHalfClosePreservesUpload(t *testing.T) {
	got := make(chan string, 1)
	peer := newPeer(t, func(c *net.TCPConn) {
		_, _ = io.WriteString(c, "ready")
		_ = c.CloseWrite()
		b, _ := io.ReadAll(c)
		got <- string(b)
	})
	m, f := newTestManager(t, Options{Dial: peer.dial})
	applyTest(t, m, testProxy(1080))
	c := connectTest(t, f, 1080)
	b, err := io.ReadAll(c)
	if err != nil || string(b) != "ready" {
		t.Fatal("remote half-close failed", string(b), err)
	}
	if _, err := io.WriteString(c, "upload after EOF"); err != nil {
		t.Fatal(err)
	}
	if err := c.CloseWrite(); err != nil {
		t.Fatal(err)
	}
	select {
	case b := <-got:
		if b != "upload after EOF" {
			t.Fatal(b)
		}
	case <-time.After(time.Second):
		t.Fatal("upload after EOF did not complete")
	}
}

func TestApplyClosesOnlyAffectedListeners(t *testing.T) {
	for _, change := range []string{"via", "disable", "remove"} {
		t.Run(change, func(t *testing.T) {
			peer := echoPeer(t)
			m, f := newTestManager(t, Options{Dial: peer.dial})
			a, b := testProxy(1080), testProxy(1081)
			applyTest(t, m, a, b)
			first, second := connectTest(t, f, 1080), connectTest(t, f, 1081)
			roundTrip(t, first, "old")
			roundTrip(t, second, "unchanged")
			old := m.Status(context.Background())[1]
			b.Via.Label = "Changed display label"
			switch change {
			case "via":
				a.Via.Name = "example1"
				a.Via.HardwareAddr = "02:00:00:00:00:02"
				applyTest(t, m, b, a)
			case "disable":
				a.Enabled = false
				applyTest(t, m, b, a)
			case "remove":
				applyTest(t, m, b)
			}
			var data [1]byte
			if _, err := first.Read(data[:]); err == nil || !errors.Is(err, syscall.ECONNRESET) {
				t.Fatalf("affected TCP was not reset: %v", err)
			}
			roundTrip(t, second, "still open")
			st := m.Status(context.Background())
			if st[0].Port != 1081 || st[0].Since != old.Since || st[0].Open != 1 {
				t.Fatal("unrelated listener restarted", st)
			}
			if change == "disable" && (st[1].Listening || st[1].Open != 0 || st[1].Sent != 0 || st[1].Received != 0 || !st[1].Since.IsZero() || st[1].Error != nil) {
				t.Fatal("disabled state retained listener counters", st[1])
			}
			if change == "via" {
				fresh := connectTest(t, f, 1080)
				roundTrip(t, fresh, "new route")
				if st[1].Sent != 0 || st[1].Received != 0 {
					t.Fatal("new generation retained counters", st[1])
				}
			}
		})
	}
}

func TestApplyCancelsLateDialWithoutPublishing(t *testing.T) {
	peer := echoPeer(t)
	entered, canceled, release := make(chan struct{}), make(chan struct{}), make(chan struct{})
	outClosed := make(chan struct{})
	var once sync.Once
	m, f := newTestManager(t, Options{Dial: func(ctx context.Context, via api.InterfaceRef, target Target) (net.Conn, error) {
		once.Do(func() { close(entered) })
		<-ctx.Done()
		close(canceled)
		<-release
		out, err := peer.dial(context.Background(), via, target)
		if err != nil {
			return nil, err
		}
		return &closeObservedConn{TCPConn: out.(*net.TCPConn), closed: outClosed}, nil
	}})
	t.Cleanup(func() {
		select {
		case <-release:
		default:
			close(release)
		}
	})
	a := testProxy(1080)
	applyTest(t, m, a)
	c := f.dial(t, 1080)
	_, _ = c.Write(append([]byte{5, 1, 0}, requestBytes(Target{Host: "192.0.2.7", Port: 443})...))
	readMethod(t, c, 0)
	<-entered
	a.Via.Name = "example1"
	applyTest(t, m, a)
	<-canceled
	close(release)
	waitFor(t, func() bool { m.mu.Lock(); defer m.mu.Unlock(); return m.total == 0 })
	select {
	case <-outClosed:
	default:
		t.Fatal("late outbound connection was not closed")
	}
	var b [1]byte
	if _, err := c.Read(b[:]); err == nil {
		t.Fatal("old dial published a reply")
	}
	st := m.Status(context.Background())[0]
	if st.Open != 0 || st.Sent != 0 || st.Received != 0 || !st.Listening {
		t.Fatal("late dial changed replacement status", st)
	}
}

type closeObservedConn struct {
	*net.TCPConn
	closed chan struct{}
	once   sync.Once
}

func (c *closeObservedConn) Close() error {
	c.once.Do(func() { close(c.closed) })
	return c.TCPConn.Close()
}

func TestCloseCancelsAndJoinsPendingDial(t *testing.T) {
	entered, exited := make(chan struct{}), make(chan struct{})
	m, f := newTestManager(t, Options{Dial: func(ctx context.Context, _ api.InterfaceRef, _ Target) (net.Conn, error) {
		close(entered)
		<-ctx.Done()
		defer close(exited)
		return nil, ctx.Err()
	}})
	applyTest(t, m, testProxy(1080))
	c := f.dial(t, 1080)
	_, _ = c.Write(append([]byte{5, 1, 0}, requestBytes(Target{Host: "192.0.2.7", Port: 443})...))
	readMethod(t, c, 0)
	<-entered
	if err := m.Close(); err != nil {
		t.Fatal(err)
	}
	select {
	case <-exited:
	default:
		t.Fatal("Close did not join dialer")
	}
	if err := m.Close(); err != nil {
		t.Fatal(err)
	}
	if !errors.Is(m.Apply(context.Background(), nil), net.ErrClosed) {
		t.Fatal("closed manager accepted configuration")
	}
}

func TestBindFailureIsIsolatedAndRetryable(t *testing.T) {
	occupied, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = occupied.Close() })
	port := occupied.Addr().(*net.TCPAddr).Port
	m, err := New(Options{Dial: func(context.Context, api.InterfaceRef, Target) (net.Conn, error) {
		return nil, errors.New("not dialed")
	}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = m.Close() })
	applyTest(t, m, testProxy(port))
	st := m.Status(context.Background())[0]
	if st.Listening || st.Error == nil || st.Error.Code != api.ErrPortInUse || !st.Since.IsZero() {
		t.Fatal("occupied port did not retain useful status", st)
	}
	_ = occupied.Close()
	applyTest(t, m, testProxy(port))
	st = m.Status(context.Background())[0]
	if !st.Listening || st.Error != nil || st.Since.IsZero() {
		t.Fatal("same configuration did not retry bind", st)
	}
}

func TestOneFailedBindDoesNotRollbackOtherPorts(t *testing.T) {
	f := &listenerFixture{addrs: make(map[int]string)}
	m, err := New(Options{
		Dial: func(context.Context, api.InterfaceRef, Target) (net.Conn, error) {
			return nil, errors.New("not dialed")
		},
		Listen: func(ctx context.Context, network, address string) (net.Listener, error) {
			if address == "127.0.0.1:1080" {
				return nil, syscall.EADDRINUSE
			}
			return f.listen(ctx, network, address)
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = m.Close() })
	applyTest(t, m, testProxy(1080), testProxy(1081))
	states := m.Status(context.Background())
	if len(states) != 2 || states[0].Port != 1080 || states[0].Error == nil || states[0].Error.Code != api.ErrPortInUse || !states[1].Listening || states[1].Error != nil {
		t.Fatal("failed bind affected another configured port", states)
	}
	c := f.dial(t, 1081)
	if _, err := c.Write([]byte{5, 1, 0}); err != nil {
		t.Fatal(err)
	}
	readMethod(t, c, 0)
}

func TestHealthDoesNotGateLiteralConnect(t *testing.T) {
	peer := echoPeer(t)
	m, f := newTestManager(t, Options{Dial: peer.dial, Health: func(context.Context, api.InterfaceRef) error {
		return &api.Error{Code: api.ErrDNSUnavailable, Detail: "no selected DNS servers"}
	}})
	applyTest(t, m, testProxy(1080))
	st := m.Status(context.Background())[0]
	if !st.Listening || st.Error == nil || st.Error.Code != api.ErrDNSUnavailable {
		t.Fatal(st)
	}
	c := connectTest(t, f, 1080)
	roundTrip(t, c, "literal still works")
}

func TestConcurrentApplyStatusAndClose(t *testing.T) {
	peer := echoPeer(t)
	m, f := newTestManager(t, Options{Dial: peer.dial})
	applyTest(t, m, testProxy(1080))
	c := connectTest(t, f, 1080)
	var wg sync.WaitGroup
	for i := range 4 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := range 100 {
				p := testProxy(1080)
				p.Via.Label = strconv.Itoa(i + j)
				if err := m.Apply(context.Background(), []api.Proxy{p}); err != nil && !errors.Is(err, net.ErrClosed) {
					t.Error(err)
				}
				_ = m.Status(context.Background())
			}
		}()
	}
	roundTrip(t, c, "concurrent metadata updates")
	if err := m.Close(); err != nil {
		t.Fatal(err)
	}
	wg.Wait()
	st := m.Status(context.Background())[0]
	if st.Listening || st.Open != 0 {
		t.Fatal("workers survived Close", st)
	}
}

func TestDialErrorReplyCodes(t *testing.T) {
	for _, tc := range []struct {
		err  error
		code byte
	}{
		{&api.Error{Code: api.ErrInterfaceUnavailable}, 3}, {&api.Error{Code: api.ErrDNSUnavailable}, 4}, {&api.Error{Code: api.ErrUnsupported}, 2},
		{syscall.ECONNREFUSED, 5}, {syscall.ENETUNREACH, 3}, {syscall.EHOSTUNREACH, 4}, {syscall.EACCES, 2}, {context.DeadlineExceeded, 4}, {errors.New("unknown"), 1},
	} {
		if code := dialReply(tc.err); code != tc.code {
			t.Fatalf("reply(%v)=%d want %d", tc.err, code, tc.code)
		}
	}
}

func TestReplyUsesActualIPv4AndIPv6BoundAddresses(t *testing.T) {
	for _, address := range []string{"192.0.2.4:31000", "[2001:db8::4]:31000"} {
		t.Run(address, func(t *testing.T) {
			want := netip.MustParseAddrPort(address)
			client, server := net.Pipe()
			defer client.Close()
			defer server.Close()
			_ = client.SetDeadline(time.Now().Add(time.Second))
			_ = server.SetDeadline(time.Now().Add(time.Second))
			done := make(chan error, 1)
			go func() { done <- writeReply(server, 0, net.TCPAddrFromAddrPort(want)) }()
			code, got := readReply(t, client)
			if err := <-done; err != nil || code != 0 || got != want {
				t.Fatalf("bound address = %v code=%d err=%v", got, code, err)
			}
		})
	}
}

func TestTruncatedAddressDoesNotReachDialer(t *testing.T) {
	var dials atomic.Int64
	m, f := newTestManager(t, Options{Dial: func(context.Context, api.InterfaceRef, Target) (net.Conn, error) {
		dials.Add(1)
		return nil, errors.New("must not dial")
	}})
	applyTest(t, m, testProxy(1080))
	for _, wire := range [][]byte{
		{5, 1, 0, 1, 192, 0},
		{5, 1, 0, 4, 0x20, 1, 0x0d, 0xb8},
		{5, 1, 0, 3, 11, 'e', 'x'},
		{5, 1, 0, 1, 192, 0, 2, 1, 0},
	} {
		c := f.dial(t, 1080)
		if _, err := c.Write(append([]byte{5, 1, 0}, wire...)); err != nil {
			t.Fatal(err)
		}
		if err := c.CloseWrite(); err != nil {
			t.Fatal(err)
		}
		readMethod(t, c, 0)
		var b [1]byte
		if _, err := c.Read(b[:]); err == nil {
			t.Fatal("truncated address remained open")
		}
	}
	if dials.Load() != 0 {
		t.Fatal("truncated address reached the dialer")
	}
}
