package mock

import (
	"context"
	"fmt"
	"time"

	"github.com/Kel077/pith/internal/api"
)

// routerIface leads to a router the mock pretends to control.
const routerIface = "en0"

func (s *Service) Router(ctx context.Context, iface string) (api.Router, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if iface != routerIface {
		return api.Router{Interface: iface}, &api.Error{Code: api.ErrUnsupported, Detail: iface + " leads to no router Pith knows"}
	}
	r := api.Router{Interface: iface, Model: "CHR30A-R", Locked: s.routerLocked, At: s.now()}
	if !r.Locked {
		r.Connected, r.PublicIPv4 = true, s.ethernetIPv4Locked()
	}
	return r, nil
}

func (s *Service) SetRouterPassword(ctx context.Context, iface, password string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	switch {
	case iface != routerIface:
		return &api.Error{Code: api.ErrUnsupported, Detail: iface + " leads to no router Pith knows"}
	case password == "" || password == "wrong":
		return &api.Error{Code: api.ErrWrongPassword, Detail: "the router refused the admin password"}
	}
	s.routerLocked = false
	return nil
}

func (s *Service) RenewRouter(ctx context.Context, iface string) (api.RouterRenewal, error) {
	r := api.RouterRenewal{Standby: hotspotIface}
	s.mu.Lock()
	locked := s.routerLocked
	s.mu.Unlock()
	switch {
	case iface != routerIface:
		return r, &api.Error{Code: api.ErrUnsupported, Detail: iface + " leads to no router Pith knows"}
	case locked:
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
	// The provider gives the address back the first time.
	r.Tries, r.Took = 2, 41*time.Second
	if s.live { // a router takes a while to get an address again
		select {
		case <-ctx.Done():
			return r, ctx.Err()
		case <-time.After(4 * time.Second):
		}
	}
	s.mu.Lock()
	s.routerRenewals++
	s.mu.Unlock()
	r.After, err = s.Probe(ctx, iface, "")
	return r, err
}

// ethernetIPv4Locked is the public IPv4 address of the sample Ethernet's
// router, which each renewal changes.
func (s *Service) ethernetIPv4Locked() string {
	return fmt.Sprintf("203.0.113.%d", 24+s.routerRenewals%100)
}

// LockRouter forgets the router's admin password, for the views of a router
// Pith can't log in to.
func (s *Service) LockRouter() {
	s.mu.Lock()
	s.routerLocked = true
	s.mu.Unlock()
}
