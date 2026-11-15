package proxy

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/netip"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/Kel077/pith/internal/api"
)

func upstreamProxy(port int, upstream string) api.Proxy {
	return api.Proxy{Port: port, Upstream: upstream, Enabled: true}
}

// upstream reaches one of the fixture's ports as a manager reaches its
// upstream proxy at address, from the device at from when it is set.
func (f *listenerFixture) upstream(t *testing.T, port int, address, from string) func(context.Context, string, string) (net.Conn, error) {
	return func(ctx context.Context, network, got string) (net.Conn, error) {
		if network != "tcp" || got != address {
			t.Errorf("upstream dialed %s %q, want tcp %q", network, got, address)
			return nil, errors.New("unexpected upstream")
		}
		f.mu.Lock()
		if from != "" {
			f.from[port] = append(f.from[port], &deviceConn{remote: &net.TCPAddr{IP: net.ParseIP(from), Port: 50000}})
		}
		target := f.addrs[port]
		f.mu.Unlock()
		return (&net.Dialer{}).DialContext(ctx, "tcp4", target)
	}
}

// noDial is the interface dialer of a manager whose ports all go out
// through an upstream proxy.
func noDial(t *testing.T) DialFunc {
	return func(context.Context, api.InterfaceRef, Target) (net.Conn, error) {
		t.Error("a port with an upstream proxy dialed through an interface")
		return nil, errors.New("must not dial")
	}
}

// ask asks an open connection for target and returns the reply.
func ask(t *testing.T, c net.Conn, target Target) (byte, net.Addr) {
	t.Helper()
	if _, err := c.Write(append([]byte{5, 1, 0}, requestBytes(target)...)); err != nil {
		t.Fatal(err)
	}
	readMethod(t, c, 0)
	code, bound := readReply(t, c)
	return code, net.TCPAddrFromAddrPort(bound)
}

func upstreamError(m *Manager) api.ErrorCode {
	st := m.Status(context.Background())[0]
	if st.Error == nil {
		return ""
	}
	return st.Error.Code
}

func TestUpstreamCarriesNamesAndAddresses(t *testing.T) {
	peer := echoPeer(t)
	targets, bounds := make(chan Target, 1), make(chan net.Addr, 1)
	far, farPorts := newTestManager(t, Options{Dial: func(ctx context.Context, via api.InterfaceRef, target Target) (net.Conn, error) {
		conn, err := peer.dial(ctx, via, target)
		if err == nil {
			targets <- target
			bounds <- conn.LocalAddr()
		}
		return conn, err
	}})
	applyTest(t, far, testProxy(2080))
	near, nearPorts := newTestManager(t, Options{
		Dial: noDial(t),
		Health: func(context.Context, api.InterfaceRef) error {
			t.Error("a port with an upstream proxy has no interface to check")
			return nil
		},
		Upstream: farPorts.upstream(t, 2080, "studio.example.com:2080", ""),
	})
	applyTest(t, near, upstreamProxy(1080, "studio.example.com:2080"))
	for _, target := range []Target{{Host: "example.com", Port: 443, IsDomain: true}, {Host: "192.0.2.9", Port: 80}, {Host: "2001:db8::9", Port: 8443}} {
		c := nearPorts.dial(t, 1080)
		code, bound := ask(t, c, target)
		if code != 0 {
			t.Fatalf("%v: CONNECT reply = %d", target, code)
		}
		if got := <-targets; got != target {
			t.Fatalf("the upstream was asked for %+v, want %+v", got, target)
		}
		// The reply tells where the upstream went out from, not this Mac.
		if want := <-bounds; bound.String() != want.String() {
			t.Fatalf("%v: bound %v, want the upstream's %v", target, bound, want)
		}
		roundTrip(t, c, target.Host)
	}
	if st := near.Status(context.Background())[0]; !st.Listening || st.Error != nil || st.Open != 3 || st.Sent == 0 || st.Received != st.Sent {
		t.Fatalf("status = %+v", st)
	}
	if st := far.Status(context.Background())[0]; st.Open != 3 {
		t.Fatalf("upstream status = %+v", st)
	}
}

