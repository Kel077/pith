package proxy

import (
	"context"
	"encoding/binary"
	"errors"
	"io"
	"net"
	"net/netip"
	"strings"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/Kel077/pith/internal/api"
)

const (
	replySuccess = byte(0)
	replyGeneral = byte(1)
	replyDenied  = byte(2)
	replyNetwork = byte(3)
	replyHost    = byte(4)
	replyRefused = byte(5)
	replyCommand = byte(7)
	replyAddress = byte(8)
)

// serve tells the protocol of a connection by its first byte, the version of
// SOCKS5 or the first letter of an HTTP method, and serves it.
//
// The wire layout and reply codes of SOCKS5 follow RFC 1928 sections 3
// through 6. This intentionally implements only no-auth CONNECT on IPv4
// listeners.
func (m *Manager) serve(e *listener, s *session) {
	defer m.finish(e, s)
	connectDeadline := s.start.Add(m.opts.ConnectTimeout)
	if err := s.in.SetDeadline(minTime(s.start.Add(m.opts.HandshakeTimeout), connectDeadline)); err != nil {
		return
	}
	var version [1]byte
	if _, err := io.ReadFull(s.in, version[:]); err != nil {
		return
	}
	if version[0] != 5 {
		m.serveHTTP(e, s, version[0], connectDeadline)
		return
	}
	if err := negotiate(s.in); err != nil {
		return
	}
	target, code, err := request(s.in)
	if err != nil {
		if code != 0 {
			_ = writeReply(s.in, code, nil)
		}
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
	out, bound, err := m.open(ctx, e, s, target)
	if err != nil {
		if s.ctx.Err() == nil && !errors.Is(err, errSessionEnded) {
			_ = writeReply(s.in, dialReply(err), nil)
		}
		return
	}
	if err := writeReply(s.in, replySuccess, bound); err != nil {
		return
	}
	if s.in.SetDeadline(time.Time{}) != nil || out.SetDeadline(time.Time{}) != nil || !m.activate(e, s) {
		return
	}
	// The CONNECT deadline ends at establishment, not ten seconds into a stream.
	cancel()
	relay(s, out, e)
}

var (
	errNoConnection = errors.New("proxy: the dialer returned no connection")
	// errSessionEnded is a session that ended while its connection was
	// opened: there is no one left to tell.
	errSessionEnded = errors.New("proxy: the session ended")
)

// open connects to target through what e goes out through, an interface or
// an upstream proxy, and makes the connection the outbound one of s. It
// returns it with the address it went out from. Its error is what the
// client is told, in its protocol.
func (m *Manager) open(ctx context.Context, e *listener, s *session, target Target) (net.Conn, net.Addr, error) {
	var out net.Conn
	var bound net.Addr
	var err error
	switch {
	case e.cfg.Upstream != "":
		out, bound, err = m.through(ctx, e, target)
	case m.isSuspended(e.cfg.Via):
		return nil, nil, errSuspended
	default:
		if out, err = m.opts.Dial(ctx, e.cfg.Via, target); out != nil {
			bound = out.LocalAddr()
		}
	}
	if err != nil {
		if out != nil {
			closeConn(out, true)
		}
		return nil, nil, err
	}
	if out == nil {
		return nil, nil, errNoConnection
	}
	if ctx.Err() != nil || !s.setOutbound(out) {
		closeConn(out, true)
		return nil, nil, errSessionEnded
	}
	return out, bound, nil
}

// negotiate agrees on no authentication with a client whose first byte,
// the version, is read already.
func negotiate(conn net.Conn) error {
	var count [1]byte
	if _, err := io.ReadFull(conn, count[:]); err != nil {
		return err
	}
	if count[0] == 0 {
		_ = writeAll(conn, []byte{5, 0xff})
		return errors.New("proxy: empty authentication method list")
	}
	var methods [255]byte
	if _, err := io.ReadFull(conn, methods[:int(count[0])]); err != nil {
		return err
	}
	for _, method := range methods[:int(count[0])] {
		if method == 0 {
			return writeAll(conn, []byte{5, 0})
		}
	}
	_ = writeAll(conn, []byte{5, 0xff})
	return errors.New("proxy: no acceptable authentication method")
}

func request(conn net.Conn) (Target, byte, error) {
	var header [4]byte
	if _, err := io.ReadFull(conn, header[:]); err != nil {
		return Target{}, 0, err
	}
	if header[0] != 5 || header[2] != 0 {
		return Target{}, replyGeneral, errors.New("proxy: malformed request header")
	}
	if header[1] != 1 {
		return Target{}, replyCommand, errors.New("proxy: command is not supported")
	}
	var target Target
	var raw [255]byte
	switch header[3] {
	case 1:
		if _, err := io.ReadFull(conn, raw[:4]); err != nil {
			return Target{}, 0, err
		}
		target.Host = netip.AddrFrom4([4]byte(raw[:4])).String()
	case 4:
		if _, err := io.ReadFull(conn, raw[:16]); err != nil {
			return Target{}, 0, err
		}
		target.Host = netip.AddrFrom16([16]byte(raw[:16])).String()
	case 3:
		if _, err := io.ReadFull(conn, raw[:1]); err != nil {
			return Target{}, 0, err
		}
		n := int(raw[0])
		if n == 0 {
			return Target{}, replyHost, errors.New("proxy: empty domain")
		}
		if _, err := io.ReadFull(conn, raw[:n]); err != nil {
			return Target{}, 0, err
		}
		target.Host, target.IsDomain = string(raw[:n]), true
		if !validDomain(target.Host) {
			return Target{}, replyHost, errors.New("proxy: invalid domain")
		}
	default:
		return Target{}, replyAddress, errors.New("proxy: address type is not supported")
	}
	if _, err := io.ReadFull(conn, raw[:2]); err != nil {
		return Target{}, 0, err
	}
	target.Port = binary.BigEndian.Uint16(raw[:2])
	if target.Port == 0 {
		return Target{}, replyHost, errors.New("proxy: destination port is zero")
	}
	return target, 0, nil
}

func validDomain(host string) bool {
	host = strings.TrimSuffix(host, ".")
	if len(host) == 0 || len(host) > 253 {
		return false
	}
	for label := range strings.SplitSeq(host, ".") {
		if len(label) == 0 || len(label) > 63 || label[0] == '-' || label[len(label)-1] == '-' {
			return false
		}
		for _, c := range []byte(label) {
			if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '-') {
				return false
			}
		}
	}
	return true
}

