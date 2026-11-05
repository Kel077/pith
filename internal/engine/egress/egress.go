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

type Router struct {
	routes  []capture.Route
	ifaces  atomic.Pointer[[]api.Interface]
	policy  atomic.Pointer[Policy]
	dial    DialFunc
	Timeout time.Duration
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
}

func (r *Router) System(remote netip.Addr) string { return capture.SystemInterface(r.routes, remote) }

func (r *Router) Dial(ctx context.Context, d rules.Decision, proto api.Protocol, remote netip.AddrPort) (Result, error) {
	if d.Action == api.Block {
		return Result{Outcome: api.Blocked}, ErrBlocked
	}
	if !remote.IsValid() || (proto != api.TCP && proto != api.UDP) {
		return Result{Outcome: api.Unreachable}, errors.New("invalid outbound flow")
	}
	ifaces := *r.ifaces.Load()
	var failures []error
	attempt := func(name string, outcome api.Outcome) (Result, bool) {
		if name == "" {
			return Result{}, false
		}
		if err := ctx.Err(); err != nil {
			failures = append(failures, err)
			return Result{}, false
		}
		attemptCtx, cancel := context.WithTimeout(ctx, r.Timeout)
		defer cancel()
		network := string(proto) + "4"
		if remote.Addr().Is6() {
			network = string(proto) + "6"
		}
		conn, err := r.dial(attemptCtx, name, network, remote.String())
		if err != nil {
			failures = append(failures, fmt.Errorf("%s: %w", name, err))
			return Result{}, false
		}
		return Result{Conn: conn, Outcome: outcome, Interface: name}, true
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
		if result, ok := attempt(iface.Name, outcome); ok {
			return result, nil
		}
	}
	if d.RuleID == "" || d.Fallback {
		if result, ok := attempt(r.System(remote.Addr()), api.System); ok {
			return result, nil
		}
	}
	if len(failures) == 0 {
		failures = append(failures, errors.New("no available route for this connection"))
	}
	return Result{Outcome: api.Unreachable}, errors.Join(failures...)
}
