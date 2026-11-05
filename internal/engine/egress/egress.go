// Package egress dials each new flow through a fixed, ordered routing decision.
package egress

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"slices"
	"sync/atomic"
	"time"

	"github.com/Kel077/pith/internal/api"
	"github.com/Kel077/pith/internal/engine/attribution"
	"github.com/Kel077/pith/internal/engine/capture"
	"github.com/Kel077/pith/internal/netif"
	"github.com/Kel077/pith/internal/rules"
)

var ErrBlocked = errors.New("connection blocked by rule")

type Policy struct {
	Rules *rules.Set
	Sniff bool
}

// Match never uses the socket owner's path for a delegated connection. The
// App is the effective process; Process is retained separately in the log.
func (p *Policy) Match(a attribution.Attribution, proto api.Protocol, remote netip.AddrPort, domain string) rules.Decision {
	f := rules.Flow{App: a.App, Protocol: proto, Address: remote.Addr(), Domain: domain}
	if !a.Delegated {
		f.Process = a.Process
	}
	return p.Rules.Match(f)
}

type DialFunc func(context.Context, string, string, string) (net.Conn, error)

// ResolveFunc resolves a domain using the selected interface's DNS and sockets.
// It is separate from dialing so synthetic addresses can never reach a physical
// interface before their DNS answers have been checked.
type ResolveFunc func(context.Context, api.Interface, string) ([]netip.Addr, error)

type Router struct {
	routes  []capture.Route
	ifaces  atomic.Pointer[[]api.Interface]
	policy  atomic.Pointer[Policy]
	dial    DialFunc
	Timeout time.Duration
	// Resolve is a test seam; nil uses interface-bound DNS. Set it before use.
	Resolve ResolveFunc
}

func New(routes []capture.Route, ifaces []api.Interface, dial DialFunc) *Router {
	if dial == nil {
		dial = func(ctx context.Context, name, network, target string) (net.Conn, error) {
			if name == "" {
				return nil, errors.New("refusing an unbound socket during capture")
			}
			d, err := netif.Dialer(name, 0, nil)
			if err != nil {
				return nil, err
			}
			return d.DialContext(ctx, network, target)
		}
	}
	r := &Router{routes: slices.Clone(routes), dial: dial, Timeout: 3 * time.Second}
	r.Interfaces(ifaces)
	return r
}

func (r *Router) Interfaces(ifaces []api.Interface) {
	value := slices.Clone(ifaces)
	for i := range value {
		value[i].DNS = slices.Clone(value[i].DNS)
		value[i].IPv4 = slices.Clone(value[i].IPv4)
		value[i].IPv6 = slices.Clone(value[i].IPv6)
	}
	r.ifaces.Store(&value)
}

func (r *Router) Reload(scene api.Scene, settings api.Settings) error {
	set, err := rules.New(scene)
	if err != nil {
		return err
	}
	r.policy.Store(&Policy{Rules: set, Sniff: settings.SniffDomains})
	return nil
}

// Policy is retained by the flow from its first packet. Reload never changes an
// existing flow, including one still reading a ClientHello or trying fallback.
func (r *Router) Policy() *Policy { return r.policy.Load() }

type Result struct {
	Conn      net.Conn
	Outcome   api.Outcome
	Interface string
	Problem   string // includes a successful system bypass of an unknown fake IP
}

func (r *Router) System(remote netip.Addr) string { return capture.SystemInterface(r.routes, remote) }

func (r *Router) Dial(ctx context.Context, d rules.Decision, proto api.Protocol, remote netip.AddrPort) (Result, error) {
	return r.DialDomain(ctx, d, proto, remote, "")
}

// DialDomain translates known fake-IP destinations only when a route rule
// explicitly selects a physical interface. System and VPN egress retain the
// original address for the proxy that owns its mapping. A block always wins.
func (r *Router) DialDomain(ctx context.Context, d rules.Decision, proto api.Protocol, remote netip.AddrPort, domain string) (Result, error) {
	if d.Action == api.Block {
		return Result{Outcome: api.Blocked}, ErrBlocked
	}
	if !remote.IsValid() || (proto != api.TCP && proto != api.UDP) {
		return Result{Outcome: api.Unreachable}, errors.New("invalid outbound flow")
	}
	fake := fakeIP(remote.Addr())
	domain = domainName(domain)
	note := ""
	if fake && domain == "" {
		// Without a mapping, sending the synthetic destination through a chosen
		// physical interface cannot honor the route. Preserve its owning proxy
		// via the saved system route even when the rule has no normal fallback.
		d.Via, d.Fallback = nil, true
		note = "fake-IP destination has no observed domain; using its pre-capture system route"
	}
	ifaces := *r.ifaces.Load()
	var failures []error
	attempt := func(name string, outcome api.Outcome, iface *api.Interface) (Result, bool) {
		if name == "" {
			return Result{}, false
		}
		if err := ctx.Err(); err != nil {
			failures = append(failures, err)
			return Result{}, false
		}
		attemptCtx, cancel := context.WithTimeout(ctx, r.Timeout)
		defer cancel()
		targets := []netip.AddrPort{remote}
		if fake && iface != nil && !vpnInterface(*iface) {
			resolve := r.Resolve
			if resolve == nil {
				resolve = resolveDomain
			}
			addresses, err := resolve(attemptCtx, *iface, domain)
			if err != nil {
				failures = append(failures, fmt.Errorf("%s DNS: %w", name, err))
				return Result{}, false
			}
			targets = realTargets(addresses, remote.Port())
			if len(targets) == 0 {
				failures = append(failures, fmt.Errorf("%s DNS returned no non-fake unicast address for %s", name, domain))
				return Result{}, false
			}
		}
		for i, target := range targets {
			if err := attemptCtx.Err(); err != nil {
				failures = append(failures, fmt.Errorf("%s: %w", name, err))
				break
			}
			network := string(proto) + "4"
			if target.Addr().Is6() {
				network = string(proto) + "6"
			}
			// Share the remaining per-interface budget among answers. A stalled
			// first address must not consume every chance to use this interface.
			deadline, _ := attemptCtx.Deadline()
			dialCtx, dialCancel := context.WithTimeout(attemptCtx, time.Until(deadline)/time.Duration(len(targets)-i))
			conn, err := r.dial(dialCtx, name, network, target.String())
			dialCancel()
			if err != nil {
				failures = append(failures, fmt.Errorf("%s: %w", name, err))
				continue
			}
			return Result{Conn: conn, Outcome: outcome, Interface: name, Problem: note}, true
		}
		return Result{}, false
	}
	for i, ref := range d.Via {
		iface := ref.Resolve(ifaces)
		if iface == nil || !iface.Up {
			continue
		}
		outcome := api.Routed
		if i > 0 {
			outcome = api.Fallback
		}
		if result, ok := attempt(iface.Name, outcome, iface); ok {
			return result, nil
		}
	}
	if d.RuleID == "" || d.Fallback {
		if result, ok := attempt(r.System(remote.Addr()), api.System, nil); ok {
			return result, nil
		}
	}
	if len(failures) == 0 {
		failures = append(failures, errors.New("no available route for this connection"))
	}
	return Result{Outcome: api.Unreachable, Problem: note}, errors.Join(failures...)
}
