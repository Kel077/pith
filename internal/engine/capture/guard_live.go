package capture

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"net"
	"net/netip"
	"slices"
	"sync"
	"time"

	"github.com/Kel077/pith/internal/api"
)

// Change excludes neighbor/cache events. Interface and route withdrawal retain
// lifetime transitions even when the next snapshot has the same name and index.
type Change struct {
	Index     int
	Prefix    netip.Prefix
	Added     bool
	Interface bool
	Address   bool
	Up        bool
	Err       error
	// Unparsed retains a known policy header with an undecodable key or address.
	// Prefix may retain an independently decoded IP key for relevance only.
	// Diagnostic contains no address payloads.
	Unparsed   bool
	Scoped     bool
	NonIP      bool
	Diagnostic string
}

var ErrEgressChanged = errors.New("egress interface or route changed")

// Guard owns one capture lifetime, but publishes many external routing views.
// Only a capture integrity failure revokes its context and all active relays.
type Guard struct {
	c          *Capture
	ctx        context.Context
	cancel     context.CancelCauseFunc
	mu         sync.Mutex
	indices    map[string]int
	epochs     map[string]uint64
	links      map[string]api.Interface
	routes     []Route
	generation uint64
	settle     [2]time.Time
	protected  map[netip.Prefix]bool
	now        func() time.Time
}

func NewGuard(c *Capture, ifaces []api.Interface) (*Guard, error) {
	ctx, cancel := context.WithCancelCause(context.Background())
	g := &Guard{c: c, ctx: ctx, cancel: cancel, indices: map[string]int{}, epochs: map[string]uint64{}, links: map[string]api.Interface{}, routes: slices.Clone(c.Plan.Before), generation: 1, protected: map[netip.Prefix]bool{}, now: time.Now}
	for _, r := range c.Plan.Before {
		g.indices[r.Interface] = r.Index
		if !r.Scoped && tunnel(r.Interface) && r.Prefix.Bits() > 0 {
			g.protected[r.Prefix] = true
		}
	}
	if err := g.Interfaces(ifaces); err != nil {
		cancel(err)
		return nil, err
	}
	return g, nil
}

func (g *Guard) Context() context.Context { return g.ctx }
func (g *Guard) Err() error               { return context.Cause(g.ctx) }
func (g *Guard) Revoke(err error) error {
	if err == nil {
		err = errors.New("capture lifetime ended")
	}
	g.cancel(&api.Error{Code: api.ErrNetworkLost, Detail: err.Error()})
	return g.Err()
}

// Binding pins a single interface lifetime. System bindings also pin a route
// generation until connected. Established sockets never adopt a new binding.
type Binding struct {
	guard      *Guard
	Name       string
	Index      int
	epoch      uint64
	generation uint64
}

