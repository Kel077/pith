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

// The wire layout and reply codes follow RFC 1928 sections 3 through 6.
// This intentionally implements only no-auth CONNECT on local IPv4 listeners.
func (m *Manager) serve(e *listener, s *session) {
	defer m.finish(e, s)
	connectDeadline := s.start.Add(m.opts.ConnectTimeout)
	if err := s.in.SetDeadline(minTime(s.start.Add(m.opts.HandshakeTimeout), connectDeadline)); err != nil {
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
	out, err := m.opts.Dial(ctx, e.cfg.Via, target)
	if err != nil {
		if out != nil {
			closeConn(out, true)
		}
		if s.ctx.Err() == nil {
			_ = writeReply(s.in, dialReply(err), nil)
		}
		return
	}
	if out == nil {
		_ = writeReply(s.in, replyGeneral, nil)
		return
	}
	if ctx.Err() != nil || !s.setOutbound(out) {
		closeConn(out, true)
		return
	}
	if err := writeReply(s.in, replySuccess, out.LocalAddr()); err != nil {
		return
	}
	if s.in.SetDeadline(time.Time{}) != nil || out.SetDeadline(time.Time{}) != nil || !m.activate(e, s) {
		return
	}
	// The CONNECT deadline ends at establishment, not ten seconds into a stream.
	cancel()
	relay(s, out, &e.sent, &e.received)
}

func negotiate(conn net.Conn) error {
	var header [2]byte
	if _, err := io.ReadFull(conn, header[:]); err != nil {
		return err
	}
	if header[0] != 5 {
		return errors.New("proxy: invalid SOCKS version")
	}
	if header[1] == 0 {
		_ = writeAll(conn, []byte{5, 0xff})
		return errors.New("proxy: empty authentication method list")
	}
	var methods [255]byte
	if _, err := io.ReadFull(conn, methods[:int(header[1])]); err != nil {
		return err
	}
	for _, method := range methods[:int(header[1])] {
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
	switch api.Code(err) {
	case api.ErrInterfaceUnavailable:
		return replyNetwork
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

type countedWriter struct {
	conn  net.Conn
	count *atomic.Uint64
}

func (w countedWriter) Write(p []byte) (int, error) {
	n, err := w.conn.Write(p)
	if n > 0 {
		w.count.Add(uint64(n))
	}
	return n, err
}

func relay(s *session, out net.Conn, sent, received *atomic.Uint64) {
	copyTo := func(dst, src net.Conn, count *atomic.Uint64) {
		_, err := io.Copy(countedWriter{conn: dst, count: count}, src)
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
	done := make(chan struct{})
	go func() {
		defer close(done)
		copyTo(out, s.in, sent)
	}()
	copyTo(s.in, out, received)
	<-done
}

func minTime(a, b time.Time) time.Time {
	if a.Before(b) {
		return a
	}
	return b
}