func TestUpstreamRepliesAreRepeated(t *testing.T) {
	var failure atomic.Pointer[error]
	far, farPorts := newTestManager(t, Options{Dial: func(context.Context, api.InterfaceRef, Target) (net.Conn, error) {
		return nil, *failure.Load()
	}})
	applyTest(t, far, testProxy(2080))
	near, nearPorts := newTestManager(t, Options{Dial: noDial(t), Upstream: farPorts.upstream(t, 2080, "192.0.2.7:2080", "")})
	applyTest(t, near, upstreamProxy(1080, "192.0.2.7:2080"))
	for _, tc := range []struct {
		err  error
		code byte
	}{
		{syscall.ECONNREFUSED, replyRefused}, {syscall.EHOSTUNREACH, replyHost}, {syscall.ENETUNREACH, replyNetwork},
		{&api.Error{Code: api.ErrDNSUnavailable}, replyHost}, {&api.Error{Code: api.ErrInterfaceUnavailable}, replyNetwork}, {errors.New("unknown"), replyGeneral},
	} {
		failure.Store(&tc.err)
		if code, _ := ask(t, nearPorts.dial(t, 1080), Target{Host: "example.com", Port: 443, IsDomain: true}); code != tc.code {
			t.Fatalf("%v: reply = %d, want the upstream's %d", tc.err, code, tc.code)
		}
		// A target the upstream couldn't reach says nothing against it.
		if code := upstreamError(near); code != "" {
			t.Fatalf("%v: upstream reported %s", tc.err, code)
		}
	}
	for _, reply := range []ReplyError{0x09, 0x7f, 0xff} {
		if code := dialReply(reply); code != replyGeneral {
			t.Fatalf("unknown upstream reply %d repeated as %d", reply, code)
		}
	}
}

func TestUpstreamThatRefusesThisMac(t *testing.T) {
	far, farPorts := newTestManager(t, Options{Dial: echoPeer(t).dial})
	shareTest(t, far, nil, sharedProxy(2080))
	near, nearPorts := newTestManager(t, Options{Dial: noDial(t), Upstream: farPorts.upstream(t, 2080, "studio:2080", "192.0.2.30")})
	applyTest(t, near, upstreamProxy(1080, "studio:2080"))
	if code, _ := ask(t, nearPorts.dial(t, 1080), Target{Host: "example.com", Port: 443, IsDomain: true}); code != replyDenied {
		t.Fatalf("reply = %d, want not allowed", code)
	}
	if code := upstreamError(near); code != api.ErrUpstreamRefused {
		t.Fatalf("status = %q, want the upstream refusing", code)
	}
	if d, ok := deviceOf(far, "192.0.2.30"); !ok || d.Allowed {
		t.Fatalf("the upstream saw %+v, %v", d, ok)
	}
	// Once the other Mac lists this one, the next connection goes through.
	shareTest(t, far, []string{"192.0.2.30"}, sharedProxy(2080))
	c := nearPorts.dial(t, 1080)
	if code, _ := ask(t, c, Target{Host: "example.com", Port: 443, IsDomain: true}); code != 0 {
		t.Fatalf("reply once listed = %d", code)
	}
	roundTrip(t, c, "listed")
	if code := upstreamError(near); code != "" {
		t.Fatalf("status once listed = %q", code)
	}
	waitFor(t, func() bool {
		d, _ := deviceOf(far, "192.0.2.30")
		return d.Allowed && d.Open == 1 && d.Sent == 6 && d.Received == 6
	})
}

func TestUpstreamThatDoesNotAnswer(t *testing.T) {
	far, farPorts := newTestManager(t, Options{Dial: echoPeer(t).dial})
	applyTest(t, far, testProxy(2080))
	var down atomic.Bool
	down.Store(true)
	reach := farPorts.upstream(t, 2080, "192.0.2.7:2080", "")
	near, nearPorts := newTestManager(t, Options{Dial: noDial(t), Upstream: func(ctx context.Context, network, address string) (net.Conn, error) {
		if down.Load() {
			return nil, syscall.ECONNREFUSED
		}
		return reach(ctx, network, address)
	}})
	applyTest(t, near, upstreamProxy(1080, "192.0.2.7:2080"))
	if code, _ := ask(t, nearPorts.dial(t, 1080), Target{Host: "192.0.2.9", Port: 443}); code != replyNetwork {
		t.Fatalf("reply = %d, want network unreachable", code)
	}
	if st := near.Status(context.Background())[0]; !st.Listening || st.Error == nil || st.Error.Code != api.ErrUpstreamUnreachable {
		t.Fatalf("status = %+v", st)
	}
	down.Store(false)
	c := nearPorts.dial(t, 1080)
	if code, _ := ask(t, c, Target{Host: "192.0.2.9", Port: 443}); code != 0 {
		t.Fatalf("reply once it answers = %d", code)
	}
	roundTrip(t, c, "back")
	if code := upstreamError(near); code != "" {
		t.Fatalf("status once it answers = %q", code)
	}
}

