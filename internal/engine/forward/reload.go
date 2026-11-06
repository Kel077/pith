package forward

import (
	"bytes"
	"context"
	"errors"
	"net"
	"net/netip"
	"sync/atomic"
	"time"

	"github.com/Kel077/pith/internal/api"
	"github.com/Kel077/pith/internal/engine/attribution"
	"github.com/Kel077/pith/internal/engine/egress"
	"github.com/Kel077/pith/internal/rules"
	"golang.org/x/net/dns/dnsmessage"
)

var errRulesChanged = errors.New("rules changed")

// A flow is registered before attribution, endpoint creation, sniffing or dialing.
// Immutable policy pointers identify generations. Forwarder.mu protects all
// mutable fields except revoked, which also gates each application-data write.
type flow struct {
	ctx        context.Context
	cancel     context.CancelFunc
	policy     *egress.Policy
	protocol   api.Protocol
	local      netip.AddrPort
	remote     netip.AddrPort
	owner      attribution.Attribution
	fresh      bool
	verified   bool
	classified bool
	domain     string
	address    netip.AddrPort
	decision   rules.Decision
	abort      func()
	out        net.Conn
	id         uint64
	resume     *udpReloadContext
	freshUntil time.Time
	revoked    atomic.Bool
}

type udpReloadContext struct {
	owner    attribution.Attribution
	verified bool
	// An Initial may already be queued or read while its worker is still doing
	// attribution. Until classification finishes, no missing name is evidence
	// that a following opaque datagram can safely use the system route.
	unclassified bool
	domain       string
	until        time.Time
	users        int // in-progress lifetime checks pin the context past its idle deadline
}

func (f *Forwarder) begin(proto api.Protocol, local, remote netip.AddrPort) *flow {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.closing || f.active >= f.opts.MaxFlows {
		return nil
	}
	if proto == api.UDP {
		if now := time.Now(); f.active+len(f.udpReload) >= f.opts.MaxFlows && !now.Before(f.udpPruneAt) {
			// Capacity pressure must not scan the retained set for every rejected
			// datagram. Valid contexts are never evicted to make room for a tuple.
			f.udpPruneAt = now.Add(100 * time.Millisecond)
			for key, saved := range f.udpReload {
				if saved.users == 0 && !now.Before(saved.until) {
					delete(f.udpReload, key)
				}
			}
		}
		if _, retained := f.udpReload[[2]netip.AddrPort{local, remote}]; !retained && f.active+len(f.udpReload) >= f.opts.MaxFlows {
			return nil
		}
	}
	ctx, cancel := context.WithCancel(f.ctx)
	fl := &flow{ctx: ctx, cancel: cancel, policy: f.opts.Router.Policy(), protocol: proto, local: local, remote: remote}
	f.live[fl] = struct{}{}
	if proto == api.TCP {
		f.pendingTCP[[2]netip.AddrPort{local, remote}] = fl
		fl.abort = func() { f.abortHandshake(fl) }
	} else {
		f.udpCurrent[[2]netip.AddrPort{local, remote}] = fl
	}
	f.active++
	f.flows.Add(1)
	return fl
}

// bindApp handles cancellation before or during endpoint registration. It never
// calls the endpoint while holding the flow registry lock.
func (f *Forwarder) bindApp(fl *flow, abort func()) bool {
	f.mu.Lock()
	fl.abort = abort
	if fl.protocol == api.TCP {
		key := [2]netip.AddrPort{fl.local, fl.remote}
		if f.pendingTCP[key] == fl {
			delete(f.pendingTCP, key)
		}
	}
	ok := f.currentLocked(fl)
	f.mu.Unlock()
	if !ok {
		abort()
	}
	return ok
}

func (f *Forwarder) currentLocked(fl *flow) bool {
	if fl.policy != f.opts.Router.Policy() {
		fl.revoked.Store(true)
	}
	if fl.protocol == api.UDP && f.udpCurrent[[2]netip.AddrPort{fl.local, fl.remote}] != fl {
		return false
	}
	return !fl.revoked.Load() && fl.ctx.Err() == nil
}

func (f *Forwarder) current(fl *flow) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.currentLocked(fl)
}

// open registers the socket before publishing telemetry outside the registry
// lock. The second phase rechecks the generation before allowing any payload.
// Reload can cancel and close the pending socket even while Store waits on disk.
func (f *Forwarder) open(fl *flow, c api.Connection, owner attribution.Attribution, result egress.Result) bool {
	f.mu.Lock()
	if !f.currentLocked(fl) {
		f.mu.Unlock()
		return false
	}
	fl.owner, fl.domain, fl.address = owner, c.Domain, result.Address
	fl.decision = fl.policy.Match(owner, fl.protocol, fl.remote, c.Domain)
	fl.out = result.Conn
	f.mu.Unlock()
	id := f.opts.Store.AddContext(fl.ctx, c)
	f.mu.Lock()
	defer f.mu.Unlock()
	fl.id = id
	if id == 0 || !f.currentLocked(fl) {
		return false
	}
	if fl.protocol == api.UDP {
		key := [2]netip.AddrPort{fl.local, fl.remote}
		if fl.resume != nil && f.udpReload[key] == fl.resume {
			delete(f.udpReload, key)
		}
		if !fl.freshUntil.IsZero() && f.udpFreshAt.Equal(fl.freshUntil) {
			// A successfully reconstructed session no longer requires PCB reads
			// per datagram. Release admission for the next affected socket.
			f.udpFreshAt = time.Time{}
		}
	}
	return true
}

