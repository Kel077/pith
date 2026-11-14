package proxy

import (
	"context"
	"encoding/binary"
	"errors"
	"io"
	"net"
	"net/netip"
	"strconv"
	"time"

	"github.com/Kel077/pith/internal/api"
)

// ReplyError is the reply, other than success, of an upstream proxy to a
// CONNECT: the port that goes out through it answers the same.
type ReplyError byte

func (e ReplyError) Error() string {
	return "proxy: the upstream proxy replied " + strconv.Itoa(int(e))
}

var (
	errUpstreamUnreachable = &api.Error{Code: api.ErrUpstreamUnreachable, Detail: "the upstream proxy doesn't answer"}
	errUpstreamNotSOCKS    = &api.Error{Code: api.ErrUpstreamUnreachable, Detail: "the upstream is not a SOCKS5 proxy"}
	errUpstreamRefused     = &api.Error{Code: api.ErrUpstreamRefused, Detail: "the upstream proxy refuses this Mac"}
	// errUpstreamClosed is an upstream that took this Mac and then left a
	// request unanswered: that request's failure, not the upstream's.
	errUpstreamClosed = errors.New("proxy: the upstream proxy closed the connection")
)

// through connects to target through the upstream proxy of e, and returns
// the connection with the address the upstream went out from. It notes how
// the upstream answered, for Status.
func (m *Manager) through(ctx context.Context, e *listener, target Target) (net.Conn, net.Addr, error) {
	conn, bound, err := connectUpstream(ctx, m.opts.Upstream, e.cfg.Upstream, &target)
	if !errors.Is(err, context.Canceled) {
		m.noteUpstream(e, err)
	}
	return conn, bound, err
}

// noteUpstream keeps what a connection or a check told of the upstream of
// e: a target it couldn't reach says nothing against the upstream itself.
func (m *Manager) noteUpstream(e *listener, err error) {
	var problem *api.Error
	switch api.Code(err) {
	case api.ErrUpstreamUnreachable, api.ErrUpstreamRefused:
		errors.As(err, &problem)
	}
	m.mu.Lock()
	e.up.checked, e.up.err = time.Now(), problem
	m.mu.Unlock()
}

// checkUpstream asks the upstream proxy of e whether it answers and takes
// this Mac, without asking it for any connection.
func (m *Manager) checkUpstream(e *listener) {
	defer m.wg.Done()
	conn, _, err := connectUpstream(e.ctx, m.opts.Upstream, e.cfg.Upstream, nil)
	if conn != nil {
		_ = conn.Close()
	}
	if !errors.Is(err, context.Canceled) {
		m.noteUpstream(e, err)
	}
	m.mu.Lock()
	e.up.checking = false
	m.mu.Unlock()
}

// connectUpstream opens a connection to the SOCKS5 proxy at address, agrees
// on no authentication and, unless target is nil, asks it to connect there:
// RFC 1928, sections 3 to 6. A name stays a name, for the upstream to resolve.
// It returns the connection, ready to relay, and the address the upstream
// bound for it, or the connection's own when the upstream tells none. An
// upstream that hasn't agreed within upstreamAnswer is unreachable; once it
// has, a deadline of ctx is the target's.
func connectUpstream(ctx context.Context, dial func(context.Context, string, string) (net.Conn, error), address string, target *Target) (net.Conn, net.Addr, error) {
	answer, answered := context.WithTimeout(ctx, upstreamAnswer)
	defer answered()
	conn, err := dial(answer, "tcp", address)
	if err != nil || conn == nil {
		if conn != nil {
			_ = conn.Close()
		}
		if errors.Is(ctx.Err(), context.Canceled) {
			return nil, nil, ctx.Err()
		}
		return nil, nil, errUpstreamUnreachable
	}
	// Reads and writes end with their context, as the dial does.
	greeted := false
	stop := context.AfterFunc(answer, func() { _ = conn.SetDeadline(time.Unix(1, 0)) })
	fail := func(err error) (net.Conn, net.Addr, error) {
		stop()
		closeConn(conn, true)
		switch {
		case errors.Is(ctx.Err(), context.Canceled):
			return nil, nil, ctx.Err()
		case !greeted && answer.Err() != nil:
			return nil, nil, errUpstreamUnreachable
		case ctx.Err() != nil:
			return nil, nil, ctx.Err()
		}
		return nil, nil, err
	}
	var buf [262]byte
	if err := writeAll(conn, []byte{5, 1, 0}); err != nil {
		return fail(errUpstreamUnreachable)
	}
	if _, err := io.ReadFull(conn, buf[:2]); err != nil {
		return fail(errUpstreamUnreachable)
	}
	switch {
	case buf[0] != 5:
		return fail(errUpstreamNotSOCKS)
	case buf[1] != 0:
		return fail(errUpstreamRefused)
	}
	if !stop() {
		return fail(errUpstreamUnreachable)
	}
	greeted = true
	if target == nil {
		return conn, conn.LocalAddr(), nil
	}
	stop = context.AfterFunc(ctx, func() { _ = conn.SetDeadline(time.Unix(1, 0)) })
	request := append(buf[:0], 5, 1, 0)
	if addr, err := netip.ParseAddr(target.Host); !target.IsDomain && err == nil {
		if addr = addr.Unmap().WithZone(""); addr.Is4() {
			request = append(append(request, 1), addr.AsSlice()...)
		} else {
			request = append(append(request, 4), addr.AsSlice()...)
		}
	} else if target.IsDomain && len(target.Host) > 0 && len(target.Host) <= 255 {
		request = append(append(request, 3, byte(len(target.Host))), target.Host...)
	} else {
		return fail(ReplyError(replyAddress))
	}
	request = binary.BigEndian.AppendUint16(request, target.Port)
	if err := writeAll(conn, request); err != nil {
		return fail(errUpstreamClosed)
	}
	if _, err := io.ReadFull(conn, buf[:4]); err != nil {
		return fail(errUpstreamClosed)
	}
	if buf[0] != 5 || buf[2] != 0 {
		return fail(errUpstreamClosed)
	}
	if buf[1] != replySuccess {
		return fail(ReplyError(buf[1]))
	}
	kind, n := buf[3], 0
	switch kind {
	case 1:
		n = 4
	case 4:
		n = 16
	case 3:
		if _, err := io.ReadFull(conn, buf[:1]); err != nil {
			return fail(errUpstreamClosed)
		}
		n = int(buf[0])
	default:
		return fail(errUpstreamClosed)
	}
	if _, err := io.ReadFull(conn, buf[:n+2]); err != nil {
		return fail(errUpstreamClosed)
	}
	bound := conn.LocalAddr()
	if ip, ok := netip.AddrFromSlice(buf[:n]); ok && kind != 3 && !ip.IsUnspecified() {
		if port := binary.BigEndian.Uint16(buf[n : n+2]); port != 0 {
			bound = net.TCPAddrFromAddrPort(netip.AddrPortFrom(ip, port))
		}
	}
	if !stop() {
		// ctx ended meanwhile, and its deadline may already be on conn.
		return fail(errUpstreamClosed)
	}
	return conn, bound, nil
}
