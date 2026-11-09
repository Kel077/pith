package client

import (
	"context"
	"errors"
	"net"
	"net/http"
	"net/http/cookiejar"
	"net/netip"
	"strconv"
	"time"

	"github.com/Kel077/pith/internal/api"
	"github.com/Kel077/pith/internal/hotspot"
	"github.com/Kel077/pith/internal/keychain"
	"github.com/Kel077/pith/internal/netif"
)

// hotspotKeychain is the keychain service of the hotspots' admin passwords;
// the account is the hardware address of the interface a hotspot serves.
const hotspotKeychain = "io.github.kel077.pith.hotspot"

// A hotspot's session lasts while its interface keeps its name, hardware
// address, router and kernel index.
type hotspotSession struct {
	key       string
	device    *hotspot.Inseego
	account   string
	transport *http.Transport
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
	started := time.Now()
	// Its public IP before, tested while the hotspot gets ready.
	err = s.device.Renew(ctx, func(ctx context.Context) (err error) {
		r.Before, err = c.Probe(ctx, iface, "")
		return err
	})
	if err != nil {
		return r, err
	}
	r.Took = time.Since(started)
	// A hotspot connected again may still be setting up its addresses.
	for range 3 {
		if r.After, err = c.Probe(ctx, iface, ""); err == nil && r.After.OK {
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
	if link == nil || !link.Up || link.HardwareAddr == "" {
		return nil, unsupported
	}
	model := hotspot.Model(link.Label)
	router, err := netip.ParseAddr(link.Router)
	if model == "" || err != nil {
		return nil, unsupported
	}
	index, err := c.hotspotIndex(name)
	if err != nil || index <= 0 {
		return nil, unsupported
	}
	key := name + "|" + link.HardwareAddr + "|" + link.Router + "|" + strconv.Itoa(index)
	c.mu.Lock()
	defer c.mu.Unlock()
	if s := c.hotspots[key]; s != nil {
		return s, nil
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
	checkIdentity := func(ctx context.Context) error {
		current, err := c.interfaces(ctx)
		if err != nil {
			return err
		}
		found := (api.InterfaceRef{Name: name, HardwareAddr: account}).Resolve(current)
		if found == nil || found.Name != name || !found.Up || found.Router != link.Router {
			return errors.New("hotspot interface identity changed")
		}
		currentIndex, err := c.hotspotIndex(name)
		if err != nil || currentIndex != index {
			return errors.New("hotspot interface index changed")
		}
		return nil
	}
	transport := &http.Transport{Proxy: nil, MaxIdleConns: 2, IdleConnTimeout: 30 * time.Second, DialContext: func(ctx context.Context, network, target string) (net.Conn, error) {
		if err := checkIdentity(ctx); err != nil {
			return nil, err
		}
		// Resolve the current kernel index for every new socket, even when the
		// same adapter returns with its name, MAC and LAN router unchanged.
		dialer, err := c.dialer(name, 5*time.Second, nil)
		if err != nil {
			return nil, err
		}
		boundCtx := netif.WithBinding(ctx, index, func() error { return checkIdentity(ctx) })
		conn, err := dialer.DialContext(boundCtx, network, target)
		if err != nil {
			return nil, err
		}
		if err := checkIdentity(ctx); err != nil {
			conn.Close()
			return nil, err
		}
		return conn, nil
	}}
	s := &hotspotSession{key: key, account: account, transport: transport, device: &hotspot.Inseego{
		Base: base, Model: model,
		HTTP: &http.Client{
			Jar: jar, Timeout: 10 * time.Second,
			// Straight to the hotspot, through its interface: no proxy, and
			// no redirect anywhere else.
			Transport: hotspotTransport{transport: transport, check: checkIdentity},
			CheckRedirect: func(req *http.Request, via []*http.Request) error {
				if req.URL.Host != via[0].URL.Host || req.URL.Scheme != via[0].URL.Scheme || req.URL.User != nil || len(via) > 3 {
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
			old.transport.CloseIdleConnections()
			delete(c.hotspots, k)
		}
	}
	c.hotspots[key] = s
	return s, nil
}

type hotspotTransport struct {
	transport *http.Transport
	check     func(context.Context) error
}

func (t hotspotTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	// Also validate pooled connections before sending session cookies or forms.
	if err := t.check(req.Context()); err != nil {
		return nil, err
	}
	return t.transport.RoundTrip(req)
}
