package egress

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net"
	"net/netip"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Kel077/pith/internal/api"
	"github.com/Kel077/pith/internal/netif"
	"github.com/Kel077/pith/internal/proxy"
)

func testInterface() api.Interface {
	return api.Interface{Name: "example0", Kind: api.KindEthernet, HardwareAddr: "02:00:00:00:00:01", Up: true,
		IPv4: []string{"192.0.2.2"}, IPv6: []string{"2001:db8::2"}, DNS: []string{"192.0.2.53", "2001:db8::53"}}
}

func testRef() api.InterfaceRef {
	iface := testInterface()
	return api.InterfaceRef{Name: iface.Name, HardwareAddr: iface.HardwareAddr}
}

type testNetwork struct {
	mu       sync.Mutex
	metadata []api.Interface
	system   []netip.Addr // the Mac's DNS servers
	links    map[int]net.Interface
	dials    []dialCall
	lists    int
	dial     func(context.Context, string, string, string) (net.Conn, error)
}

type dialCall struct{ name, network, address string }

func newTestNetwork() *testNetwork {
	iface := testInterface()
	mac, _ := net.ParseMAC(iface.HardwareAddr)
	return &testNetwork{metadata: []api.Interface{iface}, links: map[int]net.Interface{7: {Index: 7, Name: iface.Name, HardwareAddr: mac, Flags: net.FlagUp | net.FlagRunning}}}
}

func (f *testNetwork) options() Options {
	return Options{
		Interfaces: func(ctx context.Context) ([]api.Interface, error) {
			f.mu.Lock()
			defer f.mu.Unlock()
			f.lists++
			return cloneInterfaces(f.metadata), ctx.Err()
		},
		InterfaceByName: func(name string) (*net.Interface, error) {
			f.mu.Lock()
			defer f.mu.Unlock()
			for _, link := range f.links {
				if link.Name == name {
					copy := link
					return &copy, nil
				}
			}
			return nil, errors.New("missing interface")
		},
		InterfaceByIndex: func(index int) (*net.Interface, error) {
			f.mu.Lock()
			defer f.mu.Unlock()
			if link, ok := f.links[index]; ok {
				copy := link
				return &copy, nil
			}
			return nil, errors.New("missing interface")
		},
		SystemDNS: func() []netip.Addr {
			f.mu.Lock()
			defer f.mu.Unlock()
			return f.system
		},
		DialContext: func(ctx context.Context, name, network, address string) (net.Conn, error) {
			f.mu.Lock()
			f.dials = append(f.dials, dialCall{name, network, address})
			dial := f.dial
			f.mu.Unlock()
			if name == "" {
				return nil, errors.New("empty interface")
			}
			if _, err := netip.ParseAddrPort(address); err != nil {
				return nil, errors.New("nonliteral endpoint")
			}
			if dial != nil {
				return dial(ctx, name, network, address)
			}
			return newTestConn(nil), nil
		},
	}
}

type testConn struct {
	mu                    sync.Mutex
	read                  bytes.Buffer
	written               bytes.Buffer
	closed                bool
	closeWrite, closeRead int
	linger                int
	onWrite               func([]byte) ([]byte, error)
}

func newTestConn(read []byte) *testConn { c := &testConn{}; c.read.Write(read); return c }
func (c *testConn) Read(p []byte) (int, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed {
		return 0, net.ErrClosed
	}
	return c.read.Read(p)
}
func (c *testConn) Write(p []byte) (int, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed {
		return 0, net.ErrClosed
	}
	if c.onWrite != nil {
		response, err := c.onWrite(p)
		if err != nil {
			return 0, err
		}
		c.read.Write(response)
	}
	return c.written.Write(p)
}
func (c *testConn) Close() error                   { c.mu.Lock(); defer c.mu.Unlock(); c.closed = true; return nil }
func (*testConn) LocalAddr() net.Addr              { return &net.TCPAddr{IP: net.IPv4(192, 0, 2, 2), Port: 40000} }
func (*testConn) RemoteAddr() net.Addr             { return &net.TCPAddr{IP: net.IPv4(203, 0, 113, 1), Port: 443} }
func (*testConn) SetDeadline(time.Time) error      { return nil }
func (*testConn) SetReadDeadline(time.Time) error  { return nil }
func (*testConn) SetWriteDeadline(time.Time) error { return nil }
func (c *testConn) CloseWrite() error              { c.mu.Lock(); defer c.mu.Unlock(); c.closeWrite++; return nil }
func (c *testConn) CloseRead() error               { c.mu.Lock(); defer c.mu.Unlock(); c.closeRead++; return nil }
func (c *testConn) SetLinger(v int) error          { c.mu.Lock(); defer c.mu.Unlock(); c.linger = v; return nil }

