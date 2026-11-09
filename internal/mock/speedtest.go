package mock

import (
	"context"
	"fmt"
	"time"

	"github.com/Kel077/pith/internal/api"
)

// SpeedTest gives each kind of interface speeds of its kind, a little
// different each time.
func (s *Service) SpeedTest(ctx context.Context, iface string) (api.SpeedTest, error) {
	s.mu.Lock()
	i := s.iface(iface)
	var r api.SpeedTest
	switch {
	case i == nil:
		s.mu.Unlock()
		return api.SpeedTest{}, &api.Error{Code: api.ErrInternal, Detail: "no interface " + iface}
	case !i.Up:
		r = api.SpeedTest{Interface: iface, At: s.now(), Problem: "interface is down"}
	default:
		const mbps = 1_000_000
		latency, down, up, ip := 6*time.Millisecond, int64(700*mbps), int64(150*mbps), "203.0.113.24"
		switch {
		case i.Name == hotspotIface:
			latency, down, up, ip = 34*time.Millisecond, 140*mbps, 23*mbps, fmt.Sprintf("198.51.100.%d", 80+s.renewals%100)
		case i.Kind == api.KindCellular:
			latency, down, up, ip = 41*time.Millisecond, 90*mbps, 18*mbps, "198.51.100.77"
		case i.Kind == api.KindWiFi:
			latency, down, up = 11*time.Millisecond, 310*mbps, 42*mbps
		}
		jitter := func(n int64) int64 { return n + n*int64(s.rnd.IntN(11)-5)/100 }
		r = api.SpeedTest{Interface: iface, At: s.now(), OK: true, Server: "SJC", Latency: latency + time.Duration(s.rnd.IntN(4))*time.Millisecond,
			Download: jitter(down), Upload: jitter(up), PublicIP: ip}
	}
	s.mu.Unlock()
	select { // a test takes a few seconds
	case <-ctx.Done():
		return api.SpeedTest{}, ctx.Err()
	case <-time.After(time.Second):
	}
	return r, nil
}