func TestIdleUpstreamIsCheckedByStatus(t *testing.T) {
	var asked atomic.Int64
	far, farPorts := newTestManager(t, Options{Dial: func(context.Context, api.InterfaceRef, Target) (net.Conn, error) {
		asked.Add(1)
		return nil, errors.New("a check asks for no connection")
	}})
	applyTest(t, far, testProxy(2080))
	var reached atomic.Int64
	var down atomic.Bool
	reach := farPorts.upstream(t, 2080, "192.0.2.7:2080", "")
	near, _ := newTestManager(t, Options{Dial: noDial(t), Upstream: func(ctx context.Context, network, address string) (net.Conn, error) {
		reached.Add(1)
		if down.Load() {
			return nil, syscall.EHOSTUNREACH
		}
		return reach(ctx, network, address)
	}})
	applyTest(t, near, upstreamProxy(1080, "192.0.2.7:2080"))
	checked := func() bool {
		near.mu.Lock()
		defer near.mu.Unlock()
		e := near.byPort[1080]
		return !e.up.checking && !e.up.checked.IsZero()
	}
	if code := upstreamError(near); code != "" {
		t.Fatalf("status before any check = %q", code)
	}
	waitFor(t, checked)
	for range 5 {
		if code := upstreamError(near); code != "" {
			t.Fatalf("status of an upstream that answers = %q", code)
		}
	}
	if reached.Load() != 1 || asked.Load() != 0 {
		t.Fatalf("%d checks, %d connections asked of the upstream; want one check within its interval", reached.Load(), asked.Load())
	}
	// The next check, once due, finds it gone.
	down.Store(true)
	near.mu.Lock()
	near.byPort[1080].up.checked = time.Now().Add(-upstreamCheckEvery)
	near.mu.Unlock()
	_ = near.Status(context.Background())
	waitFor(t, func() bool { return checked() && upstreamError(near) == api.ErrUpstreamUnreachable })
	if reached.Load() != 2 {
		t.Fatalf("%d checks, want 2", reached.Load())
	}
}

// socksPeer is an upstream that greets, then answers a CONNECT for an IPv4
// address as answer tells; nil leaves it unanswered.
func socksPeer(t *testing.T, greet bool, answer []byte) *loopbackPeer {
	return newPeer(t, func(c *net.TCPConn) {
		var greeting [3]byte
		if _, err := io.ReadFull(c, greeting[:]); err != nil || !greet {
			_, _ = io.Copy(io.Discard, c)
			return
		}
		_, _ = c.Write([]byte{5, 0})
		var request [10]byte
		if _, err := io.ReadFull(c, request[:]); err != nil || answer == nil {
			_, _ = io.Copy(io.Discard, c)
			return
		}
		_, _ = c.Write(answer)
		_, _ = io.Copy(c, c)
		_ = c.CloseWrite()
	})
}

func (p *loopbackPeer) reach(ctx context.Context, _, _ string) (net.Conn, error) {
	return (&net.Dialer{}).DialContext(ctx, "tcp4", p.ln.Addr().String())
}