func (f *Forwarder) udpIdle(remote netip.AddrPort) time.Duration {
	if remote.Port() == 53 {
		return f.opts.DNSIdle
	}
	return f.opts.UDPIdle
}

// A rule change can leave many queued packets after the native socket exits.
// Unsuccessful reconstruction retries fresh evidence at most ten times a second
// for the whole forwarder, never once per datagram or once per unknown tuple.
func (f *Forwarder) udpLookup(fl *flow) LookupFunc {
	f.mu.Lock()
	defer f.mu.Unlock()
	if !f.currentLocked(fl) {
		return nil
	}
	key := [2]netip.AddrPort{fl.local, fl.remote}
	now := time.Now()
	if saved, ok := f.udpReload[key]; ok {
		if saved.users == 0 && !now.Before(saved.until) {
			delete(f.udpReload, key)
		} else {
			saved.until = now.Add(f.udpIdle(fl.remote))
			if f.opts.LookupFresh == nil || now.Before(f.udpFreshAt) {
				return nil
			}
			f.udpFreshAt = now.Add(100 * time.Millisecond)
			fl.freshUntil = f.udpFreshAt
			saved.users++
			fl.resume = saved
			fl.fresh = true
			return f.opts.LookupFresh
		}
	}
	// The first packet's identity is the lifetime anchor for a later reload.
	// A cached pre-reuse PCB entry must not authorize discarding its observed name.
	if f.opts.LookupFresh != nil {
		fl.fresh = true
		return f.opts.LookupFresh
	}
	return f.opts.Lookup
}

// udpDomain preserves a name only for the same independently re-observed socket
// lifetime and attribution. It is tuple-scoped, never an IP-wide DNS entry. An
// unverifiable tail is dropped until the ordinary UDP idle interval elapses;
// repeated tail packets extend that interval rather than waiting out a block.
func (f *Forwarder) udpDomain(fl *flow, owner attribution.Attribution, lookupErr error, sniffed, observed string, dnsRequest bool) (string, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if !f.currentLocked(fl) {
		return observed, false
	}
	key := [2]netip.AddrPort{fl.local, fl.remote}
	now := time.Now()
	verified := fl.fresh && lookupErr == nil && owner.SocketGeneration != 0
	if saved := fl.resume; saved != nil {
		if f.udpReload[key] != saved {
			return observed, false
		}
		saved.until = now.Add(f.udpIdle(fl.remote))
		if !verified {
			return observed, false
		}
		if saved.unclassified {
			if sniffed == "" && !dnsRequest {
				return observed, false
			}
			// A complete new request supplies its own classification. DNS's query
			// name is not the resolver's identity; retain only the observed server
			// name already supplied by the normal destination cache.
			saved.owner, saved.verified, saved.domain = owner, true, observed
			saved.unclassified = false
		} else if sniffed != "" {
			// A complete current Initial plus fresh ownership supplies its own
			// name even when a canceled predecessor had no lifetime evidence.
			// Retain the replacement evidence if its new policy blocks the Initial,
			// so a following short header cannot evade the same domain block.
			saved.owner, saved.verified, saved.domain = owner, true, sniffed
		} else if dnsRequest && saved.domain == "" && !saved.verified {
			// There is no old name or validated lifetime to inherit, and this
			// independently observed DNS request can be classified from scratch.
			saved.owner, saved.verified, saved.domain = owner, true, observed
		} else if !saved.verified {
			return observed, false
		} else if owner.SocketGeneration == saved.owner.SocketGeneration {
			if owner != saved.owner {
				return observed, false
			}
			if sniffed == "" {
				observed = saved.domain
			}
		} else {
			// A new native socket may reuse the tuple for a different hostname.
			delete(f.udpReload, key)
		}
	}
	fl.owner, fl.domain, fl.verified = owner, observed, verified
	fl.classified = true
	return observed, true
}

// completeDNSRequest recognizes a self-contained ordinary DNS query, optionally
// carrying one EDNS OPT record. Round-tripping rejects trailing opaque bytes and
// unusual compression layouts instead of treating a partial parse as evidence.
// This is only a pending-reload recovery gate, not a restriction on normal DNS.
func completeDNSRequest(data []byte) bool {
	var message dnsmessage.Message
	if message.Unpack(data) != nil || message.Response || message.OpCode != 0 || message.Truncated || message.RCode != dnsmessage.RCodeSuccess || len(message.Questions) != 1 || len(message.Answers) != 0 || len(message.Authorities) != 0 || len(message.Additionals) > 1 {
		return false
	}
	if question := message.Questions[0]; question.Class != dnsmessage.ClassINET || question.Type == 0 {
		return false
	}
	if len(message.Additionals) == 1 && (message.Additionals[0].Header.Type != dnsmessage.TypeOPT || message.Additionals[0].Header.Name.String() != ".") {
		return false
	}
	packed, err := message.Pack()
	return err == nil && bytes.Equal(packed, data)
}

