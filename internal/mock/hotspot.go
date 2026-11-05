package mock

import (
	"context"

	"github.com/Kel077/pith/internal/api"
)

// Pith talks to no hotspot yet.

func (s *Service) Hotspot(ctx context.Context, iface string) (api.Hotspot, error) {
	return api.Hotspot{Interface: iface}, &api.Error{Code: api.ErrUnsupported}
}

func (s *Service) SetHotspotPassword(ctx context.Context, iface, password string) error {
	return &api.Error{Code: api.ErrUnsupported}
}

func (s *Service) RenewHotspot(ctx context.Context, iface string) (api.HotspotRenewal, error) {
	return api.HotspotRenewal{}, &api.Error{Code: api.ErrUnsupported}
}
