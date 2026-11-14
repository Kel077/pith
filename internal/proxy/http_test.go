package proxy

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"syscall"
	"testing"

	"github.com/Kel077/pith/internal/api"
)

// httpAnswer reads the status line and header of an HTTP answer.
func httpAnswer(t *testing.T, c net.Conn) *http.Response {
	t.Helper()
	res, err := http.ReadResponse(bufio.NewReader(c), nil)
	if err != nil {
		t.Fatal(err)
	}
	return res
}

func TestHTTPConnectOpensATunnel(t *testing.T) {
	peer := echoPeer(t)
	targets := make(chan Target, 1)
	m, f := newTestManager(t, Options{Dial: func(ctx context.Context, via api.InterfaceRef, target Target) (net.Conn, error) {
		targets <- target
		return peer.dial(ctx, via, target)
	}})
	applyTest(t, m, testProxy(1080))
	for authority, want := range map[string]Target{
		"example.com:443":       {Host: "example.com", Port: 443, IsDomain: true},
		"192.0.2.9:8443":        {Host: "192.0.2.9", Port: 8443},
		"[2001:db8::9]:443":     {Host: "2001:db8::9", Port: 443},
		"[::ffff:192.0.2.9]:80": {Host: "192.0.2.9", Port: 80},
	} {
		c := f.dial(t, 1080)
		// The client doesn't wait for the answer before it sends.
		fmt.Fprintf(c, "CONNECT %s HTTP/1.1\r\nHost: %s\r\nProxy-Connection: keep-alive\r\n\r\nearly", authority, authority)
		if got := <-targets; got != want {
			t.Fatalf("%s: asked for %+v, want %+v", authority, got, want)
		}
		var answer [39]byte
		if _, err := io.ReadFull(c, answer[:]); err != nil || string(answer[:]) != "HTTP/1.1 200 Connection established\r\n\r\n" {
			t.Fatalf("%s: answer %q, %v", authority, answer[:], err)
		}
		var early [5]byte
		if _, err := io.ReadFull(c, early[:]); err != nil || string(early[:]) != "early" {
			t.Fatalf("%s: early bytes %q, %v", authority, early[:], err)
		}
		roundTrip(t, c, "through the tunnel")
		_ = c.Close()
	}
	waitFor(t, func() bool { m.mu.Lock(); defer m.mu.Unlock(); return m.total == 0 })
	if st := m.Status(context.Background())[0]; st.Sent != 4*(5+18) || st.Received != st.Sent {
		t.Fatalf("counters = %+v", st)
	}
}

func TestHTTPConnectFailures(t *testing.T) {
	var failure atomic.Pointer[error]
	m, f := newTestManager(t, Options{Dial: func(context.Context, api.InterfaceRef, Target) (net.Conn, error) {
		return nil, *failure.Load()
	}})
	applyTest(t, m, testProxy(1080))
	for _, tc := range []struct {
		err    error
		status int
	}{
		{syscall.ECONNREFUSED, 502}, {syscall.ENETUNREACH, 502}, {&api.Error{Code: api.ErrInterfaceUnavailable}, 502},
		{&api.Error{Code: api.ErrDNSUnavailable}, 504}, {syscall.EHOSTUNREACH, 504}, {syscall.EACCES, 403}, {errors.New("unknown"), 502},
	} {
		failure.Store(&tc.err)
		c := f.dial(t, 1080)
		fmt.Fprint(c, "CONNECT example.com:443 HTTP/1.1\r\nHost: example.com:443\r\n\r\n")
		if res := httpAnswer(t, c); res.StatusCode != tc.status || !res.Close {
			t.Fatalf("%v: status %d, want %d and the connection closed", tc.err, res.StatusCode, tc.status)
		}
		readClosed(t, c)
	}
}

