package forward

import (
	"context"
	"errors"
	"io"
	"net"
	"sync"
	"syscall"
	"time"

	"github.com/Kel077/pith/internal/api"
	"github.com/Kel077/pith/internal/engine/domains"
	"gvisor.dev/gvisor/pkg/tcpip"
	"gvisor.dev/gvisor/pkg/tcpip/adapters/gonet"
	"gvisor.dev/gvisor/pkg/tcpip/transport/tcp"
	"gvisor.dev/gvisor/pkg/tcpip/transport/udp"
	"gvisor.dev/gvisor/pkg/waiter"
)

func problem(err error) string {
	if err == nil || errors.Is(err, io.EOF) || errors.Is(err, net.ErrClosed) || errors.Is(err, context.Canceled) {
		return ""
	}
	var ne net.Error
	if errors.As(err, &ne) && ne.Timeout() {
		return ""
	}
	return err.Error()
}

func joinProblem(note, detail string) string {
	if note == "" {
		return detail
	}
	if detail == "" {
		return note
	}
	return note + "; " + detail
}

func (f *Forwarder) tcp(req *tcp.ForwarderRequest) {
	local, remote := tuple(req.ID())
	if f.opts.InterceptTCP != nil {
		f.opts.InterceptTCP(local, remote)
	}
	fl := f.begin(api.TCP, local, remote)
	if fl == nil {
		req.Complete(true)
		return
	}
	c := api.Connection{Started: time.Now(), Protocol: api.TCP, Local: local.String(), Remote: remote.String(), Domain: f.cache.Lookup(remote.Addr(), time.Now())}
	defer f.end(fl, &c)
	policy := fl.policy
	f.rememberTCP(local, remote)
	a, _ := f.opts.Lookup(fl.ctx, api.TCP, local, remote)
	c.App, c.Process = a.App, a.Process
	if !f.current(fl) {
		req.Complete(true)
		return
	}
	// Only an address/app block can refuse the SYN before accepting it. A cached
	// name may describe another host sharing this address; sniffing gets the
	// opportunity to replace it before any domain rule is applied.
	d := policy.Match(a, api.TCP, remote, "")
	if d.Action == api.Block {
		c.RuleID = d.RuleID
		c.Outcome = api.Blocked
		req.Complete(true)
		return
	}
	var wq waiter.Queue
	stopHandshake := context.AfterFunc(fl.ctx, func() { f.abortHandshake(fl) })
	defer stopHandshake()
	if f.opts.beforeTCPHandshake != nil {
		f.opts.beforeTCPHandshake()
	}
	ep, err := req.CreateEndpoint(&wq)
	stopHandshake()
	req.Complete(err != nil)
	if err != nil {
		return
	}
	app := gonet.NewTCPConn(&wq, ep)
	defer app.Close()
	if !f.bindApp(fl, ep.Abort) {
		return
	}
	stop := context.AfterFunc(fl.ctx, ep.Abort)
	defer stop()
	var prefix []byte
	if policy.Sniff {
		host, p, err := domains.Sniff(app, 200*time.Millisecond)
		prefix = p
		if host != "" {
			c.Domain = host
		}
		if err != nil {
			ep.Abort()
			return
		}
	}
	if !f.current(fl) {
		ep.Abort()
		return
	}
	result, dialErr := f.opts.Router.DialFlow(fl.ctx, policy, a, api.TCP, remote, c.Domain)
	c.RuleID = result.RuleID
	c.Outcome, c.Interface, c.Problem = result.Outcome, result.Interface, result.Problem
	if dialErr != nil {
		c.Problem = joinProblem(c.Problem, dialErr.Error())
		ep.Abort()
		return
	}
	out := result.Conn
	c.OutboundLocal = out.LocalAddr().String()
	defer out.Close()
	stopOut := context.AfterFunc(fl.ctx, func() { ep.Abort(); out.Close() })
	defer stopOut()
	if !f.open(fl, c, a, result) {
		ep.Abort()
		return
	}
	id := fl.id
	var dns *domains.DNS
	if remote.Port() == 53 {
		dns = domains.NewDNS(f.cache, true)
	}
	if len(prefix) > 0 {
		n, err := fl.write(out, prefix)
		f.opts.Store.AddBytes(id, uint64(n), 0)
		if dns != nil {
			dns.Query(prefix[:n], time.Now())
		}
		if err != nil || n != len(prefix) {
			c.Problem = joinProblem(c.Problem, problem(err))
			ep.Abort()
			return
		}
	}
	errs := make(chan error, 2)
	copyHalf := func(dst, src net.Conn, sending bool) {
		err := f.copyTCP(dst, src, fl, sending, dns)
		if fl.revoked.Load() {
			// Cancellation can wake the peer relay before the Reload caller gets
			// to Abort. Never turn its induced EOF into an application-side FIN.
			ep.Abort()
			err = errRulesChanged
		} else if err == nil {
			if closer, ok := dst.(interface{ CloseWrite() error }); ok {
				err = closer.CloseWrite()
			}
		}
		if err != nil {
			app.Close()
			out.Close()
		}
		errs <- err
	}
	go copyHalf(out, app, true)
	copyHalf(app, out, false)
	e1, e2 := <-errs, <-errs
	c.Problem = joinProblem(c.Problem, joinProblem(problem(e1), problem(e2)))
}