// A proxy may tell no address in its reply, as OpenSSH's dynamic forwarding
// does: the reply then carries the address this Mac reached it from.
func TestUpstreamWithoutABoundAddress(t *testing.T) {
	for _, answer := range [][]byte{
		{5, 0, 0, 1, 0, 0, 0, 0, 0, 0},
		{5, 0, 0, 3, 4, 'h', 'o', 's', 't', 0x1f, 0x90},
		{5, 0, 0, 4, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0},
	} {
		upstream := socksPeer(t, true, answer)
		m, f := newTestManager(t, Options{Dial: noDial(t), Upstream: upstream.reach})
		applyTest(t, m, upstreamProxy(1080, "192.0.2.7:2080"))
		c := f.dial(t, 1080)
		code, bound := ask(t, c, Target{Host: "192.0.2.9", Port: 443})
		if tcp := bound.(*net.TCPAddr); code != 0 || !tcp.IP.IsLoopback() || tcp.Port == 0 {
			t.Fatalf("reply = %d, bound %v", code, bound)
		}
		roundTrip(t, c, "through another proxy")
	}
}

func TestUpstreamThatIsNotSOCKS(t *testing.T) {
	upstream := newPeer(t, func(c *net.TCPConn) {
		_, _ = c.Write([]byte("HTTP/1.1 400 Bad Request\r\n\r\n"))
		_, _ = io.Copy(io.Discard, c)
	})
	m, f := newTestManager(t, Options{Dial: noDial(t), Upstream: upstream.reach})
	applyTest(t, m, upstreamProxy(1080, "192.0.2.7:8080"))
	if code, _ := ask(t, f.dial(t, 1080), Target{Host: "192.0.2.9", Port: 443}); code != replyNetwork {
		t.Fatalf("reply = %d, want network unreachable", code)
	}
	if code := upstreamError(m); code != api.ErrUpstreamUnreachable {
		t.Fatalf("status = %q", code)
	}
}

// An upstream that doesn't greet in time doesn't answer; once it greeted, a
// CONNECT that runs out of time is the target's failure, not the upstream's.
// readFailedOrClosed reads what a client sees when its CONNECT runs out of
// time: the stream closed, or a failure reply and then the close. The reply
// gets out only when the upstream's deadline fires before the client socket's,
// set for the same instant, which a busy machine may order either way.
func readFailedOrClosed(t *testing.T, c net.Conn) {
	t.Helper()
	if err := c.SetReadDeadline(time.Now().Add(5 * time.Second)); err != nil {
		t.Fatal(err)
	}
	data, err := io.ReadAll(c)
	if err != nil && !errors.Is(err, syscall.ECONNRESET) {
		t.Fatalf("read %v, want the stream closed", err)
	}
	if len(data) > 0 && (len(data) != 10 || data[0] != 5 || data[1] == replySuccess) {
		t.Fatalf("read % x, want the stream closed or a failure reply", data)
	}
}

func TestUpstreamDeadlines(t *testing.T) {
	for _, tc := range []struct {
		name  string
		greet bool
		want  api.ErrorCode
	}{{"never greets", false, api.ErrUpstreamUnreachable}, {"greets and waits for the target", true, ""}} {
		t.Run(tc.name, func(t *testing.T) {
			upstream := socksPeer(t, tc.greet, nil)
			m, f := newTestManager(t, Options{Dial: noDial(t), Upstream: upstream.reach, ConnectTimeout: 150 * time.Millisecond})
			applyTest(t, m, upstreamProxy(1080, "192.0.2.7:2080"))
			c := f.dial(t, 1080)
			if _, err := c.Write(append([]byte{5, 1, 0}, requestBytes(Target{Host: "192.0.2.9", Port: 443})...)); err != nil {
				t.Fatal(err)
			}
			readMethod(t, c, 0)
			readFailedOrClosed(t, c)
			waitFor(t, func() bool { m.mu.Lock(); defer m.mu.Unlock(); return m.total == 0 })
			m.mu.Lock()
			m.byPort[1080].up.checked = time.Now() // no check of its own meanwhile
			m.mu.Unlock()
			if code := upstreamError(m); code != tc.want {
				t.Fatalf("status = %q, want %q", code, tc.want)
			}
		})
	}
}