func TestHTTPRequestGoesToItsServerOnce(t *testing.T) {
	var seen atomic.Pointer[http.Request]
	var body atomic.Pointer[string]
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		data, _ := io.ReadAll(r.Body)
		text := string(data)
		seen.Store(r)
		body.Store(&text)
		w.Header().Set("X-Answer", "yes")
		fmt.Fprint(w, "hello from the origin")
	}))
	t.Cleanup(origin.Close)
	targets := make(chan Target, 1)
	m, f := newTestManager(t, Options{Dial: func(ctx context.Context, _ api.InterfaceRef, target Target) (net.Conn, error) {
		targets <- target
		return (&net.Dialer{}).DialContext(ctx, "tcp4", origin.Listener.Addr().String())
	}})
	applyTest(t, m, testProxy(1080))

	c := f.dial(t, 1080)
	fmt.Fprint(c, "POST http://example.com/path?q=1 HTTP/1.1\r\nHost: example.com\r\nProxy-Connection: keep-alive\r\nProxy-Authorization: Basic eDp5\r\n"+
		"Connection: X-Hop, keep-alive\r\nConnection: x-other-hop\r\nX-Hop: of this hop\r\nX-Other-Hop: too\r\nKeep-Alive: timeout=5\r\nTE: trailers\r\nX-End-To-End: kept\r\n"+
		"Content-Length: 4\r\n\r\nbody")
	if got := <-targets; got != (Target{Host: "example.com", Port: 80, IsDomain: true}) {
		t.Fatalf("asked for %+v", got)
	}
	res := httpAnswer(t, c)
	if res.StatusCode != 200 || res.Header.Get("X-Answer") != "yes" {
		t.Fatalf("answer %d %v", res.StatusCode, res.Header)
	}
	r := seen.Load()
	if r.Method != "POST" || r.RequestURI != "/path?q=1" || r.Host != "example.com" || *body.Load() != "body" {
		t.Fatalf("the origin got %s %s for %s with %q", r.Method, r.RequestURI, r.Host, *body.Load())
	}
	if r.Header.Get("Proxy-Connection") != "" || r.Header.Get("Proxy-Authorization") != "" || !r.Close {
		t.Fatalf("the origin got the proxy's fields, or was not asked to close: %v", r.Header)
	}
	// The fields of the client's hop end at the proxy; the others go on.
	for _, field := range []string{"X-Hop", "X-Other-Hop", "Keep-Alive", "Te"} {
		if value, ok := r.Header[field]; ok {
			t.Fatalf("the origin got %s %q, a field of the client's hop", field, value)
		}
	}
	if r.Header.Get("X-End-To-End") != "kept" || r.Header.Get("Connection") != "close" {
		t.Fatalf("the origin got %v", r.Header)
	}
	// The request named no client, and the proxy names none for it.
	if agent, ok := r.Header["User-Agent"]; ok {
		t.Fatalf("the origin got a User-Agent the client didn't send: %q", agent)
	}
	readClosed(t, c)

	c = f.dial(t, 1080)
	fmt.Fprint(c, "GET http://192.0.2.9:8080/ HTTP/1.1\r\nHost: 192.0.2.9:8080\r\nUser-Agent: example/1.0\r\n\r\n")
	if got := <-targets; got != (Target{Host: "192.0.2.9", Port: 8080}) {
		t.Fatalf("asked for %+v", got)
	}
	if res := httpAnswer(t, c); res.StatusCode != 200 {
		t.Fatalf("answer %d", res.StatusCode)
	}
	if agent := seen.Load().Header.Get("User-Agent"); agent != "example/1.0" {
		t.Fatalf("the origin got User-Agent %q, want the client's", agent)
	}
	waitFor(t, func() bool { m.mu.Lock(); defer m.mu.Unlock(); return m.total == 0 })
	if st := m.Status(context.Background())[0]; st.Sent == 0 || st.Received == 0 || st.Open != 0 {
		t.Fatalf("counters = %+v", st)
	}
}

