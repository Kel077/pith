package mock

import (
	"time"

	"github.com/Kel077/pith/internal/api"
)

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
			if i := p.Via.Resolve(s.ifaces); i == nil || !i.Up {
				st.Error = &api.Error{Code: api.ErrInterfaceUnavailable}
			} else if len(i.DNS) == 0 {
				st.Error = &api.Error{Code: api.ErrDNSUnavailable}
			}
		}
		out = append(out, st)
	}
	return out
}

// restartProxiesLocked forgets the counters of the proxies that a new
// configuration removes, turns off or sends through another interface, as
// the client restarts their listeners; the others keep theirs.
func (s *Service) restartProxiesLocked(next []api.Proxy) {
	on := map[int]api.Proxy{}
	for _, p := range next {
		if p.Enabled {
			on[p.Port] = p
		}
	}
	for _, p := range s.cfg.Proxies {
		if n, ok := on[p.Port]; !ok || n.Via.Name != p.Via.Name || n.Via.HardwareAddr != p.Via.HardwareAddr {
			delete(s.proxies, p.Port)
		}
	}
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
}