func TestUpstreamStreamsEndWithTheirPort(t *testing.T) {
	far, farPorts := newTestManager(t, Options{Dial: echoPeer(t).dial})
	applyTest(t, far, testProxy(2080))
	near, nearPorts := newTestManager(t, Options{Dial: echoPeer(t).dial, Upstream: farPorts.upstream(t, 2080, "192.0.2.7:2080", "")})
	applyTest(t, near, upstreamProxy(1080, "192.0.2.7:2080"), testProxy(1081))
	through, direct := nearPorts.dial(t, 1080), connectTest(t, nearPorts, 1081)
	if code, _ := ask(t, through, Target{Host: "192.0.2.9", Port: 443}); code != 0 {
		t.Fatalf("reply = %d", code)
	}
	// Suspending an interface leaves a port that has none alone.
	near.Suspend(func(api.InterfaceRef) bool { return true })
	readClosed(t, direct)
	roundTrip(t, through, "not an interface's")
	near.Resume()
	// Another upstream, or an interface instead, restarts the port.
	applyTest(t, near, upstreamProxy(1080, "192.0.2.8:2080"), testProxy(1081))
	readClosed(t, through)
	waitFor(t, func() bool { return far.Status(context.Background())[0].Open == 0 })
}

func sceneProxy(port int, scene string) api.Proxy {
	return api.Proxy{Port: port, Scene: scene, Enabled: true}
}

func TestPortByRulesAsksRoute(t *testing.T) {
	peer := echoPeer(t)
	type asked struct {
		scene  string
		target Target
	}
	calls := make(chan asked, 1)
	var failure atomic.Pointer[error]
	m, f := newTestManager(t, Options{
		Dial: noDial(t),
		Health: func(context.Context, api.InterfaceRef) error {
			t.Error("a port by rules has no interface to check")
			return nil
		},
		Route: func(ctx context.Context, scene string, target Target) (net.Conn, error) {
			calls <- asked{scene, target}
			if err := failure.Load(); err != nil {
				return nil, *err
			}
			return peer.dial(ctx, api.InterfaceRef{}, target)
		},
	})
	// The scene is one of the caller's configuration, whatever its ID; a
	// port that is off may name one too.
	off := sceneProxy(1081, "another scene")
	off.Enabled = false
	applyTest(t, m, sceneProxy(1080, "a1b2c3d4e5f6"), off)
	c := f.dial(t, 1080)
	code, bound := ask(t, c, Target{Host: "example.com", Port: 443, IsDomain: true})
	if got := <-calls; got.scene != "a1b2c3d4e5f6" || got.target != (Target{Host: "example.com", Port: 443, IsDomain: true}) {
		t.Fatalf("Route asked for %+v", got)
	}
	if tcp := bound.(*net.TCPAddr); code != 0 || !tcp.IP.IsLoopback() || tcp.Port == 0 {
		t.Fatalf("reply = %d, bound %v", code, bound)
	}
	roundTrip(t, c, "by the rules")
	// Suspending an interface leaves a port that has none alone.
	m.Suspend(func(api.InterfaceRef) bool { return true })
	roundTrip(t, c, "not an interface's")
	if st := m.Status(context.Background()); len(st) != 2 || !st[0].Listening || st[0].Error != nil || st[0].Open != 1 || st[1].Listening {
		t.Fatalf("status = %+v", st)
	}
	m.Resume()
	// A rule that blocks is a connection not allowed; one whose interfaces
	// don't connect fails as a port through them does.
	for _, tc := range []struct {
		err  error
		code byte
	}{{syscall.EACCES, replyDenied}, {&api.Error{Code: api.ErrInterfaceUnavailable}, replyNetwork}, {syscall.ECONNREFUSED, replyRefused}} {
		failure.Store(&tc.err)
		code, _ := ask(t, f.dial(t, 1080), Target{Host: "192.0.2.9", Port: 443})
		<-calls
		if code != tc.code {
			t.Fatalf("%v: reply = %d, want %d", tc.err, code, tc.code)
		}
	}
	if m.Suspended(testVia) {
		t.Fatal("still suspended after Resume")
	}
}

func TestPortByRulesWithoutRouteRefuses(t *testing.T) {
	m, f := newTestManager(t, Options{Dial: noDial(t)})
	applyTest(t, m, sceneProxy(1080, "office"))
	if code, _ := ask(t, f.dial(t, 1080), Target{Host: "192.0.2.9", Port: 443}); code != replyGeneral {
		t.Fatalf("reply = %d, want a general failure", code)
	}
}

