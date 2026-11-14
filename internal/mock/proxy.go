package mock

import (
	"cmp"
	"context"
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
			if p.Upstream != "" || p.Scene != "" {
				// The proxy it goes out through answers; the rules of its
				// scene have somewhere to send each connection.
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

func (s *Service) ChangeProxyIP(ctx context.Context, port int) (api.ProxyIPChange, error) {
	return api.ProxyIPChange{Port: port}, &api.Error{Code: api.ErrUnsupported}
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
