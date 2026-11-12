package client

import (
	"context"
	"errors"
	"net"
	"net/http"
	"net/netip"
	"strconv"
	"time"

	"github.com/Kel077/pith/internal/api"
	"github.com/Kel077/pith/internal/hotspot"
	"github.com/Kel077/pith/internal/keychain"
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
	// Connected again, a hotspot may still be setting up its addresses and
	// its DNS: the M2000's refuses queries for some ten seconds more.
	r.After, err = c.retest(ctx, iface, r.Before)
	return r, err
}

// retest tests the internet through an interface that just got new public
// addresses, every pace until it finds as many as before did, for 25 paces at
// most, and 3 more once a family works: one that doesn't come back with the
// other may not come back at all. It returns the test that found the most.
func (c *Client) retest(ctx context.Context, iface string, before api.Probe) (api.Probe, error) {
	pace := c.hotspotPace
	if pace == 0 {
		pace = time.Second
	}
	var after api.Probe
	want := max(1, publicIPs(before))
	deadline := time.Now().Add(25 * pace)
	for {
		p, err := c.Probe(ctx, iface, "")
		if err != nil {
			return after, err
		}
		if publicIPs(p) >= publicIPs(after) {
			after = p
		}
		if publicIPs(after) >= want {
			return after, nil
		}
		if late := time.Now().Add(3 * pace); after.OK && late.Before(deadline) {
			deadline = late
		}
		if time.Now().Add(pace).After(deadline) {
			return after, nil
		}
		select {
		case <-ctx.Done():
			return after, ctx.Err()
		case <-time.After(pace):
		}
	}
}

// publicIPs counts the public addresses a probe found, one per family.
func publicIPs(p api.Probe) int {
	n := 0
	if p.PublicIPv4 != "" {
		n++
	}
	if p.PublicIPv6 != "" {
		n++
	}
	return n
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
	port := c.hotspotPort
	if port == "" {
		port = "80"
	}
	base := "http://" + net.JoinHostPort(router.String(), port)
	account := link.HardwareAddr
	client, transport, err := c.deviceClient(name, *link, index, nil)
	if err != nil {
		return nil, err
	}
	s := &hotspotSession{key: key, account: account, transport: transport, device: &hotspot.Inseego{
		Base: base, Model: model, HTTP: client,
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