func TestHTTPRefusesWhatIsNotAProxyRequest(t *testing.T) {
	var dials atomic.Int64
	m, f := newTestManager(t, Options{Dial: func(context.Context, api.InterfaceRef, Target) (net.Conn, error) {
		dials.Add(1)
		return nil, errors.New("must not dial")
	}})
	applyTest(t, m, testProxy(1080))
	for _, wire := range []string{
		"GET / HTTP/1.1\r\nHost: 192.0.2.7:1080\r\n\r\n",
		"GET https://example.com/ HTTP/1.1\r\nHost: example.com\r\n\r\n",
		"CONNECT example.com HTTP/1.1\r\nHost: example.com\r\n\r\n",
		"CONNECT example.com:0 HTTP/1.1\r\nHost: example.com:0\r\n\r\n",
		"CONNECT exa_mple.com:443 HTTP/1.1\r\nHost: exa_mple.com:443\r\n\r\n",
		"GET http://example.com:99999/ HTTP/1.1\r\nHost: example.com\r\n\r\n",
		"NOT HTTP AT ALL\r\n\r\n",
		"GET http://example.com/ HTTP/1.1\r\nX-Long: " + strings.Repeat("a", maxHTTPHead) + "\r\n\r\n",
	} {
		c := f.dial(t, 1080)
		go func() { _, _ = io.WriteString(c, wire) }()
		if res := httpAnswer(t, c); res.StatusCode != 400 || !res.Close {
			t.Fatalf("%.40q: status %d", wire, res.StatusCode)
		}
	}
	// Neither SOCKS5 nor a method: nothing is answered.
	for _, wire := range []string{"\x04\x01\x00\x50", "get http://example.com/ HTTP/1.1\r\n\r\n", "\x16\x03\x01"} {
		c := f.dial(t, 1080)
		_, _ = io.WriteString(c, wire)
		readClosed(t, c)
	}
	if dials.Load() != 0 {
		t.Fatal("a request that is not a proxy's reached the dialer")
	}
}

func TestHTTPFromDevices(t *testing.T) {
	m, f := newTestManager(t, Options{Dial: echoPeer(t).dial})
	shareTest(t, m, []string{"192.0.2.10"}, sharedProxy(1080))
	listed := f.dialFrom(t, 1080, "192.0.2.10")
	fmt.Fprint(listed, "CONNECT example.com:443 HTTP/1.1\r\nHost: example.com:443\r\n\r\n")
	if res := httpAnswer(t, listed); res.StatusCode != 200 {
		t.Fatalf("a listed device: status %d", res.StatusCode)
	}
	roundTrip(t, listed, "a phone")
	waitFor(t, func() bool {
		d, _ := deviceOf(m, "192.0.2.10")
		return d.Open == 1 && d.Sent == 7 && d.Received == 7
	})
	stranger := f.dialFrom(t, 1080, "192.0.2.11")
	fmt.Fprint(stranger, "CONNECT example.com:443 HTTP/1.1\r\nHost: example.com:443\r\n\r\n")
	if res := httpAnswer(t, stranger); res.StatusCode != 403 {
		t.Fatalf("a device that isn't listed: status %d", res.StatusCode)
	}
	readClosed(t, stranger)
	if d, ok := deviceOf(m, "192.0.2.11"); !ok || d.Allowed {
		t.Fatalf("the refused device = %+v, %v", d, ok)
	}
}

// An HTTP request goes out through an upstream proxy as a SOCKS5 one does.
func TestHTTPThroughAnUpstream(t *testing.T) {
	targets := make(chan Target, 1)
	peer := echoPeer(t)
	far, farPorts := newTestManager(t, Options{Dial: func(ctx context.Context, via api.InterfaceRef, target Target) (net.Conn, error) {
		targets <- target
		return peer.dial(ctx, via, target)
	}})
	applyTest(t, far, testProxy(2080))
	near, nearPorts := newTestManager(t, Options{Dial: noDial(t), Upstream: farPorts.upstream(t, 2080, "192.0.2.7:2080", "")})
	applyTest(t, near, upstreamProxy(1080, "192.0.2.7:2080"))
	c := nearPorts.dial(t, 1080)
	fmt.Fprint(c, "CONNECT example.com:443 HTTP/1.1\r\nHost: example.com:443\r\n\r\n")
	if res := httpAnswer(t, c); res.StatusCode != 200 {
		t.Fatalf("status %d", res.StatusCode)
	}
	if got := <-targets; got != (Target{Host: "example.com", Port: 443, IsDomain: true}) {
		t.Fatalf("the upstream was asked for %+v", got)
	}
	roundTrip(t, c, "HTTP in, SOCKS5 out")
}
