package mock

import (
	"cmp"
	"context"
	"fmt"
	"net/netip"
	"slices"
	"time"

	"github.com/Kel077/pith/internal/api"
)

// stranger is a device that tries the sample's shared proxies without
// being one of its devices, for the interface to offer to allow.
const stranger = "192.0.2.44"

// proxiesLocked is the status of the configured local proxies, as the
// client reports it: in the configuration's order, turned-off ones too.
func (s *Service) proxiesLocked() []api.ProxyStatus {
	var out []api.ProxyStatus
	for _, p := range s.cfg.Proxies {
		st := api.ProxyStatus{Port: p.Port}
		switch {
		case !p.Enabled:
		case p.Port == s.portInUse:
			st.Error = &api.Error{Code: api.ErrPortInUse}
		default:
			live := s.proxies[p.Port]
			if live == nil {
				live = &api.ProxyStatus{Port: p.Port, Listening: true, Since: s.now()}
				s.proxies[p.Port] = live
			}
			st = *live
			st.Changing = s.changes[p.Port] != nil || p.Upstream == "" && p.Scene == "" && s.renewing[p.Via.Name] > 0
			if p.Upstream != "" {
				// The proxy it goes out through answers, as a port of another
				// Mac's Pith does.
				st.Upstream = s.upstreamLocked(p)
			} else if p.Scene != "" {
				// The rules of its scene have somewhere to send each connection.
			} else if i := p.Via.Resolve(s.ifaces); i == nil || !i.Up {
				st.Error = &api.Error{Code: api.ErrInterfaceUnavailable}
			} else if len(i.DNS) == 0 {
				st.Error = &api.Error{Code: api.ErrDNSUnavailable}
			}
		}
		out = append(out, st)
	}
	return out
}

// upstreamInterface is the name of the hotspot the mock pretends every
// upstream proxy goes out through: each is a port of another Mac's Pith.
const upstreamInterface = "5G Hotspot"

// upstreamLocked is what the Pith behind the upstream of p says of itself.
func (s *Service) upstreamLocked(p api.Proxy) *api.ProxyInfo {
	_, port, err := api.ParseUpstream(p.Upstream)
	if err != nil {
		return nil
	}
	return &api.ProxyInfo{Pith: api.ProxyInfoVersion, Port: port, Exit: api.ExitInterface, Interface: upstreamInterface, Online: true,
		PublicIPv4: fmt.Sprintf("198.51.100.%d", 140+s.upstream%100), PublicIPv6: fmt.Sprintf("2001:db8:%x::1", 0x140+s.upstream),
		At: s.born, CanChangeIP: !s.fixed, Changing: s.changes[p.Port] != nil}
}

// ForbidUpstreamChanges has the Pith behind every upstream refuse to change
// its address for this Mac, as its port does with ChangeIP off: for the
// views of a proxy whose IP can't be changed from here.
func (s *Service) ForbidUpstreamChanges() {
	s.mu.Lock()
	s.fixed = true
	s.mu.Unlock()
}

// proxyChange is a change of a proxy's public address under way. Like the
// client's, it goes on when the caller who asked for it leaves, and another
// call for the port waits for it and gets what it did.
type proxyChange struct {
	done   chan struct{}
	change api.ProxyIPChange
	err    error
}

func (s *Service) ChangeProxyIP(ctx context.Context, port int) (api.ProxyIPChange, error) {
	change := api.ProxyIPChange{Port: port}
	s.mu.Lock()
	var p api.Proxy
	for _, q := range s.cfg.Proxies {
		if q.Port == port && q.Enabled {
			p = q
		}
	}
	if p.Port == 0 || p.Scene != "" {
		s.mu.Unlock()
		return change, &api.Error{Code: api.ErrUnsupported, Detail: "no address of this proxy to change"}
	}
	if p.Upstream != "" && s.fixed {
		s.mu.Unlock()
		return change, &api.Error{Code: api.ErrNotAllowed, Detail: "the upstream port does not allow address changes"}
	}
	c := s.changes[port]
	if c == nil {
		c = &proxyChange{done: make(chan struct{})}
		s.changes[port] = c
		go func() {
			c.change, c.err = s.changeProxyIP(context.WithoutCancel(ctx), p)
			s.mu.Lock()
			delete(s.changes, port)
			s.mu.Unlock()
			close(c.done)
		}()
	}
	s.mu.Unlock()
	select {
	case <-ctx.Done():
		return change, ctx.Err()
	case <-c.done:
		return c.change, c.err
	}
}

