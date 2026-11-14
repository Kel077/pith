package client

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"strconv"
	"syscall"
	"time"

	"github.com/Kel077/pith/internal/api"
	"github.com/Kel077/pith/internal/proxy"
	"github.com/Kel077/pith/internal/rules"
)

// A local proxy that goes out by a scene's rules gives each interface of a
// rule the engine's three seconds, and a name's lookup for the rules that
// name addresses two.
const (
	ruleAttempt   = 3 * time.Second
	ruleLookup    = 2 * time.Second
	ruleAddresses = 8 // of a name, at most
)

// sceneRules are the rules of a scene a local proxy goes out by.
type sceneRules struct {
	set *rules.Set
	// addresses: an enabled rule that names no app names an address, which
	// a name must be looked up to match.
	addresses bool
}

// compileScenes compiles the rules of the scenes that local proxies go out
// by. Their connections match the rules compiled last.
func (c *Client) compileScenes(cfg api.Config) error {
	compiled := map[string]sceneRules{}
	for _, p := range cfg.Proxies {
		if _, done := compiled[p.Scene]; p.Scene == "" || done {
			continue
		}
		for _, scene := range cfg.Scenes {
			if scene.ID != p.Scene {
				continue
			}
			set, err := rules.New(scene)
			if err != nil {
				return err
			}
			entry := sceneRules{set: set}
			for _, r := range scene.Rules {
				if !r.Enabled || len(r.Apps) > 0 {
					continue
				}
				for _, raw := range r.Targets {
					if target, err := api.ParseTarget(raw); err == nil && target.Prefix.IsValid() {
						entry.addresses = true
					}
				}
			}
			compiled[p.Scene] = entry
		}
	}
	c.sceneRules.Store(&compiled)
	return nil
}

// errBlocked is a connection a block rule refuses: not allowed, as a
// proxy's client reads it.
func errBlocked(rule string) error {
	return fmt.Errorf("blocked by rule %s: %w", rule, syscall.EACCES)
}

// routeByRules connects for the local proxies that go out by a scene's
// rules, as api.Proxy.Scene tells, dialing through an interface with dial.
//
// A scene whose rules name domains alone sees the name a client asks for,
// and what the connection goes out through resolves it. When a rule names
// an address, a name is looked up once, as macOS does, the rules see the
// name with each address, as the engine sees a connection an app makes, and
// the connection goes to an address they were matched against: never to
// one looked up again, which they haven't seen.
func (c *Client) routeByRules(dial proxy.DialFunc) func(context.Context, string, proxy.Target) (net.Conn, error) {
	return func(ctx context.Context, sceneID string, target proxy.Target) (net.Conn, error) {
		var scene sceneRules
		if compiled := c.sceneRules.Load(); compiled != nil {
			scene = (*compiled)[sceneID]
		}
		if scene.set == nil {
			return nil, &api.Error{Code: api.ErrNoScene, Detail: sceneID}
		}
		if !target.IsDomain {
			addr, err := netip.ParseAddr(target.Host)
			if err != nil {
				return nil, errors.New("invalid proxy destination address")
			}
			return c.connectByRule(ctx, dial, scene.set.Match(rules.Flow{Protocol: api.TCP, Address: addr.Unmap().WithZone("")}), target)
		}
		byName := scene.set.Match(rules.Flow{Protocol: api.TCP, Domain: target.Host})
		if !scene.addresses || byName.Action == api.Block {
			return c.connectByRule(ctx, dial, byName, target)
		}
		lookup, cancel := context.WithTimeout(ctx, ruleLookup)
		addrs, err := c.lookup(lookup, "ip", target.Host)
		cancel()
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		if err != nil || len(addrs) == 0 {
			// Without an address, the rules that name addresses can't be
			// matched: the connection fails rather than go around them.
			return nil, &net.DNSError{Err: "no address to match the rules against", Name: target.Host, IsNotFound: true}
		}
		// The addresses a rule sends somewhere come first, then those no
		// rule names; a blocked one is never tried.
		type candidate struct {
			target   proxy.Target
			decision rules.Decision
		}
		var ruled, unruled []candidate
		blockedBy := ""
		for _, addr := range addrs[:min(len(addrs), ruleAddresses)] {
			addr = addr.Unmap().WithZone("")
			d := scene.set.Match(rules.Flow{Protocol: api.TCP, Domain: target.Host, Address: addr})
			to := candidate{proxy.Target{Host: addr.String(), Port: target.Port}, d}
			switch {
			case d.Action == api.Block:
				blockedBy = d.RuleID
			case d.RuleID != "":
				ruled = append(ruled, to)
			default:
				unruled = append(unruled, to)
			}
		}
		var last error
		for _, to := range append(ruled, unruled...) {
			conn, err := c.connectByRule(ctx, dial, to.decision, to.target)
			if err == nil {
				return conn, nil
			}
			if ctx.Err() != nil {
				return nil, ctx.Err()
			}
			last = err
		}
		if last == nil {
			return nil, errBlocked(blockedBy)
		}
		return nil, last
	}
}

