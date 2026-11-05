package capture

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"reflect"
	"slices"
	"sync"

	"github.com/Kel077/pith/internal/api"
)

// Change is a configured route or interface lifetime event, never an ARP/NDP
// cache event. Events retain transitions that two equal snapshots would miss.
type Change struct {
	Index     int
	Prefix    netip.Prefix
	Added     bool
	Interface bool
	Up        bool
	Err       error
}

// Guard is one revocable observation generation. A new Start creates a new
// generation; a revoked generation can never learn a replacement interface.
type Guard struct {
	c       *Capture
	indices map[string]int
	links   map[string]api.Interface
	ctx     context.Context
	cancel  context.CancelCauseFunc
	mu      sync.Mutex
}

func NewGuard(c *Capture, ifaces []api.Interface) (*Guard, error) {
	ctx, cancel := context.WithCancelCause(context.Background())
	g := &Guard{c: c, indices: make(map[string]int), links: make(map[string]api.Interface), ctx: ctx, cancel: cancel}
	for _, r := range c.Plan.Before {
		if r.Interface == "" || r.Index <= 0 {
			cancel(errors.New("incomplete baseline interface identity"))
			return nil, context.Cause(ctx)
		}
		if old := g.indices[r.Interface]; old != 0 && old != r.Index {
			cancel(errors.New("ambiguous baseline interface index"))
			return nil, context.Cause(ctx)
		}
		g.indices[r.Interface] = r.Index
	}
	for _, iface := range ifaces {
		index, err := c.opts.Driver.Index(iface.Name)
		if err != nil || index <= 0 {
			cancel(fmt.Errorf("read interface generation for %s: %w", iface.Name, err))
			return nil, context.Cause(ctx)
		}
		if old := g.indices[iface.Name]; old != 0 && old != index {
			cancel(errors.New("interface changed during baseline"))
			return nil, context.Cause(ctx)
		}
		g.indices[iface.Name] = index
		g.links[iface.Name] = normalizedLink(iface)
	}
	return g, nil
}

func (g *Guard) Context() context.Context { return g.ctx }
func (g *Guard) Err() error               { return context.Cause(g.ctx) }
func (g *Guard) Revoke(err error) error {
	if err == nil {
		err = errors.New("routing generation ended")
	}
	g.cancel(&api.Error{Code: api.ErrNetworkLost, Detail: err.Error()})
	return g.Err()
}

func (g *Guard) Check(name string) error {
	if err := g.Err(); err != nil {
		return err
	}
	expected := g.indices[name]
	index, err := g.c.opts.Driver.Index(name)
	if err != nil || expected == 0 || index != expected {
		return g.Revoke(fmt.Errorf("interface generation changed for %s", name))
	}
	return g.Err()
}

func (g *Guard) Index(name string) (int, error) {
	if err := g.Check(name); err != nil {
		return 0, err
	}
	return g.indices[name], nil
}

// Observe invalidates external lifetime events even if a replacement has the
// same name/index/addresses. Only our own setup announcements are harmless.
func (g *Guard) Observe(event Change) error {
	if event.Err != nil {
		return g.Revoke(event.Err)
	}
	if event.Index == g.c.state.Index {
		// Setup notifications can be delivered after activation. The utun's own
		// event fd independently reports DOWN; route validation checks ownership.
		if event.Added || event.Interface || !slices.Contains(g.c.Plan.Prefixes, event.Prefix) {
			if g.c.active.Load() {
				return g.Validate(true)
			}
			return g.Err()
		}
		return g.Revoke(errors.New("capture interface or route was withdrawn"))
	}
	if event.Added && g.localAddress(event.Prefix) {
		if g.c.active.Load() {
			return g.Validate(true)
		}
		return g.Err()
	}
	return g.Revoke(errors.New("external route or interface generation changed; restart routing"))
}

func normalizedLink(iface api.Interface) api.Interface {
	iface.Label = "" // A service label does not identify a socket binding.
	iface.IPv4 = slices.Clone(iface.IPv4)
	slices.Sort(iface.IPv4)
	iface.IPv6 = slices.Clone(iface.IPv6)
	slices.Sort(iface.IPv6)
	iface.DNS = slices.Clone(iface.DNS)
	return iface
}

// Interfaces checks the independently notified metadata before publishing it.
// Even same-index address or DNS replacement requires a new capture baseline.
func (g *Guard) Interfaces(ifaces []api.Interface) error {
	seen := make(map[string]bool)
	for _, iface := range ifaces {
		if iface.Name == g.c.Name {
			continue
		}
		before, ok := g.links[iface.Name]
		if !ok || !reflect.DeepEqual(before, normalizedLink(iface)) {
			return g.Revoke(errors.New("interface configuration changed; restart routing"))
		}
		seen[iface.Name] = true
	}
	if len(seen) != len(g.links) {
		return g.Revoke(errors.New("baseline interface disappeared"))
	}
	return g.Validate(true)
}

