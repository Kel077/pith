package client

import (
	"context"
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
	ruleAttempt = 3 * time.Second
	ruleLookup  = 2 * time.Second
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
func (c *Client) routeByRules(dial proxy.DialFunc) func(context.Context, string, proxy.Target) (net.Conn, error) {
	return func(ctx context.Context, sceneID string, target proxy.Target) (net.Conn, error) {
		var scene sceneRules
		if compiled := c.sceneRules.Load(); compiled != nil {
			scene = (*compiled)[sceneID]
		}
		if scene.set == nil {
			return nil, &api.Error{Code: api.ErrNoScene, Detail: sceneID}
		}
		flow := rules.Flow{Protocol: api.TCP}
		if target.IsDomain {
			flow.Domain = target.Host
		} else if addr, err := netip.ParseAddr(target.Host); err == nil {
			flow.Address = addr.Unmap().WithZone("")
		}
		d := scene.set.Match(flow)
		if d.RuleID == "" && target.IsDomain && scene.addresses {
			// No rule names it: a rule may name the address it has. The
			// connection then goes to that very address.
			lookup, cancel := context.WithTimeout(ctx, ruleLookup)
			addrs, _ := c.lookup(lookup, "ip", target.Host)
			cancel()
			for _, addr := range addrs {
				addr = addr.Unmap().WithZone("")
				if byAddr := scene.set.Match(rules.Flow{Protocol: api.TCP, Domain: target.Host, Address: addr}); byAddr.RuleID != "" {
					d, target = byAddr, proxy.Target{Host: addr.String(), Port: target.Port}
					break
				}
			}
		}
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
			system = (&net.Dialer{}).DialContext
		}
		return system(ctx, "tcp", net.JoinHostPort(target.Host, strconv.Itoa(int(target.Port))))
	}
}

// proxySuspended tells whether local proxies refuse connections through
// via for now, as while its router renews its address.
func (c *Client) proxySuspended(via api.InterfaceRef) bool {
	c.proxyMu.Lock()
	manager := c.proxies
	c.proxyMu.Unlock()
	return manager != nil && manager.Suspended(via)
}