func (f *Forwarder) end(fl *flow, c *api.Connection) {
	f.mu.Lock()
	delete(f.live, fl)
	if fl.protocol == api.TCP {
		key := [2]netip.AddrPort{fl.local, fl.remote}
		if f.pendingTCP[key] == fl {
			delete(f.pendingTCP, key)
		}
	} else {
		key := [2]netip.AddrPort{fl.local, fl.remote}
		if f.udpCurrent[key] == fl {
			delete(f.udpCurrent, key)
		}
	}
	if fl.resume != nil {
		fl.resume.users--
	}
	id := fl.id
	revoked := fl.revoked.Load()
	f.mu.Unlock()
	fl.cancel()
	if revoked {
		if id != 0 {
			f.opts.Store.EndWithReason(id, "", api.EndRulesChanged)
		}
	} else if id != 0 {
		f.opts.Store.EndWithReason(id, c.Problem, "")
	} else if c.Outcome == api.Blocked || c.Outcome == api.Unreachable {
		c.Ended = time.Now()
		f.opts.Store.Add(*c)
	}
	f.mu.Lock()
	f.active--
	f.mu.Unlock()
	f.flows.Done()
}

func (fl *flow) write(dst net.Conn, p []byte) (int, error) {
	if fl.revoked.Load() {
		return 0, errRulesChanged
	}
	if err := fl.ctx.Err(); err != nil {
		return 0, err
	}
	return dst.Write(p)
}

// sameRoute compares policy intent, not a rule's identity, its display label or
// the fallback socket that happened to connect. Existing sockets never migrate.
func sameRoute(a, b rules.Decision) bool {
	if a.Action != b.Action {
		return false
	}
	if a.Action != api.Route {
		return true
	}
	if len(a.Via) == 0 || len(b.Via) == 0 {
		return len(a.Via) == len(b.Via)
	}
	return a.Via[0].Name == b.Via[0].Name && a.Via[0].HardwareAddr == b.Via[0].HardwareAddr
}

// Reload visits the bounded active set once. Established flows keep their
// sockets only when the outcome and first preferred interface are unchanged.
// Pending flows have incomplete names/destinations and are conservatively
// canceled, including a dial that might still resolve a fake-IP destination.
// All endpoint and native-socket operations happen after releasing the registry.
func (f *Forwarder) Reload(scene api.Scene, settings api.Settings) error {
	type closure struct {
		fl    *flow
		abort func()
		out   net.Conn
		id    uint64
	}
	f.mu.Lock()
	if f.closing {
		f.mu.Unlock()
		return errors.New("forwarder is closed")
	}
	if err := f.opts.Router.Reload(scene, settings); err != nil {
		f.mu.Unlock()
		return err
	}
	policy := f.opts.Router.Policy()
	var closeFlows []closure
	for fl := range f.live {
		if fl.revoked.Load() {
			continue
		}
		keep := false
		if fl.id != 0 {
			decision := policy.Match(fl.owner, fl.protocol, fl.remote, fl.domain)
			keep = sameRoute(fl.decision, decision)
			if keep && fl.address.IsValid() && fl.address != fl.remote && policy.Match(fl.owner, fl.protocol, fl.address, fl.domain).Action == api.Block {
				keep = false
			}
			if keep {
				fl.policy, fl.decision = policy, decision
			}
		}
		if !keep {
			fl.revoked.Store(true)
			if fl.protocol == api.UDP {
				key := [2]netip.AddrPort{fl.local, fl.remote}
				if _, retained := f.udpReload[key]; !retained && f.udpCurrent[key] == fl {
					f.udpReload[key] = &udpReloadContext{owner: fl.owner, verified: fl.verified, unclassified: !fl.classified, domain: fl.domain, until: time.Now().Add(f.udpIdle(fl.remote))}
				}
			}
			closeFlows = append(closeFlows, closure{fl: fl, abort: fl.abort, out: fl.out, id: fl.id})
		}
	}
	f.mu.Unlock()
	for _, c := range closeFlows {
		c.fl.cancel()
		if c.abort != nil {
			c.abort()
		}
		if c.out != nil {
			c.out.Close()
		}
	}
	// Flush can hold the store lock during disk I/O. Close every affected socket
	// before entering telemetry, not just the first socket in the collected set.
	for _, c := range closeFlows {
		if c.id != 0 {
			f.opts.Store.EndWithReason(c.id, "", api.EndRulesChanged)
		}
	}
	return nil
}