func literalTarget() proxy.Target { return proxy.Target{Host: "203.0.113.1", Port: 443} }

func TestInterfaceResolution(t *testing.T) {
	for _, mode := range []string{"exact", "renamed", "ambiguous", "missing", "down", "kernel-down", "mac-replaced", "vpn", "utun", "ipsec", "ppp", "bridge", "other-kind", "loopback", "no-dns"} {
		t.Run(mode, func(t *testing.T) {
			f := newTestNetwork()
			ref := testRef()
			wantError := api.ErrInterfaceUnavailable
			switch mode {
			case "renamed":
				f.metadata[0].Name = "example1"
				link := f.links[7]
				link.Name = "example1"
				f.links[7] = link
			case "ambiguous":
				f.metadata = append(f.metadata, f.metadata[0])
				f.metadata[1].Name = "example1"
				ref.Name = "absent"
			case "missing":
				f.metadata = nil
			case "down":
				f.metadata[0].Up = false
			case "kernel-down":
				link := f.links[7]
				link.Flags &^= net.FlagRunning
				f.links[7] = link
			case "mac-replaced":
				link := f.links[7]
				link.HardwareAddr, _ = net.ParseMAC("02:00:00:00:00:02")
				f.links[7] = link
			case "vpn":
				f.metadata[0].Kind = api.KindVPN
				wantError = api.ErrUnsupported
			case "utun", "ipsec", "ppp":
				f.metadata[0].Name = mode + "-example"
				ref.Name = f.metadata[0].Name
				wantError = api.ErrUnsupported
			case "bridge":
				f.metadata[0].Name = "bridge0"
				ref.Name = "bridge0"
				wantError = api.ErrUnsupported
			case "other-kind":
				f.metadata[0].Kind = api.KindOther
				wantError = api.ErrUnsupported
			case "loopback":
				link := f.links[7]
				link.Flags |= net.FlagLoopback
				f.links[7] = link
			case "no-dns":
				f.metadata[0].DNS = nil
			}
			n := New(f.options())
			conn, err := n.Dial(context.Background(), ref, literalTarget())
			if mode == "exact" || mode == "renamed" || mode == "no-dns" {
				if err != nil {
					t.Fatal(err)
				}
				conn.Close()
				if mode == "no-dns" && api.Code(n.Health(context.Background(), ref)) != api.ErrDNSUnavailable {
					t.Fatal("Health did not report missing DNS")
				}
			} else {
				if conn != nil || api.Code(err) != wantError {
					t.Fatalf("conn=%v error=%v, want %v", conn, err, wantError)
				}
				if code := api.Code(n.Health(context.Background(), ref)); code != wantError {
					t.Fatalf("Health error=%v, want %v", code, wantError)
				}
			}
			if mode != "exact" && mode != "renamed" && mode != "no-dns" && len(f.dials) != 0 {
				t.Fatal("unavailable interface was dialed")
			}
		})
	}
}

