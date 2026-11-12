package mock

import (
	"context"

	"github.com/Kel077/pith/internal/api"
)

func (s *Service) Router(ctx context.Context, iface string) (api.Router, error) {
	return api.Router{Interface: iface}, &api.Error{Code: api.ErrUnsupported, Detail: iface + " leads to no router Pith knows"}
}

func (s *Service) SetRouterPassword(ctx context.Context, iface, password string) error {
	return &api.Error{Code: api.ErrUnsupported, Detail: iface + " leads to no router Pith knows"}
}

func (s *Service) RenewRouter(ctx context.Context, iface string) (api.RouterRenewal, error) {
	return api.RouterRenewal{}, &api.Error{Code: api.ErrUnsupported, Detail: iface + " leads to no router Pith knows"}
}