func (g *Guard) localAddress(p netip.Prefix) bool {
	if !p.IsValid() || p.Bits() != p.Addr().BitLen() {
		return false
	}
	a := g.c.Plan.Addresses
	return a.IPv4.Contains(p.Addr()) || a.IPv6.IsValid() && a.IPv6.Contains(p.Addr())
}

// Validate never adopts a new upstream. It verifies the original configured
// routes plus exact ownership of every capture prefix. Order is immaterial.
func (g *Guard) Validate(active bool) error {
	g.mu.Lock()
	defer g.mu.Unlock()
	if err := g.Err(); err != nil {
		return err
	}
	routes, err := g.c.opts.Driver.Snapshot()
	if err != nil {
		return g.Revoke(fmt.Errorf("observe routing: %w", err))
	}
	counts := make(map[Route]int)
	for _, r := range g.c.Plan.Before {
		counts[r]++
	}
	owned := make(map[netip.Prefix]int)
	for _, r := range routes {
		if r.Interface == g.c.Name || r.Index == g.c.state.Index {
			if r.Interface != g.c.Name || r.Index != g.c.state.Index {
				return g.Revoke(errors.New("capture interface identity changed"))
			}
			if slices.Contains(g.c.Plan.Prefixes, r.Prefix) {
				if r.Scoped || r.Local || r.Reject || r.Gateway.IsValid() {
					return g.Revoke(errors.New("capture route ownership changed"))
				}
				owned[r.Prefix]++
				continue
			}
			if g.localAddress(r.Prefix) || r.Prefix == g.c.Plan.Addresses.IPv6 || r.Prefix.Addr().IsLinkLocalUnicast() || r.Prefix.Addr().IsMulticast() {
				continue
			}
			return g.Revoke(errors.New("unexpected route on capture interface"))
		}
		if r.Local && g.localAddress(r.Prefix) {
			continue
		}
		counts[r]--
	}
	for _, n := range counts {
		if n != 0 {
			return g.Revoke(errors.New("configured external routes changed; restart routing"))
		}
	}
	for _, p := range g.c.Plan.Prefixes {
		want := 0
		if active {
			want = 1
		}
		if owned[p] != want {
			return g.Revoke(fmt.Errorf("capture route missing or duplicated: %s", p))
		}
	}
	for name := range g.indices {
		if err := g.Check(name); err != nil {
			return err
		}
	}
	index, err := g.c.opts.Driver.Index(g.c.Name)
	if err != nil || index != g.c.state.Index {
		return g.Revoke(errors.New("capture interface generation changed"))
	}
	return g.Err()
}

// Admit is independent of event delivery. A withdrawn more-specific VPN route
// must not turn its traffic into a physical rule/fallback or native raw send.
func (g *Guard) Admit(destination netip.Addr) error {
	if err := g.Err(); err != nil {
		return err
	}
	bits := -1
	for _, p := range g.c.Plan.Prefixes {
		if p.Contains(destination) && p.Bits() > bits {
			bits = p.Bits()
		}
	}
	for _, r := range g.c.Plan.Before {
		if !r.Scoped && tunnel(r.Interface) && r.Prefix.Contains(destination) && r.Prefix.Bits() > bits {
			return g.Revoke(errors.New("previously bypassed VPN destination entered capture"))
		}
	}
	return nil
}

// Packet runs before every transport, including fragments and ICMP.
func (g *Guard) Packet(packet []byte) bool {
	var addr netip.Addr
	if len(packet) >= 20 && packet[0]>>4 == 4 {
		addr = netip.AddrFrom4([4]byte(packet[16:20]))
	}
	if len(packet) >= 40 && packet[0]>>4 == 6 {
		addr = netip.AddrFrom16([16]byte(packet[24:40]))
	}
	return !addr.IsValid() || g.Admit(addr) != nil
}

type guardedConn struct {
	net.Conn
	guard *Guard
	stop  func() bool
}

func (g *Guard) Conn(conn net.Conn) net.Conn {
	c := &guardedConn{Conn: conn, guard: g}
	c.stop = context.AfterFunc(g.ctx, func() { conn.Close() })
	return c
}
func (c *guardedConn) Close() error { c.stop(); return c.Conn.Close() }
func (c *guardedConn) Read(b []byte) (int, error) {
	if err := c.guard.Err(); err != nil {
		return 0, err
	}
	return c.Conn.Read(b)
}
func (c *guardedConn) Write(b []byte) (int, error) {
	if err := c.guard.Err(); err != nil {
		return 0, err
	}
	return c.Conn.Write(b)
}
func (c *guardedConn) CloseWrite() error {
	if err := c.guard.Err(); err != nil {
		return err
	}
	if conn, ok := c.Conn.(interface{ CloseWrite() error }); ok {
		return conn.CloseWrite()
	}
	return errors.New("connection does not support write half-close")
}