func (g *Guard) bindingLocked(name string) (Binding, error) {
	if err := g.Err(); err != nil {
		return Binding{}, err
	}
	if name == "" || name == g.c.Name {
		return Binding{}, ErrEgressChanged
	}
	index, err := g.c.opts.Driver.Index(name)
	if err != nil || index <= 0 {
		return Binding{}, ErrEgressChanged
	}
	if index != g.indices[name] {
		g.epochs[name]++
		g.indices[name] = index
	}
	if iface, ok := g.links[name]; ok && !iface.Up {
		return Binding{}, ErrEgressChanged
	}
	return Binding{guard: g, Name: name, Index: index, epoch: g.epochs[name]}, nil
}
func (g *Guard) Binding(name string) (Binding, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.bindingLocked(name)
}
func (b Binding) Check() error {
	g := b.guard
	if g == nil {
		return ErrEgressChanged
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	if err := g.Err(); err != nil {
		return err
	}
	index, err := g.c.opts.Driver.Index(b.Name)
	if err != nil || index != b.Index || g.epochs[b.Name] != b.epoch || b.generation != 0 && b.generation != g.generation {
		return ErrEgressChanged
	}
	return nil
}
func (g *Guard) Check(name string) error        { _, err := g.Binding(name); return err }
func (g *Guard) Index(name string) (int, error) { b, err := g.Binding(name); return b.Index, err }

func family(p netip.Prefix) int {
	if p.Addr().Is6() {
		return 1
	}
	return 0
}

// Observe never converts an ordinary external change into global cancellation.
func (g *Guard) Observe(event Change) error {
	if event.Err != nil {
		return g.Revoke(event.Err)
	}
	if event.Interface || event.Address {
		// Notifications are observation triggers, not route additions/deletions.
		// Serialize with Activate so a setup event cannot inspect a partial set.
		// Validate reads current link flags: a queued DOWN followed by UP must
		// not tear down a now-healthy tunnel merely because delivery was late.
		g.c.lifecycle.Lock()
		defer g.c.lifecycle.Unlock()
		if event.Interface && event.Index != g.c.state.Index && !event.Up {
			g.mu.Lock()
			for name, index := range g.indices {
				if index == event.Index {
					g.epochs[name]++
				}
			}
			g.mu.Unlock()
		}
		return g.Validate(g.c.active.Load())
	}
	if event.Unparsed {
		if _, safe := g.unparsedDisposition(event); !safe {
			return g.Revoke(errors.New(g.RouteDiagnostic(event)))
		}
	}
	if event.Index == g.c.state.Index {
		if !event.Added && !event.Interface && slices.Contains(g.c.Plan.Prefixes, event.Prefix) {
			return g.Revoke(errors.New("capture route was withdrawn"))
		}
	} else {
		g.mu.Lock()
		for name, index := range g.indices {
			if index == event.Index && (event.Unparsed || event.Interface && !event.Up || !event.Added && event.Prefix.IsValid() && tunnel(name)) {
				g.epochs[name]++
			}
			if index == event.Index && tunnel(name) && !event.Added && event.Prefix.IsValid() && event.Prefix.Bits() == 0 {
				g.settle[family(event.Prefix)] = g.now().Add(3 * time.Second)
			}
		}
		g.mu.Unlock()
	}
	// Setup can emit partial route sets; Activate performs the first full check.
	if g.c.active.Load() {
		return g.Validate(true)
	}
	return g.Err()
}

// RouteDiagnostic attaches the decision, without incorporating snapshot errors
// (which may contain addresses). The caller retains it even if startup fails.
func (g *Guard) RouteDiagnostic(event Change) string {
	if event.Diagnostic == "" {
		return ""
	}
	if event.Unparsed {
		reason, _ := g.unparsedDisposition(event)
		return event.Diagnostic + "; " + reason
	}
	return event.Diagnostic
}

func (g *Guard) unparsedDisposition(event Change) (string, bool) {
	if event.Address || event.Interface {
		return "interface notification; verify current capture snapshot", true
	}
	if event.NonIP {
		return "non-IP route; verify capture and continue", true
	}
	if event.Index <= 0 {
		return "unknown interface; capture ownership cannot be excluded", false
	}
	if event.Index == g.c.state.Index {
		return "owned interface; capture ownership cannot be established", false
	}
	if event.Scoped {
		return "external scoped route; verify capture and continue", true
	}
	if event.Prefix.IsValid() && !slices.Contains(g.c.Plan.Prefixes, event.Prefix) && (event.Prefix.Addr().IsLinkLocalUnicast() || event.Prefix.Addr().IsMulticast()) {
		return "external link-local or multicast key; verify capture and continue", true
	}
	return "unreadable unscoped IP route; capture overlap cannot be excluded", false
}

// Addresses, DNS and labels are mutable metadata, not interface lifetimes.
func (g *Guard) Interfaces(ifaces []api.Interface) error {
	g.mu.Lock()
	defer g.mu.Unlock()
	next := make(map[string]api.Interface)
	for _, iface := range ifaces {
		if iface.Name == g.c.Name {
			continue
		}
		if old, ok := g.links[iface.Name]; ok && (old.HardwareAddr != iface.HardwareAddr || old.Up && !iface.Up) {
			g.epochs[iface.Name]++
		}
		next[iface.Name] = api.Interface{Name: iface.Name, HardwareAddr: iface.HardwareAddr, Up: iface.Up}
	}
	for name := range g.links {
		if _, ok := next[name]; !ok {
			g.epochs[name]++
		}
	}
	g.links = next
	return g.Err()
}

func (g *Guard) localAddress(p netip.Prefix) bool {
	if !p.IsValid() || p.Bits() != p.Addr().BitLen() {
		return false
	}
	a := g.c.Plan.Addresses
	return a.IPv4.Contains(p.Addr()) || a.IPv6.IsValid() && a.IPv6.Contains(p.Addr())
}

func routeCounts(routes []Route) map[Route]int {
	counts := map[Route]int{}
	for _, r := range routes {
		counts[r]++
	}
	return counts
}

func (g *Guard) shadow(r Route, addresses []netip.Prefix) bool {
	if r.Scoped {
		return false
	}
	for _, p := range g.c.Plan.Prefixes {
		if r.Prefix == p {
			return true
		}
	}
	if r.Local || r.Prefix.Addr().IsMulticast() || r.Prefix.Addr().IsLinkLocalUnicast() {
		return false
	}
	for _, old := range g.c.Plan.Before {
		if old.Prefix == r.Prefix && old.Scoped == r.Scoped && tunnel(old.Interface) == tunnel(r.Interface) {
			return false
		}
	}
	if !r.Gateway.IsValid() {
		for _, addr := range addresses {
			if r.Prefix == addr.Masked() || r.Prefix.Bits() == addr.Addr().BitLen() && r.Prefix.Contains(addr.Addr()) {
				return false
			}
		}
	}
	for _, p := range g.c.Plan.Prefixes {
		if p.Contains(r.Prefix.Addr()) && r.Prefix.Bits() > p.Bits() {
			return true
		}
	}
	return false
}

// Validate verifies capture independently, then publishes the external table.
// An inconsistent external index only makes that egress temporarily unavailable.
func (g *Guard) Validate(active bool) error {
	g.mu.Lock()
	var diagnostics []string
	defer func() {
		g.mu.Unlock()
		if publish := g.c.opts.Diagnostic; publish != nil {
			for _, summary := range diagnostics {
				publish(summary)
			}
		}
	}()
	if err := g.Err(); err != nil {
		return err
	}
	routes, details, attempts, err := g.observeSnapshot()
	if err != nil {
		if stopped := g.Err(); stopped != nil {
			return stopped
		}
		diagnostics = []string{"capture ownership: routing snapshot remained unavailable through the bounded observation window"}
		return g.Revoke(fmt.Errorf("observe capture ownership: %w", err))
	}
	if attempts > 1 {
		diagnostics = []string{"capture ownership: routing snapshot recovered within the bounded observation window"}
	}
	fault := func(err error) error {
		diagnostics = g.ownershipDiagnostics(routes, details, active)
		return g.Revoke(err)
	}
	addresses, err := g.c.opts.Driver.InterfacePrefixes()
	if err != nil {
		return fault(fmt.Errorf("observe connected routes: %w", err))
	}
	owned := map[netip.Prefix]int{}
	var external []Route
	for _, r := range routes {
		if r.Interface == g.c.Name || r.Index == g.c.state.Index {
			if r.Interface != g.c.Name || r.Index != g.c.state.Index {
				return fault(errors.New("capture interface identity changed"))
			}
			if slices.Contains(g.c.Plan.Prefixes, r.Prefix) {
				if r.Scoped || r.Local || r.Reject || r.Gateway.IsValid() {
					return fault(errors.New("capture route ownership changed"))
				}
				owned[r.Prefix]++
				continue
			}
			if g.localAddress(r.Prefix) || r.Prefix == g.c.Plan.Addresses.IPv6 || r.Prefix.Addr().IsLinkLocalUnicast() || r.Prefix.Addr().IsMulticast() {
				continue
			}
			return fault(errors.New("unexpected route on capture interface"))
		}
		if r.Local && g.localAddress(r.Prefix) {
			continue
		}
		if g.shadow(r, addresses) {
			return fault(errors.New("external route shadows capture"))
		}
		external = append(external, r)
	}
	for _, p := range g.c.Plan.Prefixes {
		want := 0
		if active {
			want = 1
		}
		if owned[p] != want {
			return fault(fmt.Errorf("capture route missing or duplicated: %s", p))
		}
	}
	var index int
	up := true
	if reader, ok := g.c.opts.Driver.(interfaceStateReader); ok {
		index, up, err = reader.InterfaceState(g.c.Name)
	} else {
		index, err = g.c.opts.Driver.Index(g.c.Name)
	}
	if err != nil || index != g.c.state.Index {
		return fault(errors.New("capture interface generation changed"))
	}
	if !up {
		return fault(errors.New("capture interface is down"))
	}
	if !maps.Equal(routeCounts(g.routes), routeCounts(external)) {
		for _, old := range g.routes {
			if !old.Scoped && old.Prefix.Bits() == 0 && tunnel(old.Interface) && !slices.Contains(external, old) {
				g.settle[family(old.Prefix)] = g.now().Add(3 * time.Second)
			}
		}
		g.generation++
		g.routes = external
	}
	for name, old := range g.indices {
		index, err := g.c.opts.Driver.Index(name)
		if err != nil {
			index = 0
		}
		if index != old {
			g.epochs[name]++
			g.indices[name] = index
		}
	}
	for _, r := range external {
		if !r.Scoped && tunnel(r.Interface) && r.Prefix.Bits() > 0 {
			g.protected[r.Prefix] = true
		}
		if _, ok := g.indices[r.Interface]; !ok {
			g.indices[r.Interface] = r.Index
		}
	}
	return g.Err()
}

// Called with mu held: no new binding or system route may use the old view while
// ownership is unreadable. Established flows are not revoked during this brief
// observation window. A readable but incorrect table is never retried here.
func (g *Guard) observeSnapshot() ([]Route, []RouteDetail, int, error) {
	ctx, cancel := context.WithTimeout(g.ctx, 2*time.Second)
	defer cancel()
	var last error
	for attempts := 1; ; attempts++ {
		var routes []Route
		var details []RouteDetail
		if reader, ok := g.c.opts.Driver.(interface {
			SnapshotDetailsContext(context.Context) ([]Route, []RouteDetail, error)
		}); ok {
			routes, details, last = reader.SnapshotDetailsContext(ctx)
		} else {
			routes, details, last = snapshotDetails(g.c.opts.Driver)
		}
		if ctx.Err() != nil {
			return nil, nil, attempts, errors.Join(ctx.Err(), last)
		}
		if last == nil {
			return routes, details, attempts, nil
		}
		timer := time.NewTimer(50 * time.Millisecond)
		select {
		case <-ctx.Done():
			timer.Stop()
			return nil, nil, attempts, errors.Join(ctx.Err(), last)
		case <-timer.C:
		}
	}
}

func (g *Guard) System(destination netip.Addr) (Binding, error) {
	if !g.c.active.Load() {
		return Binding{}, ErrEgressChanged
	}
	if err := g.Validate(true); err != nil {
		return Binding{}, err
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	name := SystemInterface(g.routes, destination)
	if name == "" {
		return Binding{}, ErrEgressChanged
	}
	f := 0
	if destination.Is6() {
		f = 1
	}
	if !tunnel(name) && g.now().Before(g.settle[f]) {
		return Binding{}, ErrEgressChanged
	}
	b, err := g.bindingLocked(name)
	if err != nil {
		return b, err
	}
	// A route snapshot and the current interface must describe the same lifetime.
	for _, r := range g.routes {
		if r.Interface == name && r.Index != b.Index {
			return Binding{}, ErrEgressChanged
		}
	}
	b.generation = g.generation
	return b, nil
}

// Admit drops VPN bypass destinations even before withdrawal events arrive.
// Their disappearance must never authorize physical rules or system fallback.
func (g *Guard) Admit(destination netip.Addr) error {
	if err := g.Err(); err != nil {
		return err
	}
	if !g.c.active.Load() {
		return ErrEgressChanged
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	bits := -1
	for _, p := range g.c.Plan.Prefixes {
		if p.Contains(destination) && p.Bits() > bits {
			bits = p.Bits()
		}
	}
	for p := range g.protected {
		if p.Contains(destination) && p.Bits() > bits {
			return errors.New("VPN bypass destination entered capture")
		}
	}
	return nil
}

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
	guard   *Guard
	binding Binding
	stop    func() bool
}

func (g *Guard) Conn(conn net.Conn, bindings ...Binding) net.Conn {
	c := &guardedConn{Conn: conn, guard: g}
	if len(bindings) > 0 {
		c.binding = bindings[0]
		c.binding.generation = 0
	}
	c.stop = context.AfterFunc(g.ctx, func() { conn.Close() })
	return c
}
func (c *guardedConn) check() error {
	if c.binding.guard != nil {
		return c.binding.Check()
	}
	return c.guard.Err()
}
func (c *guardedConn) Close() error { c.stop(); return c.Conn.Close() }
func (c *guardedConn) Read(b []byte) (int, error) {
	if err := c.check(); err != nil {
		return 0, err
	}
	return c.Conn.Read(b)
}
func (c *guardedConn) Write(b []byte) (int, error) {
	if err := c.check(); err != nil {
		return 0, err
	}
	return c.Conn.Write(b)
}
func (c *guardedConn) CloseWrite() error {
	if err := c.check(); err != nil {
		return err
	}
	if conn, ok := c.Conn.(interface{ CloseWrite() error }); ok {
		return conn.CloseWrite()
	}
	return errors.New("connection does not support write half-close")
}
