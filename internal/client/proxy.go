package client

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"time"

	"github.com/Kel077/pith/internal/api"
	"github.com/Kel077/pith/internal/proxy"
	proxyegress "github.com/Kel077/pith/internal/proxy/egress"
)

var errClientClosed = errors.New("client is closed")

// Close releases local listeners and subscriptions. It neither changes saved
// Enabled flags nor stops the independent privileged routing helper.
func (c *Client) Close() error {
	c.proxyMu.Lock()
	c.closed = true
	c.closeLifetime()
	manager := c.proxies
	c.proxyMu.Unlock()
	var err error
	if manager != nil {
		err = manager.Close()
	}
	c.subscriptions.Wait()
	c.http.CloseIdleConnections()
	return err
}

func (c *Client) validateProxyInterfaces(ctx context.Context, configs []api.Proxy) error {
	if len(configs) == 0 {
		return nil
	}
	ifaces, err := c.interfaces(ctx)
	if err != nil {
		// An unavailable inventory cannot invalidate an unplugged adapter's saved
		// intent. The strict dialer still requires fresh usable identity before I/O.
		return ctx.Err()
	}
	for _, config := range configs {
		if iface := config.Via.Resolve(ifaces); iface != nil && !iface.CanProxy() {
			return &api.Error{Code: api.ErrInvalidConfig, Detail: fmt.Sprintf("proxy %d: selected interface is not a supported physical exit", config.Port)}
		}
	}
	return nil
}

func (c *Client) syncProxies(parent context.Context, configs []api.Proxy, firstLoad bool) error {
	c.proxyMu.Lock()
	if c.closed {
		c.proxyMu.Unlock()
		return errClientClosed
	}
	if firstLoad && c.proxiesLoaded {
		c.proxyMu.Unlock()
		return parent.Err()
	}
	if c.proxies == nil {
		factory := c.newProxies
		if factory == nil {
			factory = func() (*proxy.Manager, error) {
				egress := proxyegress.New(proxyegress.Options{Interfaces: func(ctx context.Context) ([]api.Interface, error) { return c.interfaces(ctx) }})
				return proxy.New(proxy.Options{Dial: egress.Dial, Health: egress.Health})
			}
		}
		manager, err := factory()
		if err != nil {
			c.proxyMu.Unlock()
			return err
		}
		c.proxies = manager
	}
	manager := c.proxies
	c.proxyMu.Unlock()
	// After the configuration was committed, caller cancellation cannot leave
	// an obsolete enabled listener open. Application Close still cancels setup.
	base := c.lifetime
	if firstLoad {
		base = parent
	}
	ctx, cancel := context.WithTimeout(base, 2*time.Second)
	defer cancel()
	stopClose := context.AfterFunc(c.lifetime, cancel)
	defer stopClose()
	err := manager.Apply(ctx, configs)
	if firstLoad && parent.Err() != nil {
		// Loading is not a committed Save. Retract any initial listeners that
		// were opened before the caller canceled, so a failed load stays idle.
		undoCtx, undoCancel := context.WithTimeout(c.lifetime, 2*time.Second)
		defer undoCancel()
		return errors.Join(parent.Err(), manager.Apply(undoCtx, nil))
	}
	if err != nil {
		return err
	}
	c.proxyMu.Lock()
	defer c.proxyMu.Unlock()
	if c.closed {
		return errClientClosed
	}
	c.proxiesLoaded = true
	return nil
}

func (c *Client) proxyStatus(ctx context.Context) []api.ProxyStatus {
	c.proxyMu.Lock()
	manager := c.proxies
	c.proxyMu.Unlock()
	if manager == nil {
		return nil
	}
	states := manager.Status(ctx)
	if len(states) == 0 {
		return nil
	}
	return states
}

func (c *Client) withProxyStatus(ctx context.Context, status api.Status) api.Status {
	// Helper JSON cannot supply or replace state owned by this App process.
	status.Proxies = c.proxyStatus(ctx)
	return status
}

func sameRoutingConfig(a, b api.Config) bool {
	return reflect.DeepEqual(a.Scenes, b.Scenes) && a.Settings.IPv6 == b.Settings.IPv6 &&
		a.Settings.SniffDomains == b.Settings.SniffDomains && a.Settings.SafeStart == b.Settings.SafeStart && a.Settings.LogDays == b.Settings.LogDays
}

func sameProxies(a, b []api.Proxy) bool {
	return reflect.DeepEqual(a, b)
}
