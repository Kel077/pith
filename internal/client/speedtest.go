package client

import (
	"context"

	"github.com/Kel077/pith/internal/api"
)

// Pith tests no speed yet.

func (c *Client) SpeedTest(ctx context.Context, iface string) (api.SpeedTest, error) {
	return api.SpeedTest{Interface: iface}, &api.Error{Code: api.ErrUnsupported}
}