func TestBindingGuardsReuseAndPreservesSocketMethods(t *testing.T) {
	for _, mode := range []string{"index", "name", "mac", "down", "unchanged"} {
		t.Run(mode, func(t *testing.T) {
			f := newTestNetwork()
			raw := newTestConn([]byte("response"))
			f.dial = func(ctx context.Context, name, network, address string) (net.Conn, error) {
				if err := netif.ValidateBinding(ctx, 7); err != nil {
					return nil, err
				}
				if netif.ValidateBinding(ctx, 8) == nil {
					t.Error("wrong socket index accepted")
				}
				return raw, nil
			}
			conn, err := New(f.options()).Dial(context.Background(), testRef(), literalTarget())
			if err != nil {
				t.Fatal(err)
			}
			defer conn.Close()
			if err := conn.(interface{ CloseWrite() error }).CloseWrite(); err != nil {
				t.Fatal(err)
			}
			if err := conn.(interface{ CloseRead() error }).CloseRead(); err != nil {
				t.Fatal(err)
			}
			if err := conn.(interface{ SetLinger(int) error }).SetLinger(0); err != nil {
				t.Fatal(err)
			}
			f.mu.Lock()
			link := f.links[7]
			switch mode {
			case "index":
				delete(f.links, 7)
				link.Index = 8
				f.links[8] = link
			case "name":
				link.Name = "unrelated"
				f.links[7] = link
			case "mac":
				link.HardwareAddr, _ = net.ParseMAC("02:00:00:00:00:02")
				f.links[7] = link
			case "down":
				link.Flags = 0
				f.links[7] = link
			}
			f.mu.Unlock()
			_, err = conn.Write([]byte("request"))
			if mode == "unchanged" {
				if err != nil {
					t.Fatal(err)
				}
			} else {
				if api.Code(err) != api.ErrInterfaceUnavailable {
					t.Fatalf("reuse error=%v", err)
				}
				if _, err := conn.Read(make([]byte, 8)); api.Code(err) != api.ErrInterfaceUnavailable {
					t.Fatalf("read after reuse=%v", err)
				}
				raw.mu.Lock()
				closed, written := raw.closed, raw.written.Len()
				raw.mu.Unlock()
				if !closed || written != 0 {
					t.Fatal("reused interface accepted I/O or did not close")
				}
			}
		})
	}
}

func TestLateDialIsClosed(t *testing.T) {
	for _, mode := range []string{"canceled", "reused", "error-with-connection"} {
		t.Run(mode, func(t *testing.T) {
			f := newTestNetwork()
			raw := newTestConn(nil)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			f.dial = func(context.Context, string, string, string) (net.Conn, error) {
				switch mode {
				case "canceled":
					cancel()
				case "reused":
					f.mu.Lock()
					link := f.links[7]
					link.Name = "unrelated"
					f.links[7] = link
					f.mu.Unlock()
				case "error-with-connection":
					return raw, errors.New("dial failed")
				}
				return raw, nil
			}
			conn, err := New(f.options()).Dial(ctx, testRef(), literalTarget())
			if err == nil || conn != nil || !raw.closed {
				t.Fatalf("late connection survived: %v %v closed=%v", conn, err, raw.closed)
			}
		})
	}
}

func TestBindingChangesAfterReadDiscardData(t *testing.T) {
	f := newTestNetwork()
	left, right := net.Pipe()
	defer right.Close()
	f.dial = func(context.Context, string, string, string) (net.Conn, error) { return left, nil }
	conn, err := New(f.options()).Dial(context.Background(), testRef(), literalTarget())
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	done := make(chan struct{})
	go func() {
		defer close(done)
		f.mu.Lock()
		link := f.links[7]
		link.Name = "unrelated"
		f.links[7] = link
		f.mu.Unlock()
		right.Write([]byte("must not relay"))
	}()
	conn.SetDeadline(time.Now().Add(time.Second))
	count, err := conn.Read(make([]byte, 32))
	right.Close()
	<-done
	if count != 0 || api.Code(err) != api.ErrInterfaceUnavailable {
		t.Fatalf("read count=%d error=%v", count, err)
	}
}

func TestNewBindingUsesCurrentIndex(t *testing.T) {
	f := newTestNetwork()
	n := New(f.options())
	if err := n.Health(context.Background(), testRef()); err != nil {
		t.Fatal(err)
	}
	f.mu.Lock()
	link := f.links[7]
	delete(f.links, 7)
	link.Index = 9
	f.links[9] = link
	f.mu.Unlock()
	f.dial = func(ctx context.Context, _, _, _ string) (net.Conn, error) {
		return newTestConn(nil), netif.ValidateBinding(ctx, 9)
	}
	conn, err := n.Dial(context.Background(), testRef(), literalTarget())
	if err != nil {
		t.Fatal(err)
	}
	conn.Close()
	if f.lists != 1 {
		t.Fatalf("metadata was unnecessarily reread: %d", f.lists)
	}
}