var relayBuffers = sync.Pool{New: func() any { return new([32 << 10]byte) }}

// ICMP port-unreachable describes a datagram, not the end of a UDP socket's
// lifetime. In particular, a client can close while its final QUIC packets are
// still queued, then reject a late server reply. Absent an explicit rule change,
// keep the registered tuple and outbound socket until the ordinary idle deadline.
// An ICMP error never re-matches a tail without its initial name or exited owner.
func udpPacketError(err error, conn net.Conn) bool {
	if errors.Is(err, syscall.ECONNREFUSED) {
		return true
	}
	if _, ok := conn.(*gonet.UDPConn); !ok {
		return false
	}
	// gonet converts tcpip.Error to an untyped error string inside net.OpError.
	// Match that exact typed-adapter result, not arbitrary message substrings.
	var op *net.OpError
	return errors.As(err, &op) && op.Net == "udp" && op.Err != nil && op.Err.Error() == (&tcpip.ErrConnectionRefused{}).String()
}

func (f *Forwarder) copyTCP(dst, src net.Conn, fl *flow, sending bool, dns *domains.DNS) error {
	buffer := relayBuffers.Get().(*[32 << 10]byte)
	defer relayBuffers.Put(buffer)
	for {
		n, readErr := src.Read(buffer[:])
		if n > 0 {
			written, err := fl.write(dst, buffer[:n])
			if sending {
				f.opts.Store.AddBytes(fl.id, uint64(written), 0)
			} else {
				f.opts.Store.AddBytes(fl.id, 0, uint64(written))
			}
			if dns != nil {
				if sending {
					dns.Query(buffer[:written], time.Now())
				} else {
					dns.Response(buffer[:written], time.Now())
				}
			}
			if err != nil {
				return err
			}
			if written != n {
				return io.ErrShortWrite
			}
		}
		if errors.Is(readErr, io.EOF) {
			return nil
		}
		if readErr != nil {
			return readErr
		}
	}
}