// No port connects to a loopback, unspecified or multicast destination, or
// to a name reserved for loopback, whatever it goes out through: neither the
// dialer, nor the upstream, nor the rules are asked.
func TestNoPortConnectsToThisMacItself(t *testing.T) {
	asked := func(what string) {
		t.Errorf("%s was asked for a destination on this Mac", what)
	}
	far, farPorts := newTestManager(t, Options{Dial: func(context.Context, api.InterfaceRef, Target) (net.Conn, error) {
		asked("the upstream")
		return nil, errors.New("must not dial")
	}})
	applyTest(t, far, testProxy(2080))
	m, f := newTestManager(t, Options{
		Dial: func(context.Context, api.InterfaceRef, Target) (net.Conn, error) {
			asked("an interface")
			return nil, errors.New("must not dial")
		},
		Route: func(context.Context, string, Target) (net.Conn, error) {
			asked("a scene's rules")
			return nil, errors.New("must not route")
		},
		Upstream: farPorts.upstream(t, 2080, "192.0.2.7:2080", ""),
	})
	applyTest(t, m, testProxy(1080), upstreamProxy(1081, "192.0.2.7:2080"), sceneProxy(1082, "office"))
	for _, port := range []int{1080, 1081, 1082} {
		for _, target := range []Target{
			{Host: "127.0.0.1", Port: 8080}, {Host: "127.8.8.8", Port: 80}, {Host: "::1", Port: 8080}, {Host: "::ffff:127.0.0.1", Port: 8080},
			{Host: "0.0.0.0", Port: 80}, {Host: "::", Port: 80}, {Host: "224.0.0.251", Port: 5353}, {Host: "ff02::fb", Port: 5353},
			{Host: "localhost", Port: 8080, IsDomain: true}, {Host: "LocalHost.", Port: 8080, IsDomain: true}, {Host: "db.localhost", Port: 5432, IsDomain: true},
			// An address spelled where a name goes, as a resolver would read it.
			{Host: "127.0.0.1", Port: 8080, IsDomain: true}, {Host: "127.0.0.1.", Port: 8080, IsDomain: true}, {Host: "0.0.0.0", Port: 80, IsDomain: true},
			{Host: "127.1", Port: 8080, IsDomain: true}, {Host: "2130706433", Port: 8080, IsDomain: true}, {Host: "0x7f000001", Port: 8080, IsDomain: true},
			{Host: "0x7F.0.0.1", Port: 8080, IsDomain: true}, {Host: "0177.0.0.01", Port: 8080, IsDomain: true}, {Host: "example.123", Port: 80, IsDomain: true},
		} {
			if code, _ := ask(t, f.dial(t, port), target); code != replyDenied {
				t.Errorf("port %d, %v: reply = %d, want not allowed", port, target, code)
			}
		}
		for _, authority := range []string{"127.0.0.1:8080", "[::1]:8080", "localhost:8080"} {
			c := f.dial(t, port)
			fmt.Fprintf(c, "CONNECT %s HTTP/1.1\r\nHost: %s\r\n\r\n", authority, authority)
			if res := httpAnswer(t, c); res.StatusCode != 403 {
				t.Errorf("port %d, CONNECT %s: status %d, want 403", port, authority, res.StatusCode)
			}
		}
		c := f.dial(t, port)
		fmt.Fprint(c, "GET http://localhost:8080/ HTTP/1.1\r\nHost: localhost:8080\r\n\r\n")
		if res := httpAnswer(t, c); res.StatusCode != 403 {
			t.Errorf("port %d, GET http://localhost: status %d, want 403", port, res.StatusCode)
		}
	}
	// A name that only looks like one of them is a name like any other.
	for _, host := range []string{"localhost.example.com", "notlocalhost", "example.com", "face", "0xface.example", "123.example.com", "nas"} {
		target, ok := canonical(Target{Host: host, Port: 80, IsDomain: true})
		if !ok || !target.IsDomain || target.Host != host || ownTarget(target) {
			t.Errorf("%s was taken for an address, or for this Mac: %+v, %v", host, target, ok)
		}
	}
}

