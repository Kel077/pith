package client

import (
	"context"
	"time"

	"github.com/Kel077/pith/internal/api"
)

func (c *Client) ChangeProxyIP(ctx context.Context, port int) (api.ProxyIPChange, error) {
	c.proxyMu.Lock()
	manager := c.proxies
	c.proxyMu.Unlock()
	if manager == nil {
		return api.ProxyIPChange{Port: port}, &api.Error{Code: api.ErrUnsupported, Detail: "no local proxy listens"}
	}
	return manager.ChangeIP(ctx, port)
}

// exitInfo tells a local proxy's port of the interface it goes out through:
// proxy.Options.Info.
func (c *Client) exitInfo(ctx context.Context, via api.InterfaceRef, maxAge time.Duration) (api.ProxyInfo, error) {
	return api.ProxyInfo{}, &api.Error{Code: api.ErrUnsupported, Detail: "not written yet"}
}

// changeExitIP changes the public address of the interface a local proxy
// goes out through: proxy.Options.ChangeIP.
func (c *Client) changeExitIP(ctx context.Context, via api.InterfaceRef) (api.ProxyIPChange, error) {
	return api.ProxyIPChange{}, &api.Error{Code: api.ErrUnsupported, Detail: "not written yet"}
}

// exitChanging tells whether the public address of an interface is being
// changed now: proxy.Options.Changing.
func (c *Client) exitChanging(via api.InterfaceRef) bool { return false }