func (s *Service) changeProxyIP(ctx context.Context, p api.Proxy) (api.ProxyIPChange, error) {
	change := api.ProxyIPChange{Port: p.Port}
	var before, after api.Probe
	var took time.Duration
	switch {
	case p.Upstream != "":
		// The other Mac's hotspot reconnects to its carrier.
		s.mu.Lock()
		was := s.upstreamLocked(p)
		s.mu.Unlock()
		if s.live {
			select {
			case <-ctx.Done():
				return change, ctx.Err()
			case <-time.After(3 * time.Second):
			}
		}
		s.mu.Lock()
		s.upstream++
		now := s.upstreamLocked(p)
		s.mu.Unlock()
		before = api.Probe{PublicIPv4: was.PublicIPv4, PublicIPv6: was.PublicIPv6}
		after, took = api.Probe{PublicIPv4: now.PublicIPv4, PublicIPv6: now.PublicIPv6, At: s.now()}, 14*time.Second
	case p.Via.Name == hotspotIface:
		r, err := s.RenewHotspot(ctx, p.Via.Name)
		if err != nil {
			return change, err
		}
		before, after, took = r.Before, r.After, r.Took
	default:
		r, err := s.RenewRouter(ctx, p.Via.Name)
		if err != nil {
			return change, err
		}
		before, after, took = r.Before, r.After, r.Took
	}
	change.BeforeIPv4, change.BeforeIPv6 = before.PublicIPv4, before.PublicIPv6
	change.PublicIPv4, change.PublicIPv6, change.At, change.Seconds = after.PublicIPv4, after.PublicIPv6, after.At, took.Seconds()
	return change, nil
}

// restartProxiesLocked forgets the counters of the proxies that a new
// configuration removes, turns off, sends through another interface, proxy
// or scene's rules, or starts or stops sharing, as the client restarts their
// listeners; the others keep theirs. It forgets the devices the new
// configuration no longer lists, too.
func (s *Service) restartProxiesLocked(next api.Config) {
	on := map[int]api.Proxy{}
	for _, p := range next.Proxies {
		if p.Enabled {
			on[p.Port] = p
		}
	}
	for _, p := range s.cfg.Proxies {
		if n, ok := on[p.Port]; !ok || n.Via.Name != p.Via.Name || n.Via.HardwareAddr != p.Via.HardwareAddr || n.Upstream != p.Upstream || n.Scene != p.Scene || n.Shared != p.Shared {
			delete(s.proxies, p.Port)
		}
	}
	for address := range s.devices {
		if addr, err := netip.ParseAddr(address); err != nil || !allows(next.Devices, addr) {
			delete(s.devices, address)
		}
	}
}

// allows tells whether one of devices covers addr.
func allows(devices []api.Device, addr netip.Addr) bool {
	for _, d := range devices {
		if prefix, err := d.Prefix(); err == nil && prefix.Contains(addr) {
			return true
		}
	}
	return false
}

// devicesLocked is the other devices that use the shared proxies, as the
// client reports them: the devices the configuration lists one by one, as
// if each had a browser going through a shared proxy, and the stranger,
// refused until the configuration lists it. None when no proxy that
// listens is shared.
func (s *Service) devicesLocked() []api.DeviceStatus {
	shared := false
	for _, st := range s.proxiesLocked() {
		for _, p := range s.cfg.Proxies {
			shared = shared || p.Port == st.Port && p.Shared && st.Listening
		}
	}
	if !shared {
		return nil
	}
	var out []api.DeviceStatus
	seen := map[string]bool{}
	add := func(addr netip.Addr, allowed bool) {
		address := addr.String()
		if seen[address] {
			return
		}
		seen[address] = true
		live := s.devices[address]
		if live == nil {
			live = &api.DeviceStatus{Address: address, Seen: s.born}
			s.devices[address] = live
		}
		live.Allowed = allowed
		out = append(out, *live)
	}
	for _, d := range s.cfg.Devices {
		if prefix, err := d.Prefix(); err == nil && prefix.IsSingleIP() {
			add(prefix.Addr(), true)
		}
	}
	if s.stranger {
		addr := netip.MustParseAddr(stranger)
		add(addr, allows(s.cfg.Devices, addr))
	}
	slices.SortStableFunc(out, func(a, b api.DeviceStatus) int {
		return cmp.Or(b.Seen.Compare(a.Seen), cmp.Compare(a.Address, b.Address))
	})
	return out
}

// stepProxiesLocked moves the counters of the proxies that can reach the
// internet, as a browser using them would, without touching the
// simulation's randomness.
func (s *Service) stepProxiesLocked(d time.Duration) {
	for _, st := range s.proxiesLocked() {
		live := s.proxies[st.Port]
		if live == nil {
			continue
		}
		if st.Error != nil {
			live.Open = 0
			continue
		}
		secs, weight := d.Seconds(), float64(1+st.Port%3)
		live.Open = 1 + int(s.now().Unix()/7+int64(st.Port))%3
		live.Sent += uint64(secs * 30_000 * weight)
		live.Received += uint64(secs * 600_000 * weight)
	}
	for k, st := range s.devicesLocked() {
		if !st.Allowed {
			continue
		}
		live := s.devices[st.Address]
		secs, weight := d.Seconds(), float64(1+k%3)
		live.Seen = s.now()
		live.Open = 1 + int(s.now().Unix()/11+int64(k))%2
		live.Sent += uint64(secs * 12_000 * weight)
		live.Received += uint64(secs * 240_000 * weight)
	}
}
