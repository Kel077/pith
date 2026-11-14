package client

import (
	"context"

	"github.com/Kel077/pith/internal/api"
)

func (c *Client) ChangeProxyIP(ctx context.Context, port int) (api.ProxyIPChange, error) {
	return api.ProxyIPChange{Port: port}, &api.Error{Code: api.ErrUnsupported, Detail: "not written yet"}
}