func TestAddressAndDNSChangesDoNotRevokeExistingBinding(t *testing.T) {
	f := newTestNetwork()
	opts := f.options()
	opts.MetadataTTL = -1
	n := New(opts)
	conn, err := n.Dial(context.Background(), testRef(), literalTarget())
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	f.mu.Lock()
	f.metadata[0].IPv4 = []string{"198.51.100.2"}
	f.metadata[0].IPv6 = []string{"2001:db8:1::2"}
	f.metadata[0].DNS = []string{"198.51.100.53"}
	f.mu.Unlock()
	if _, err := conn.Write([]byte("still the same physical interface")); err != nil {
		t.Fatal(err)
	}
	b, err := n.bind(context.Background(), testRef())
	if err != nil || b.iface.DNS[0] != "198.51.100.53" {
		t.Fatalf("fresh binding did not refresh DNS: %v %v", b.iface, err)
	}
}

func TestMetadataCacheAndCancellation(t *testing.T) {
	f := newTestNetwork()
	opts := f.options()
	opts.MetadataTTL = 10 * time.Minute
	var calls atomic.Int32
	started, release := make(chan struct{}), make(chan struct{})
	opts.Interfaces = func(ctx context.Context) ([]api.Interface, error) {
		if calls.Add(1) == 1 {
			close(started)
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-release:
			return []api.Interface{testInterface()}, nil
		}
	}
	n := New(opts)
	if n.opts.MetadataTTL != time.Second {
		t.Fatal("metadata cache exceeded one second")
	}
	result := make(chan error, 1)
	go func() { result <- n.Health(context.Background(), testRef()) }()
	<-started
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Millisecond)
	defer cancel()
	if err := n.Health(ctx, testRef()); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("waiting cache caller=%v", err)
	}
	close(release)
	if err := <-result; err != nil {
		t.Fatal(err)
	}
	for range 5 {
		if err := n.Health(context.Background(), testRef()); err != nil {
			t.Fatal(err)
		}
	}
	if calls.Load() != 1 {
		t.Fatalf("uncached metadata queries=%d", calls.Load())
	}
}

func TestCanceledMetadataOwnerDoesNotPoisonHealthyWaiter(t *testing.T) {
	for _, reason := range []string{"canceled", "deadline"} {
		for _, operation := range []string{"health", "dial"} {
			t.Run(reason+"/"+operation, func(t *testing.T) {
				f := newTestNetwork()
				opts := f.options()
				var calls atomic.Int32
				started := make(chan struct{})
				opts.Interfaces = func(ctx context.Context) ([]api.Interface, error) {
					if calls.Add(1) == 1 {
						close(started)
						<-ctx.Done()
						return nil, ctx.Err()
					}
					return []api.Interface{testInterface()}, ctx.Err()
				}
				n := New(opts)
				ownerCtx, ownerCancel := context.WithCancel(context.Background())
				wantErr := context.Canceled
				if reason == "deadline" {
					ownerCancel()
					ownerCtx, ownerCancel = context.WithTimeout(context.Background(), 30*time.Millisecond)
					wantErr = context.DeadlineExceeded
				}
				defer ownerCancel()
				ownerDone := make(chan error, 1)
				go func() { ownerDone <- n.Health(ownerCtx, testRef()) }()
				<-started

				waiterCtx, waiterCancel := context.WithTimeout(context.Background(), time.Second)
				defer waiterCancel()
				waiterStarted, waiterDone := make(chan struct{}), make(chan error, 1)
				go func() {
					close(waiterStarted)
					if operation == "health" {
						waiterDone <- n.Health(waiterCtx, testRef())
						return
					}
					conn, err := n.Dial(waiterCtx, testRef(), literalTarget())
					if conn != nil {
						conn.Close()
					}
					waiterDone <- err
				}()
				<-waiterStarted
				if reason == "canceled" {
					ownerCancel()
				}
				ownerErr, waiterErr := <-ownerDone, <-waiterDone
				if !errors.Is(ownerErr, wantErr) {
					t.Fatalf("owner error=%v, want %v", ownerErr, wantErr)
				}
				if waiterErr != nil {
					t.Fatalf("healthy waiter inherited owner failure: %v", waiterErr)
				}
				if calls.Load() != 2 {
					t.Fatalf("healthy waiter did not refresh once: %d reads", calls.Load())
				}
				if err := n.Health(context.Background(), testRef()); err != nil || calls.Load() != 2 {
					t.Fatalf("replacement snapshot was not cached: error=%v reads=%d", err, calls.Load())
				}
			})
		}
	}
}

