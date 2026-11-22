package mock

import (
	"context"
	"time"

	"github.com/Kel077/pith/internal/api"
)

// hotspotIface leads to a mobile hotspot the mock pretends to control.
const hotspotIface = "en9"
const rmnetIface = "cell-demo"

func (s *Service) Hotspot(ctx context.Context, iface string) (api.Hotspot, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if iface == rmnetIface {
		h := api.Hotspot{Interface: iface, Model: api.HotspotRM520N, Connected: s.iface(iface).Up, At: s.now()}
		if h.Connected {
			h.Technology, h.Bars = "5G SA", 4
			h.Cell = &api.HotspotCell{Operator: "Example Mobile", Band: "n78", Signal: -86, SNR: 19,
				RSRP: new(-86), RSRQ: new(-9), SINR: new(18.5), Carriers: []api.HotspotCarrier{
					{Role: "PCC", RAT: "NR5G", Band: "n78", BandwidthMHz: 80},
					{Role: "SCC", RAT: "NR5G", Band: "n78", BandwidthMHz: 20},
				}}
		}
		return h, nil
	}
	if iface != hotspotIface {
		return api.Hotspot{Interface: iface}, &api.Error{Code: api.ErrUnsupported, Detail: iface + " leads to no hotspot Pith knows"}
	}
	h := api.Hotspot{Interface: iface, Model: "M2000", Connected: true, Technology: "5G", Bars: 4, Locked: s.hotspotLocked, At: s.now()}
	if !h.Locked {
		h.Cell = &api.HotspotCell{Operator: "T-Mobile", Band: "n41", Signal: -92, SNR: 17}
	}
	return h, nil
}

func (s *Service) SetHotspotPassword(ctx context.Context, iface, password string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	switch {
	case iface == rmnetIface:
		return &api.Error{Code: api.ErrUnsupported, Detail: "RMNet does not use a hotspot admin password"}
	case iface != hotspotIface:
		return &api.Error{Code: api.ErrUnsupported, Detail: iface + " leads to no hotspot Pith knows"}
	case password == "" || password == "wrong":
		return &api.Error{Code: api.ErrWrongPassword, Detail: "the hotspot refused the admin password"}
	}
	s.hotspotLocked = false
	return nil
}

func (s *Service) RenewHotspot(ctx context.Context, iface string) (api.HotspotRenewal, error) {
	var r api.HotspotRenewal
	s.mu.Lock()
	locked := s.hotspotLocked
	s.mu.Unlock()
	switch {
	case iface != hotspotIface && iface != rmnetIface:
		return r, &api.Error{Code: api.ErrUnsupported, Detail: iface + " leads to no hotspot Pith knows"}
	case iface == hotspotIface && locked:
		return r, &api.Error{Code: api.ErrWrongPassword, Detail: "no admin password"}
	}
	if err := s.beginRenewal(iface); err != nil {
		return r, err
	}
	defer s.endRenewal(iface)
	var err error
	if r.Before, err = s.Probe(ctx, iface, ""); err != nil {
		return r, err
	}
	if iface == rmnetIface && !r.Before.OK {
		return r, &api.Error{Code: api.ErrInterfaceUnavailable, Detail: "the modem is offline"}
	}
	r.Took = 14 * time.Second
	if s.live { // a hotspot takes a while to connect again
		select {
		case <-ctx.Done():
			return r, ctx.Err()
		case <-time.After(3 * time.Second):
		}
	}
	s.mu.Lock()
	s.renewals[iface]++
	s.mu.Unlock()
	r.After, err = s.Probe(ctx, iface, "")
	return r, err
}

// beginRenewal counts a change of iface's public address in: ErrBusy while
// another interface's is being changed, as the client has one change at a
// time, so that the interface carrying the Mac while a router renews keeps
// working. endRenewal counts it out.
func (s *Service) beginRenewal(iface string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	for other := range s.renewing {
		if other != iface {
			return &api.Error{Code: api.ErrBusy, Detail: "the address of " + other + " is being changed"}
		}
	}
	s.renewing[iface]++
	return nil
}

func (s *Service) endRenewal(iface string) {
	s.mu.Lock()
	if s.renewing[iface]--; s.renewing[iface] <= 0 {
		delete(s.renewing, iface)
	}
	s.mu.Unlock()
}

// LockHotspot forgets the hotspot's admin password, for the views of a
// hotspot Pith can't log in to.
func (s *Service) LockHotspot() {
	s.mu.Lock()
	s.hotspotLocked = true
	s.mu.Unlock()
}