func writeReply(conn net.Conn, code byte, bound net.Addr) error {
	var reply [22]byte
	reply[0], reply[1], reply[3] = 5, code, 1
	n := 10
	if code == replySuccess {
		tcp, ok := bound.(*net.TCPAddr)
		if !ok || tcp.Port <= 0 || tcp.Port > 65535 {
			_ = writeReply(conn, replyGeneral, nil)
			return errors.New("proxy: outbound local address is not TCP")
		}
		ip, ok := netip.AddrFromSlice(tcp.IP)
		if !ok {
			_ = writeReply(conn, replyGeneral, nil)
			return errors.New("proxy: outbound local address is invalid")
		}
		ip = ip.Unmap()
		if ip.Is4() {
			a := ip.As4()
			copy(reply[4:8], a[:])
		} else {
			reply[3], n = 4, 22
			a := ip.As16()
			copy(reply[4:20], a[:])
		}
		binary.BigEndian.PutUint16(reply[n-2:n], uint16(tcp.Port))
	}
	return writeAll(conn, reply[:n])
}

func writeAll(conn net.Conn, b []byte) error {
	for len(b) != 0 {
		n, err := conn.Write(b)
		if n > 0 {
			b = b[n:]
		}
		if err != nil {
			return err
		}
		if n == 0 {
			return io.ErrShortWrite
		}
	}
	return nil
}

func dialReply(err error) byte {
	// What an upstream proxy answered is what this one answers.
	var reply ReplyError
	if errors.As(err, &reply) {
		if byte(reply) >= replyGeneral && byte(reply) <= replyAddress {
			return byte(reply)
		}
		return replyGeneral
	}
	switch api.Code(err) {
	case api.ErrInterfaceUnavailable, api.ErrUpstreamUnreachable:
		return replyNetwork
	case api.ErrUpstreamRefused:
		return replyDenied
	case api.ErrDNSUnavailable:
		return replyHost
	case api.ErrUnsupported:
		return replyDenied
	}
	switch {
	case errors.Is(err, syscall.ECONNREFUSED):
		return replyRefused
	case errors.Is(err, syscall.ENETUNREACH), errors.Is(err, syscall.ENETDOWN), errors.Is(err, syscall.ENODEV), errors.Is(err, syscall.ENXIO):
		return replyNetwork
	case errors.Is(err, syscall.EHOSTUNREACH), errors.Is(err, context.DeadlineExceeded):
		return replyHost
	case errors.Is(err, syscall.EACCES), errors.Is(err, syscall.EPERM):
		return replyDenied
	}
	var dns *net.DNSError
	if errors.As(err, &dns) {
		return replyHost
	}
	var timeout net.Error
	if errors.As(err, &timeout) && timeout.Timeout() {
		return replyHost
	}
	return replyGeneral
}

// countedWriter counts the bytes written, for the port and for the other
// device the stream is for, and tells the session when it last moved data
// and whether a write is still in progress.
type countedWriter struct {
	conn    net.Conn
	count   *atomic.Uint64
	device  *atomic.Uint64 // nil for a stream of this Mac's own
	session *session
}

func (w countedWriter) Write(p []byte) (int, error) {
	w.session.writing.Add(1)
	n, err := w.conn.Write(p)
	if n > 0 {
		w.count.Add(uint64(n))
		if w.device != nil {
			w.device.Add(uint64(n))
		}
		w.session.last.Store(clock())
	}
	w.session.writing.Add(-1)
	return n, err
}

func relay(s *session, out net.Conn, e *listener) {
	copyTo := func(dst, src net.Conn, count, device *atomic.Uint64) {
		_, err := io.Copy(countedWriter{conn: dst, count: count, device: device, session: s}, src)
		if err != nil {
			s.close(false)
			return
		}
		if half, ok := dst.(interface{ CloseWrite() error }); ok {
			if half.CloseWrite() != nil {
				s.close(false)
			}
		} else {
			// Production TCP connections preserve CloseWrite. An injected generic
			// connection without half-close cannot leave the opposite copy blocked.
			s.close(false)
		}
	}
	var sent, received *atomic.Uint64
	if s.dev != nil {
		sent, received = &s.dev.sent, &s.dev.received
	}
	done := make(chan struct{})
	go func() {
		defer close(done)
		copyTo(out, s.in, &e.sent, sent)
	}()
	copyTo(s.in, out, &e.received, received)
	<-done
}

func minTime(a, b time.Time) time.Time {
	if a.Before(b) {
		return a
	}
	return b
}