// An address a client spells where a name goes is asked for as the
// address, of an interface, an upstream and a scene's rules alike.
func TestAnAddressSpelledAsANameIsTheAddress(t *testing.T) {
	asked := make(chan Target, 1)
	peer := echoPeer(t)
	m, f := newTestManager(t, Options{Dial: func(ctx context.Context, via api.InterfaceRef, target Target) (net.Conn, error) {
		asked <- target
		return peer.dial(ctx, via, target)
	}})
	applyTest(t, m, testProxy(1080))
	for spelled, want := range map[string]Target{
		"192.0.2.9":  {Host: "192.0.2.9", Port: 443},
		"192.0.2.9.": {Host: "192.0.2.9", Port: 443},
	} {
		if code, _ := ask(t, f.dial(t, 1080), Target{Host: spelled, Port: 443, IsDomain: true}); code != 0 {
			t.Fatalf("%q: reply = %d", spelled, code)
		}
		if got := <-asked; got != want {
			t.Fatalf("%q: asked for %+v, want %+v", spelled, got, want)
		}
	}
}

// addrConn is a connection known by its two addresses alone.
type addrConn struct {
	net.Conn
	local, remote net.Addr
}

func (c addrConn) LocalAddr() net.Addr  { return c.local }
func (c addrConn) RemoteAddr() net.Addr { return c.remote }

// A shared port doesn't go out through a proxy on this Mac: other devices
// would reach, through a proxy that may not refuse them, what listens for
// this Mac alone. The upstream is told nothing; a port that isn't shared
// goes through it as before.
func TestSharedPortRefusesAnUpstreamOnThisMac(t *testing.T) {
	tcp := func(address string) net.Addr { return net.TCPAddrFromAddrPort(netip.MustParseAddrPort(address)) }
	for _, tc := range []struct {
		local, remote string
		own           bool
	}{
		{"127.0.0.1:50000", "127.0.0.1:1081", true}, {"[::1]:50000", "[::1]:1081", true},
		{"192.0.2.7:50000", "192.0.2.7:1081", true}, {"192.0.2.7:50000", "127.0.0.1:1081", true},
		{"192.0.2.7:50000", "192.0.2.8:1081", false}, {"192.0.2.7:50000", "198.51.100.7:1081", false},
	} {
		if got := toThisMac(addrConn{local: tcp(tc.local), remote: tcp(tc.remote)}); got != tc.own {
			t.Errorf("toThisMac(%s -> %s) = %v", tc.local, tc.remote, got)
		}
	}

	var greeted atomic.Int64
	upstream := newPeer(t, func(c *net.TCPConn) {
		var b [1]byte
		if n, _ := c.Read(b[:]); n > 0 {
			greeted.Add(1)
		}
	})
	echo, far := echoPeer(t), newFixture()
	farManager, err := New(Options{Dial: echo.dial, Listen: far.listen})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = farManager.Close() })
	applyTest(t, farManager, testProxy(2080))
	reach := far.upstream(t, 2080, "studio.example.com:2080", "")
	m, f := newTestManager(t, Options{Dial: noDial(t), Upstream: func(ctx context.Context, network, address string) (net.Conn, error) {
		if address == "studio.example.com:2080" {
			return reach(ctx, network, address) // the test's upstream, on loopback
		}
		return upstream.reach(ctx, network, address)
	}})
	shared := upstreamProxy(1080, "mac.example.com:1081")
	shared.Shared = true
	shareTest(t, m, []string{"192.0.2.10"}, shared, upstreamProxy(1081, "studio.example.com:2080"))
	// Without any connection, the status tells.
	waitFor(t, func() bool {
		st := m.Status(context.Background())[0]
		return st.Error != nil && st.Error.Code == api.ErrUpstreamRefused
	})
	for _, from := range []string{"192.0.2.10", ""} {
		c := f.dial(t, 1080)
		if from != "" {
			_ = c.Close()
			c = f.dialFrom(t, 1080, from)
		}
		if code, _ := ask(t, c, Target{Host: "example.com", Port: 443, IsDomain: true}); code != replyDenied {
			t.Fatalf("from %q: reply = %d, want not allowed", from, code)
		}
	}
	if greeted.Load() != 0 {
		t.Fatalf("the upstream on this Mac was told something %d times", greeted.Load())
	}
	// The same upstream address carries a port that isn't shared.
	c := f.dial(t, 1081)
	if code, _ := ask(t, c, Target{Host: "example.com", Port: 443, IsDomain: true}); code != 0 {
		t.Fatalf("a port that isn't shared: reply = %d", code)
	}
	roundTrip(t, c, "this Mac's own")
}
