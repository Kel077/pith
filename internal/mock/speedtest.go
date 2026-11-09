package mock

import (
	"context"

	"github.com/Kel077/pith/internal/api"
)

// The mock tests no speed yet.

func (s *Service) SpeedTest(ctx context.Context, iface string) (api.SpeedTest, error) {
	return api.SpeedTest{Interface: iface}, &api.Error{Code: api.ErrUnsupported}
}
