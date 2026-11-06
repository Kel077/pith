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
	Guard   *capture.Guard // Set before forwarding begins; nil only for standalone tests.
	routes  []capture.Route
	ifaces  atomic.Pointer[[]api.Interface]
	policy  atomic.Pointer[Policy]
	dial    DialFunc
	Timeout time.Duration
	// Resolve is a test seam; nil uses interface-bound DNS. Set it before use.
	Resolve ResolveFunc
	// Current refreshes kernel link identities while retaining notified DNS metadata.
	// Nil uses the supplied interface list in tests with an injected dial function.
	Current func([]api.Interface) ([]api.Interface, error)
}

func New(routes []capture.Route, ifaces []api.Interface, dial DialFunc) *Router {
	var current func([]api.Interface) ([]api.Interface, error)
	if dial == nil {
		current = netif.Current
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
	r := &Router{routes: slices.Clone(routes), dial: dial, Timeout: 3 * time.Second, Current: current}
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

// Reload publishes a policy for a standalone router. Once attached to a running
// forwarder, callers must use Forwarder.Reload to also revoke affected relays.
func (r *Router) Reload(scene api.Scene, settings api.Settings) error {
	set, err := rules.New(scene)
	if err != nil {
		return err
	}
	r.policy.Store(&Policy{Rules: set, Sniff: settings.SniffDomains})
	return nil
}

// Policy returns an immutable generation. Forwarder.Reload publishes a new
// generation and revokes affected relays; a dial retains one generation until
// the forwarder checks it again before publishing or sending application data.
func (r *Router) Policy() *Policy { return r.policy.Load() }

type Result struct {
	Conn      net.Conn
	Outcome   api.Outcome
	RuleID    string
	Interface string
	Problem   string         // includes a successful system bypass of an unknown fake IP
	Address   netip.AddrPort // actual destination, including a resolved fake IP
}

func (r *Router) System(remote netip.Addr) string {
	if r.Guard != nil {
		b, err := r.Guard.System(remote)
		if err != nil {
			return ""
		}
		return b.Name
	}
	return capture.SystemInterface(r.routes, remote)
}

func (r *Router) Dial(ctx context.Context, d rules.Decision, proto api.Protocol, remote netip.AddrPort) (Result, error) {
	return r.DialDomain(ctx, d, proto, remote, "")
}

// DialFlow retains the flow's policy and attribution while resolving synthetic
// destinations. Real addresses are checked only for blocks; they never select a
// different route rule. The caller must validate the generation before using a
// successful result if policy can change during the dial.
func (r *Router) DialFlow(ctx context.Context, policy *Policy, owner attribution.Attribution, proto api.Protocol, remote netip.AddrPort, domain string) (Result, error) {
	d := policy.Match(owner, proto, remote, domain)
	return r.dialDomain(ctx, d, proto, remote, domain, func(target netip.AddrPort) rules.Decision {
		return policy.Match(owner, proto, target, domain)
	})
}

// DialDomain translates known fake-IP destinations only when a route rule
// explicitly selects a physical interface. System and VPN egress retain the
// original address for the proxy that owns its mapping. Forwarded application
// flows must use DialFlow so real destination block rules are checked as well.
func (r *Router) DialDomain(ctx context.Context, d rules.Decision, proto api.Protocol, remote netip.AddrPort, domain string) (Result, error) {
	return r.dialDomain(ctx, d, proto, remote, domain, nil)
}

func (r *Router) dialDomain(ctx context.Context, d rules.Decision, proto api.Protocol, remote netip.AddrPort, domain string, check func(netip.AddrPort) rules.Decision) (Result, error) {
	if r.Guard != nil {
		if err := r.Guard.Admit(remote.Addr()); err != nil {
			return Result{Outcome: api.Unreachable, RuleID: d.RuleID}, err
		}
		var cancel context.CancelFunc
		ctx, cancel = context.WithCancel(ctx)
		stop := context.AfterFunc(r.Guard.Context(), cancel)
		defer cancel()
		defer stop()
	}
	if d.Action == api.Block {
		return Result{Outcome: api.Blocked, RuleID: d.RuleID}, ErrBlocked
	}
	if !remote.IsValid() || (proto != api.TCP && proto != api.UDP) {
		return Result{Outcome: api.Unreachable, RuleID: d.RuleID}, errors.New("invalid outbound flow")
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
	var failures []error
	attempt := func(name string, outcome api.Outcome, iface *api.Interface) (Result, bool, error) {
		if name == "" && !(iface == nil && r.Guard != nil) {
			return Result{}, false, nil
		}
		if err := ctx.Err(); err != nil {
			failures = append(failures, err)
			return Result{}, false, nil
		}
		attemptCtx, cancel := context.WithTimeout(ctx, r.Timeout)
		defer cancel()
		var binding capture.Binding
		checkBinding := func() error { return nil }
		if r.Guard != nil {
			var err error
			if iface == nil {
				binding, err = r.Guard.System(remote.Addr())
				name = binding.Name
			} else {
				binding, err = r.Guard.Binding(name)
			}
			if err != nil {
				failures = append(failures, err)
				return Result{}, false, nil
			}
			checkBinding = func() error {
				if err := binding.Check(); err != nil {
					return err
				}
				if iface != nil && r.Current != nil {
					current, err := r.Current(*r.ifaces.Load())
					if err != nil {
						return err
					}
					found := (api.InterfaceRef{Name: iface.Name, HardwareAddr: iface.HardwareAddr}).Resolve(current)
					if found == nil || !found.Up || found.Name != iface.Name {
						return capture.ErrEgressChanged
					}
				}
				return nil
			}
			attemptCtx = netif.WithBinding(attemptCtx, binding.Index, checkBinding)
		}
		targets := []netip.AddrPort{remote}
		var blocked rules.Decision
		if fake && iface != nil && !vpnInterface(*iface) {
			resolve := r.Resolve
			if resolve == nil {
				resolve = resolveDomain
			}
			addresses, err := resolve(attemptCtx, *iface, domain)
			if err != nil {
				if r.Guard != nil && r.Guard.Err() != nil {
					return Result{Outcome: api.Unreachable, RuleID: d.RuleID}, true, r.Guard.Err()
				}
				failures = append(failures, fmt.Errorf("%s DNS: %w", name, err))
				return Result{}, false, nil
			}
			targets = realTargets(addresses, remote.Port())
			if len(targets) == 0 {
				failures = append(failures, fmt.Errorf("%s DNS returned no non-fake unicast address for %s", name, domain))
				return Result{}, false, nil
			}
			if check != nil {
				allowed := targets[:0]
				for _, target := range targets {
					if decision := check(target); decision.Action == api.Block {
						if blocked.RuleID == "" {
							blocked = decision
						}
					} else {
						allowed = append(allowed, target)
					}
				}
				targets = allowed
			}
			// Check every returned candidate before bounding connection attempts.
			// A blocked answer beyond this budget still forbids unsafe fallback.
			if len(targets) > 32 {
				targets = targets[:32]
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
			if r.Guard != nil {
				if guardErr := checkBinding(); guardErr != nil {
					if conn != nil {
						conn.Close()
					}
					failures = append(failures, guardErr)
					return Result{}, false, nil
				}
				if err == nil {
					conn = r.Guard.Conn(conn, binding)
				}
			}
			if err != nil {
				failures = append(failures, fmt.Errorf("%s: %w", name, err))
				continue
			}
			return Result{Conn: conn, Outcome: outcome, RuleID: d.RuleID, Interface: name, Problem: note, Address: target}, true, nil
		}
		if blocked.RuleID != "" {
			// An allowed answer may connect, but once all such attempts fail do
			// not return the original fake IP to a proxy that could choose a
			// known blocked answer. This also stops later interface fallback.
			return Result{Outcome: api.Blocked, RuleID: blocked.RuleID}, true, ErrBlocked
		}
		return Result{}, false, nil
	}
	for i, ref := range d.Via {
		ifaces := *r.ifaces.Load()
		if r.Current != nil {
			var err error
			ifaces, err = r.Current(ifaces)
			if err != nil {
				failures = append(failures, err)
				continue
			}
		}
		if r.Guard != nil {
			if err := r.Guard.Err(); err != nil {
				return Result{Outcome: api.Unreachable, RuleID: d.RuleID}, err
			}
			if err := r.Guard.Interfaces(ifaces); err != nil {
				return Result{}, err
			}
		}
		iface := ref.Resolve(ifaces)
		if iface == nil || !iface.Up {
			continue
		}
		outcome := api.Routed
		if i > 0 {
			outcome = api.Fallback
		}
		if result, done, err := attempt(iface.Name, outcome, iface); done {
			return result, err
		}
	}
	if d.RuleID == "" || d.Fallback {
		// Resolve a guarded system binding only once, inside the attempt, and
		// preserve its error instead of losing it through System's string API.
		name := ""
		if r.Guard == nil {
			name = capture.SystemInterface(r.routes, remote.Addr())
		}
		if result, done, err := attempt(name, api.System, nil); done {
			return result, err
		}
	}
	if len(failures) == 0 {
		failures = append(failures, errors.New("no available route for this connection"))
	}
	return Result{Outcome: api.Unreachable, RuleID: d.RuleID, Problem: note}, errors.Join(failures...)
}
