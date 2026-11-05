package client

import (
	"context"
	"errors"
	"net"
	"net/http"
	"net/http/cookiejar"
	"net/netip"
	"time"

	"github.com/Kel077/pith/internal/api"
	"github.com/Kel077/pith/internal/hotspot"
	"github.com/Kel077/pith/internal/keychain"
)

// hotspotKeychain is the keychain service of the hotspots' admin passwords;
// the account is the hardware address of the interface a hotspot serves.
const hotspotKeychain = "io.github.kel077.pith.hotspot"

// A hotspot's session lasts while its interface keeps its name, hardware
// address and router.
type hotspotSession struct {
	key     string
	device  *hotspot.Inseego
	account string
}

func (c *Client) Hotspot(ctx context.Context, iface string) (api.Hotspot, error) {
	s, err := c.hotspot(ctx, iface)
	if err != nil {
		return api.Hotspot{Interface: iface}, err
	}
	h, err := s.device.Status(ctx)
	h.Interface = iface
	return h, err
}

func (c *Client) SetHotspotPassword(ctx context.Context, iface, password string) error {
	s, err := c.hotspot(ctx, iface)
	if err != nil {
		return err
	}
	if err := s.device.TryPassword(ctx, password); err != nil {
		return err
	}
	return c.secretSet(ctx, hotspotKeychain, s.account, password)
}

func (c *Client) RenewHotspot(ctx context.Context, iface string) (api.HotspotRenewal, error) {
	var r api.HotspotRenewal
	s, err := c.hotspot(ctx, iface)
	if err != nil {
		return r, err
	}
	if r.Before, err = c.Probe(ctx, iface, ""); err != nil {
		return r, err
	}
	started := time.Now()
	if err := s.device.Renew(ctx); err != nil {
		return r, err
	}
	r.Took = time.Since(started)
	// A hotspot connected again may still be setting up its addresses.
	for range 3 {
		if r.After, err = c.Probe(ctx, iface, ""); err != nil || r.After.OK {
			break
		}
		select {
		case <-ctx.Done():
			return r, ctx.Err()
		case <-time.After(2 * time.Second):
		}
	}
	return r, err
}

// hotspot finds the hotspot behind an interface, or ErrUnsupported.
func (c *Client) hotspot(ctx context.Context, name string) (*hotspotSession, error) {
	ifaces, err := c.interfaces(ctx)
	if err != nil {
		return nil, err
	}
	var link *api.Interface
	for i := range ifaces {
		if ifaces[i].Name == name {
			link = &ifaces[i]
		}
	}
	unsupported := &api.Error{Code: api.ErrUnsupported, Detail: name + " leads to no hotspot Pith knows"}
	if link == nil || !link.Up {
		return nil, unsupported
	}
	model := hotspot.Model(link.Label)
	router, err := netip.ParseAddr(link.Router)
	if model == "" || err != nil {
		return nil, unsupported
	}
	key := name + "|" + link.HardwareAddr + "|" + link.Router
	c.mu.Lock()
	defer c.mu.Unlock()
	if s := c.hotspots[key]; s != nil {
		return s, nil
	}
	dialer, err := c.dialer(name, 5*time.Second, nil)
	if err != nil {
		return nil, err
	}
	jar, err := cookiejar.New(nil)
	if err != nil {
		return nil, err
	}
	port := c.hotspotPort
	if port == "" {
		port = "80"
	}
	base := "http://" + net.JoinHostPort(router.String(), port)
	account := link.HardwareAddr
	if account == "" {
		account = name
	}
	s := &hotspotSession{key: key, account: account, device: &hotspot.Inseego{
		Base: base, Model: model,
		HTTP: &http.Client{
			Jar: jar, Timeout: 10 * time.Second,
			// Straight to the hotspot, through its interface: no proxy, and
			// no redirect anywhere else.
			Transport: &http.Transport{DialContext: dialer.DialContext, Proxy: nil, MaxIdleConns: 2},
			CheckRedirect: func(req *http.Request, via []*http.Request) error {
				if req.URL.Host != via[0].URL.Host || len(via) > 3 {
					return http.ErrUseLastResponse
				}
				return nil
			},
		},
		Password: func() (string, error) {
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			secret, err := c.secretGet(ctx, hotspotKeychain, account)
			if errors.Is(err, keychain.ErrNotFound) {
				return "", nil
			}
			return secret, err
		},
	}}
	if c.hotspots == nil {
		c.hotspots = map[string]*hotspotSession{}
	}
	for k, old := range c.hotspots { // the interface's old session, if it changed
		if old.key != key && len(k) > len(name) && k[:len(name)+1] == name+"|" {
			delete(c.hotspots, k)
		}
	}
	c.hotspots[key] = s
	return s, nil
}