func (f *Forwarder) udp(req *udp.ForwarderRequest) bool {
	local, remote := tuple(req.ID())
	fl := f.begin(api.UDP, local, remote)
	if fl == nil {
		return false
	}
	policy := fl.policy
	c := api.Connection{Started: time.Now(), Protocol: api.UDP, Local: local.String(), Remote: remote.String(), Domain: f.cache.Lookup(remote.Addr(), time.Now())}
	var wq waiter.Queue
	ep, err := req.CreateEndpoint(&wq)
	if err != nil {
		f.end(fl, &c)
		return true
	}
	// Register the endpoint before returning from the packet handler, so another
	// datagram of this tuple cannot create a second session.
	go func() {
		defer f.end(fl, &c)
		app := gonet.NewUDPConn(&wq, ep)
		defer app.Close()
		if !f.bindApp(fl, func() { app.Close() }) {
			return
		}
		stop := context.AfterFunc(fl.ctx, func() { app.Close() })
		defer stop()
		lookup := f.udpLookup(fl)
		if lookup == nil {
			return
		}
		a, lookupErr := lookup(fl.ctx, api.UDP, local, remote)
		c.App, c.Process = a.App, a.Process
		idle := f.opts.UDPIdle
		if remote.Port() == 53 {
			idle = f.opts.DNSIdle
		}
		app.SetReadDeadline(time.Now().Add(idle))
		data := make([]byte, 65535)
		n, err := app.Read(data)
		if err != nil {
			return
		}
		host := ""
		if policy.Sniff {
			if host = domains.QUIC(data[:n]); host != "" {
				c.Domain = host
			}
		}
		if f.opts.beforeUDPClassify != nil {
			f.opts.beforeUDPClassify(fl.ctx)
		}
		if domain, ok := f.udpDomain(fl, a, lookupErr, host, c.Domain, remote.Port() == 53 && completeDNSRequest(data[:n])); ok {
			c.Domain = domain
		} else {
			return
		}
		if !f.current(fl) {
			return
		}
		result, dialErr := f.opts.Router.DialFlow(fl.ctx, policy, a, api.UDP, remote, c.Domain)
		c.RuleID = result.RuleID
		c.Outcome, c.Interface, c.Problem = result.Outcome, result.Interface, result.Problem
		if dialErr != nil {
			c.Problem = joinProblem(c.Problem, dialErr.Error())
			return
		}
		out := result.Conn
		c.OutboundLocal = out.LocalAddr().String()
		defer out.Close()
		stopOut := context.AfterFunc(fl.ctx, func() { out.Close() })
		defer stopOut()
		if !f.open(fl, c, a, result) {
			return
		}
		id := fl.id
		var dns *domains.DNS
		if remote.Port() == 53 {
			dns = domains.NewDNS(f.cache, false)
		}
		refresh := func() { deadline := time.Now().Add(idle); app.SetDeadline(deadline); out.SetDeadline(deadline) }
		refresh()
		written, err := fl.write(out, data[:n])
		f.opts.Store.AddBytes(id, uint64(written), 0)
		if dns != nil {
			dns.Query(data[:written], time.Now())
		}
		if err != nil {
			c.Problem = joinProblem(c.Problem, problem(err))
			return
		}
		errs := make(chan error, 2)
		copyDatagrams := func(dst, src net.Conn, sending bool) {
			buf := make([]byte, 65535)
			var result error
			for {
				n, err := src.Read(buf)
				if err != nil {
					if udpPacketError(err, src) {
						// Consuming an asynchronous error does not refresh idle or
						// replay a packet. The next read still sees queued datagrams.
						continue
					}
					result = err
					break
				}
				refresh()
				written, err := fl.write(dst, buf[:n])
				if sending {
					f.opts.Store.AddBytes(id, uint64(written), 0)
				} else {
					f.opts.Store.AddBytes(id, 0, uint64(written))
				}
				if dns != nil {
					if sending {
						dns.Query(buf[:written], time.Now())
					} else {
						dns.Response(buf[:written], time.Now())
					}
				}
				if err != nil {
					if udpPacketError(err, dst) {
						// UDP may drop this datagram. Do not retry it or create a
						// new socket, route, or application attribution.
						continue
					}
					result = err
					break
				}
			}
			app.Close()
			out.Close()
			errs <- result
		}
		go copyDatagrams(out, app, true)
		copyDatagrams(app, out, false)
		e1, e2 := <-errs, <-errs
		c.Problem = joinProblem(c.Problem, joinProblem(problem(e1), problem(e2)))
	}()
	return true
}
