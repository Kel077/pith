package proxy

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"io"
	"math"
	"net"
	"net/http"
	"net/netip"
	"strconv"
	"sync/atomic"
	"time"
)

// maxHTTPHead bounds the request line and header fields of an HTTP request.
const maxHTTPHead = 32 << 10

// isMethodLetter tells whether b can start an HTTP method, as the first
// byte of a connection that isn't SOCKS5.
func isMethodLetter(b byte) bool { return b >= 'A' && b <= 'Z' }

// serveHTTP serves a connection whose first byte isn't SOCKS5's version as
// an HTTP proxy does, RFC 9110 section 9.3.6 and RFC 9112 section 3.2:
// CONNECT host:port opens a tunnel, and a request for an http:// URL is sent
// on to its server, which closes the connection after its answer. Devices
// whose network settings take an HTTP proxy alone use it. Like SOCKS5, it
// asks for no password: who may connect is the listener's affair.
func (m *Manager) serveHTTP(e *listener, s *session, first byte, connectDeadline time.Time) {
	if !isMethodLetter(first) {
		return
	}
	head := &io.LimitedReader{R: s.in, N: maxHTTPHead}
	br := bufio.NewReader(io.MultiReader(bytes.NewReader([]byte{first}), head))
	req, err := http.ReadRequest(br)
	if err != nil {
		httpError(s.in, http.StatusBadRequest)
		return
	}
	head.N = math.MaxInt64 // a request's body has no bound
	target, tunnel, ok := httpTarget(req)
	if !ok {
		httpError(s.in, http.StatusBadRequest)
		return
	}
	if s.ctx.Err() != nil {
		return
	}
	if err := s.in.SetDeadline(connectDeadline); err != nil {
		return
	}
	ctx, cancel := context.WithDeadline(s.ctx, connectDeadline)
	defer cancel()
	out, _, err := m.open(ctx, e, s, target)
	if err != nil {
		if s.ctx.Err() == nil && err != errSessionEnded {
			httpError(s.in, httpStatus(err))
		}
		return
	}
	if tunnel {
		if err := writeAll(s.in, []byte("HTTP/1.1 200 Connection established\r\n\r\n")); err != nil {
			return
		}
	}
	if s.in.SetDeadline(time.Time{}) != nil || out.SetDeadline(time.Time{}) != nil || !m.activate(e, s) {
		return
	}
	cancel()
	var sent, received *atomic.Uint64
	if s.dev != nil {
		sent, received = &s.dev.sent, &s.dev.received
	}
	up := countedWriter{conn: out, count: &e.sent, device: sent, session: s}
	if tunnel {
		// What the client sent after its request, without waiting for the
		// answer, is the tunnel's first bytes.
		if early, _ := br.Peek(br.Buffered()); len(early) > 0 {
			if _, err := up.Write(early); err != nil {
				return
			}
		}
		relay(s, out, e)
		return
	}
	// One request a connection: the server is asked to close after its
	// answer, and whatever the client sends after this request is dropped
	// with the connection.
	req.RequestURI = ""
	for _, field := range []string{"Proxy-Connection", "Proxy-Authorization", "Keep-Alive", "Upgrade"} {
		req.Header.Del(field)
	}
	req.Header.Set("Connection", "close")
	req.Close = true
	if _, ok := req.Header["User-Agent"]; !ok {
		// net/http names itself for a request that names no client.
		req.Header["User-Agent"] = []string{""}
	}
	// The answer is relayed while the request is sent: a server may answer
	// before it has read a request's body, as with 100 Continue.
	answered := make(chan struct{})
	go func() {
		defer close(answered)
		_, _ = io.Copy(countedWriter{conn: s.in, count: &e.received, device: received, session: s}, out)
		s.close(false)
	}()
	if err := req.Write(up); err != nil {
		s.close(false)
	}
	<-answered
}

// httpTarget is where an HTTP request asks the proxy to connect: the
// authority of a CONNECT, which asks for a tunnel, or the host of an
// http:// URL, on port 80 unless it names another.
func httpTarget(req *http.Request) (target Target, tunnel, ok bool) {
	var host, port string
	if req.Method == http.MethodConnect {
		var err error
		if host, port, err = net.SplitHostPort(req.RequestURI); err != nil {
			return Target{}, false, false
		}
		tunnel = true
	} else {
		if req.URL == nil || req.URL.Scheme != "http" || req.URL.Host == "" {
			return Target{}, false, false
		}
		if host, port = req.URL.Hostname(), req.URL.Port(); port == "" {
			port = "80"
		}
	}
	n, err := strconv.ParseUint(port, 10, 16)
	if err != nil || n == 0 {
		return Target{}, false, false
	}
	if addr, err := netip.ParseAddr(host); err == nil {
		return Target{Host: addr.Unmap().WithZone("").String(), Port: uint16(n)}, tunnel, true
	}
	if !validDomain(host) {
		return Target{}, false, false
	}
	return Target{Host: host, Port: uint16(n), IsDomain: true}, tunnel, true
}

// httpStatus is the HTTP answer to a connection that couldn't be opened, as
// dialReply is SOCKS5's.
func httpStatus(err error) int {
	switch dialReply(err) {
	case replyDenied:
		return http.StatusForbidden
	case replyHost:
		return http.StatusGatewayTimeout
	}
	return http.StatusBadGateway
}

// httpError answers a request with a status alone, and that the connection
// closes.
func httpError(conn net.Conn, status int) {
	_ = writeAll(conn, fmt.Appendf(nil, "HTTP/1.1 %d %s\r\nContent-Length: 0\r\nConnection: close\r\n\r\n", status, http.StatusText(status)))
}
