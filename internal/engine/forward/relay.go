package forward

import (
	"context"
	"errors"
	"io"
	"net"
	"sync"
	"time"

	"github.com/Kel077/pith/internal/api"
	"github.com/Kel077/pith/internal/engine/domains"
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
	if !f.begin() {
		req.Complete(true)
		return
	}
	defer f.end()
	policy := f.opts.Router.Policy()
	f.rememberTCP(local, remote)
	a, _ := f.opts.Lookup(f.ctx, api.TCP, local, remote)
	c := api.Connection{Started: time.Now(), App: a.App, Process: a.Process, Protocol: api.TCP, Local: local.String(), Remote: remote.String(), Domain: f.cache.Lookup(remote.Addr(), time.Now())}
	// Only an address/app block can refuse the SYN before accepting it. A cached
	// name may describe another host sharing this address; sniffing gets the
	// opportunity to replace it before any domain rule is applied.
	d := policy.Match(a, api.TCP, remote, "")
	if d.Action == api.Block {
		c.RuleID = d.RuleID
		c.Outcome = api.Blocked
		c.Ended = time.Now()
		f.opts.Store.Add(c)
		req.Complete(true)
		return
	}
	var wq waiter.Queue
	ep, err := req.CreateEndpoint(&wq)
	req.Complete(err != nil)
	if err != nil {
		return
	}
	app := gonet.NewTCPConn(&wq, ep)
	defer app.Close()
	stop := context.AfterFunc(f.ctx, func() { ep.Abort() })
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
	result, dialErr := f.opts.Router.DialFlow(f.ctx, policy, a, api.TCP, remote, c.Domain)
	c.RuleID = result.RuleID
	c.Outcome, c.Interface, c.Problem = result.Outcome, result.Interface, result.Problem
	if dialErr != nil {
		c.Problem = joinProblem(c.Problem, dialErr.Error())
		c.Ended = time.Now()
		f.opts.Store.Add(c)
		ep.Abort()
		return
	}
	out := result.Conn
	c.OutboundLocal = out.LocalAddr().String()
	defer out.Close()
	stopOut := context.AfterFunc(f.ctx, func() { out.Close() })
	defer stopOut()
	id := f.opts.Store.Add(c)
	var dns *domains.DNS
	if remote.Port() == 53 {
		dns = domains.NewDNS(f.cache, true)
	}
	if len(prefix) > 0 {
		n, err := out.Write(prefix)
		f.opts.Store.AddBytes(id, uint64(n), 0)
		if dns != nil {
			dns.Query(prefix[:n], time.Now())
		}
		if err != nil || n != len(prefix) {
			f.opts.Store.End(id, joinProblem(c.Problem, problem(err)))
			ep.Abort()
			return
		}
	}
	errs := make(chan error, 2)
	copyHalf := func(dst, src net.Conn, sending bool) {
		err := f.copyTCP(dst, src, id, sending, dns)
		if err == nil {
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
	f.opts.Store.End(id, joinProblem(c.Problem, problem(errors.Join(e1, e2))))
}

var relayBuffers = sync.Pool{New: func() any { return new([32 << 10]byte) }}

func (f *Forwarder) copyTCP(dst, src net.Conn, id uint64, sending bool, dns *domains.DNS) error {
	buffer := relayBuffers.Get().(*[32 << 10]byte)
	defer relayBuffers.Put(buffer)
	for {
		n, readErr := src.Read(buffer[:])
		if n > 0 {
			written, err := dst.Write(buffer[:n])
			if sending {
				f.opts.Store.AddBytes(id, uint64(written), 0)
			} else {
				f.opts.Store.AddBytes(id, 0, uint64(written))
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
	if !f.begin() {
		return false
	}
	policy := f.opts.Router.Policy()
	local, remote := tuple(req.ID())
	var wq waiter.Queue
	ep, err := req.CreateEndpoint(&wq)
	if err != nil {
		f.end()
		return true
	}
	// Register the endpoint before returning from the packet handler, so another
	// datagram of this tuple cannot create a second session.
	go func() {
		defer f.end()
		app := gonet.NewUDPConn(&wq, ep)
		defer app.Close()
		stop := context.AfterFunc(f.ctx, func() { app.Close() })
		defer stop()
		a, _ := f.opts.Lookup(f.ctx, api.UDP, local, remote)
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
		c := api.Connection{Started: time.Now(), App: a.App, Process: a.Process, Protocol: api.UDP, Local: local.String(), Remote: remote.String(), Domain: f.cache.Lookup(remote.Addr(), time.Now())}
		if policy.Sniff {
			if host := domains.QUIC(data[:n]); host != "" {
				c.Domain = host
			}
		}
		result, dialErr := f.opts.Router.DialFlow(f.ctx, policy, a, api.UDP, remote, c.Domain)
		c.RuleID = result.RuleID
		c.Outcome, c.Interface, c.Problem = result.Outcome, result.Interface, result.Problem
		if dialErr != nil {
			c.Problem = joinProblem(c.Problem, dialErr.Error())
			c.Ended = time.Now()
			f.opts.Store.Add(c)
			return
		}
		out := result.Conn
		c.OutboundLocal = out.LocalAddr().String()
		defer out.Close()
		stopOut := context.AfterFunc(f.ctx, func() { out.Close() })
		defer stopOut()
		id := f.opts.Store.Add(c)
		var dns *domains.DNS
		if remote.Port() == 53 {
			dns = domains.NewDNS(f.cache, false)
		}
		refresh := func() { deadline := time.Now().Add(idle); app.SetDeadline(deadline); out.SetDeadline(deadline) }
		refresh()
		written, err := out.Write(data[:n])
		f.opts.Store.AddBytes(id, uint64(written), 0)
		if dns != nil {
			dns.Query(data[:written], time.Now())
		}
		if err != nil {
			f.opts.Store.End(id, joinProblem(c.Problem, problem(err)))
			return
		}
		errs := make(chan error, 2)
		copyDatagrams := func(dst, src net.Conn, sending bool) {
			buf := make([]byte, 65535)
			var result error
			for {
				n, err := src.Read(buf)
				if err != nil {
					result = err
					break
				}
				refresh()
				written, err := dst.Write(buf[:n])
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
		f.opts.Store.End(id, joinProblem(c.Problem, problem(errors.Join(e1, e2))))
	}()
	return true
}