// connectByRule connects to target as the decision of a scene's rules
// tells: not at all for a block rule; through a route rule's interfaces in
// order, then where macOS sends it if the rule falls back; where macOS
// sends it when no rule decided.
func (c *Client) connectByRule(ctx context.Context, dial proxy.DialFunc, d rules.Decision, target proxy.Target) (net.Conn, error) {
	if d.Action == api.Block {
		return nil, errBlocked(d.RuleID)
	}
	if d.RuleID != "" {
		var last error = &api.Error{Code: api.ErrInterfaceUnavailable, Detail: "the rule has no interface"}
		for _, via := range d.Via {
			if c.proxySuspended(via) {
				last = &api.Error{Code: api.ErrInterfaceUnavailable, Detail: "the interface is offline while its router renews its address"}
				continue
			}
			attempt, cancel := context.WithTimeout(ctx, ruleAttempt)
			conn, err := dial(attempt, via, target)
			cancel()
			if err == nil && conn != nil {
				return conn, nil
			}
			if conn != nil {
				_ = conn.Close()
			}
			if ctx.Err() != nil {
				return nil, ctx.Err()
			}
			if err != nil {
				last = err
			}
		}
		if !d.Fallback {
			return nil, last
		}
	}
	// Where macOS sends it: its routes, and its resolver for a name.
	system := c.systemDial
	if system == nil {
		system = systemDialer.DialContext
	}
	return system(ctx, "tcp", net.JoinHostPort(target.Host, strconv.Itoa(int(target.Port))))
}

// systemDialer connects where macOS sends a connection, but never to this
// Mac over loopback, by address or by a name that resolves to one: a port
// through an interface can't reach what listens for this Mac alone, and a
// port by rules, which other devices may use, must not either.
var systemDialer = &net.Dialer{Control: notLoopback}

// notLoopback refuses the addresses a proxy never connects to: loopback,
// unspecified and multicast ones.
func notLoopback(_, address string, _ syscall.RawConn) error {
	host, _, err := net.SplitHostPort(address)
	if err != nil {
		return err
	}
	addr, err := netip.ParseAddr(host)
	if err != nil {
		return err
	}
	if addr = addr.Unmap(); addr.IsLoopback() || addr.IsUnspecified() || addr.IsMulticast() {
		return fmt.Errorf("a proxy doesn't connect to %s: %w", addr, syscall.EACCES)
	}
	return nil
}

// proxySuspended tells whether local proxies refuse connections through
// via for now, as while its router renews its address.
func (c *Client) proxySuspended(via api.InterfaceRef) bool {
	c.proxyMu.Lock()
	manager := c.proxies
	c.proxyMu.Unlock()
	return manager != nil && manager.Suspended(via)
}
