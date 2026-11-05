package mock

import (
	"context"
	"time"

	"github.com/Kel077/pith/internal/api"
)

// hotspotIface leads to a mobile hotspot the mock pretends to control.
const hotspotIface = "en9"

func (s *Service) Hotspot(ctx context.Context, iface string) (api.Hotspot, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
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
	case iface != hotspotIface:
		return r, &api.Error{Code: api.ErrUnsupported, Detail: iface + " leads to no hotspot Pith knows"}
	case locked:
		return r, &api.Error{Code: api.ErrWrongPassword, Detail: "no admin password"}
	}
	var err error
	if r.Before, err = s.Probe(ctx, iface, ""); err != nil {
		return r, err
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
	s.renewals++
	s.mu.Unlock()
	r.After, err = s.Probe(ctx, iface, "")
	return r, err
}

// LockHotspot forgets the hotspot's admin password, for the views of a
// hotspot Pith can't log in to.
func (s *Service) LockHotspot() {
	s.mu.Lock()
	s.hotspotLocked = true
	s.mu.Unlock()
}