func TestNoDNSDoesNotUseSystemResolver(t *testing.T) {
	f := newTestNetwork()
	f.metadata[0].DNS = []string{"127.0.0.1", "::1", "0.0.0.0", "::", "224.0.0.1", "ff02::1", "resolver.example.com", "fe80::1%unrelated"}
	n := New(f.options())
	conn, err := n.Dial(context.Background(), testRef(), proxy.Target{Host: "example.com", Port: 443, IsDomain: true})
	if conn != nil || api.Code(err) != api.ErrDNSUnavailable || len(f.dials) != 0 {
		t.Fatalf("DNS fallback: %v %v %v", conn, err, f.dials)
	}
}

func TestDomainValidationAndLiteralFamilies(t *testing.T) {
	for _, name := range []string{"", ".", "bad..example.com", "-bad.example.com", "bad-.example.com", "bad_name.example.com", "example.com:443", "example.com/path", strings.Repeat("a", 64) + ".example.com", "example.com\x00"} {
		if _, err := domainName(name); err == nil {
			t.Fatalf("accepted invalid name %q", name)
		}
	}
	if name, err := domainName("ExAmPle.COM."); err != nil || name != "example.com." {
		t.Fatalf("name=%q err=%v", name, err)
	}
	for _, address := range []string{"203.0.113.1", "2001:db8::1", "fe80::1"} {
		f := newTestNetwork()
		conn, err := New(f.options()).Dial(context.Background(), testRef(), proxy.Target{Host: address, Port: 443})
		if err != nil {
			t.Fatal(err)
		}
		conn.Close()
		if len(f.dials) != 1 || f.dials[0].name != "example0" {
			t.Fatal(f.dials)
		}
		if strings.Contains(address, ":") && f.dials[0].network != "tcp6" {
			t.Fatal(f.dials)
		}
	}
	for _, address := range []string{"127.0.0.1", "::1", "0.0.0.0", "::", "224.0.0.1", "ff02::1", "fe80::1%unrelated", "example.com"} {
		f := newTestNetwork()
		conn, err := New(f.options()).Dial(context.Background(), testRef(), proxy.Target{Host: address, Port: 443})
		if conn != nil || err == nil || len(f.dials) != 0 {
			t.Fatalf("unsafe literal %q was dialed", address)
		}
	}
}

func TestConcurrentBoundIOAndClose(t *testing.T) {
	f := newTestNetwork()
	raw := newTestConn(bytes.Repeat([]byte("response"), 128))
	f.dial = func(context.Context, string, string, string) (net.Conn, error) { return raw, nil }
	conn, err := New(f.options()).Dial(context.Background(), testRef(), literalTarget())
	if err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	for range 8 {
		wg.Go(func() {
			for range 50 {
				_, err := conn.Write([]byte("request"))
				if err != nil && !errors.Is(err, net.ErrClosed) && api.Code(err) != api.ErrInterfaceUnavailable {
					t.Error(err)
					return
				}
			}
		})
		wg.Go(func() {
			for range 50 {
				_, err := conn.Read(make([]byte, 8))
				if err != nil && !errors.Is(err, io.EOF) && !errors.Is(err, net.ErrClosed) && api.Code(err) != api.ErrInterfaceUnavailable {
					t.Error(err)
					return
				}
			}
		})
	}
	f.mu.Lock()
	link := f.links[7]
	link.Name = "unrelated"
	f.links[7] = link
	f.mu.Unlock()
	conn.Close()
	wg.Wait()
}
