package client

import (
	"context"

	"github.com/Kel077/pith/internal/api"
)

// Pith talks to no hotspot yet.

func (c *Client) Hotspot(ctx context.Context, iface string) (api.Hotspot, error) {
	return api.Hotspot{Interface: iface}, &api.Error{Code: api.ErrUnsupported}
}

func (c *Client) SetHotspotPassword(ctx context.Context, iface, password string) error {
	return &api.Error{Code: api.ErrUnsupported}
}

func (c *Client) RenewHotspot(ctx context.Context, iface string) (api.HotspotRenewal, error) {
	return api.HotspotRenewal{}, &api.Error{Code: api.ErrUnsupported}
}
