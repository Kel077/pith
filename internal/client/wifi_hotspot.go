package client

import (
	"context"
	"net/netip"
	"slices"
	"strings"
	"time"

	"github.com/Kel077/pith/internal/api"
	"github.com/Kel077/pith/internal/hotspot"
)

// Wi-Fi discovery belongs to the IPv4 link and the router's neighbor identity.
// WAN IPv6 renumbering can leave the same hotspot and web session in place.
type wifiHotspot struct {
	link       api.Interface
	index      int
	gatewayMAC string
	done       chan struct{}
	session    *hotspotSession
	err        error
	recognized bool
	at         time.Time
}

func sameWiFiHotspotLink(a, b api.Interface) bool {
	return b.Kind == api.KindWiFi && b.Up && a.Name == b.Name && a.HardwareAddr == b.HardwareAddr &&
		a.Router == b.Router && slices.Equal(a.IPv4, b.IPv4)
}

func (c *Client) wifiHotspotFor(ctx context.Context, link api.Interface, gateway netip.Addr, index int) (*hotspotSession, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	neighbor := c.wifiGatewayMAC(ctx, index, gateway)
	c.proxyMu.Lock()
	if c.closed {
		c.proxyMu.Unlock()
		return nil, errClientClosed
	}
	c.exitMu.Lock()
	state := c.exits[link.Name]
	if state == nil || !sameWiFiHotspotLink(link, state.link) {
		c.exitMu.Unlock()
		c.proxyMu.Unlock()
		return nil, wifiGatewayChanged()
	}
	entry := c.wifiHotspots[link.Name]
	if entry != nil && entry.obsolete(index, neighbor, c.exitTime()) {
		c.dropWiFiHotspot(link.Name, entry)
		entry = nil
	}
	if entry == nil {
		entry = &wifiHotspot{link: state.link, index: index, gatewayMAC: neighbor, done: make(chan struct{})}
		if c.wifiHotspots == nil {
			c.wifiHotspots = make(map[string]*wifiHotspot)
		}
		c.wifiHotspots[link.Name] = entry
		c.exitJobs.Add(1)
		go c.identifyWiFiHotspot(entry, gateway, neighbor)
	}
	c.exitMu.Unlock()
	c.proxyMu.Unlock()
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-entry.done:
		if entry.err != nil && !entry.recognized && api.Code(entry.err) != api.ErrInterfaceUnavailable {
			// An anonymous probe can time out on an HTTPS-only gateway.
			// Keep its retry state locally and let the router driver check it.
			return nil, &api.Error{Code: api.ErrUnsupported, Detail: "the gateway did not identify as an Inseego M2000"}
		}
		return entry.session, entry.err
	}
}

func (e *wifiHotspot) obsolete(index int, neighbor string, now time.Time) bool {
	if e.index != index {
		return true
	}
	select {
	case <-e.done:
		return neighbor != "" && neighbor != e.gatewayMAC || neighbor == "" && e.session != nil ||
			e.err != nil && api.Code(e.err) != api.ErrUnsupported && now.Sub(e.at) >= exitIdentifyAge
	default:
		return false
	}
}

// dropWiFiHotspot is called with exitMu held. A pending discovery checks
// whether it still owns this entry before publishing its result.
func (c *Client) dropWiFiHotspot(name string, entry *wifiHotspot) {
	delete(c.wifiHotspots, name)
	if entry.session != nil {
		entry.session.transport.CloseIdleConnections()
	}
}

func (c *Client) identifyWiFiHotspot(entry *wifiHotspot, gateway netip.Addr, initialMAC string) {
	defer c.exitJobs.Done()
	ctx, cancel := context.WithTimeout(c.lifetime, 3*time.Second)
	defer cancel()
	link, index := entry.link, entry.index
	base := c.hotspotBase(gateway)
	client, transport, err := c.deviceClient(link.Name, link, index, nil, nil)
	if err == nil {
		// Discovery owns this anonymous transport and cookie jar. The
		// credentialed session starts with a clean one.
		err = (&hotspot.Inseego{Base: base, Model: "M2000", HTTP: client}).Identify(ctx)
		transport.CloseIdleConnections()
	}
	neighbor := c.wifiGatewayMAC(ctx, index, gateway)
	var session *hotspotSession
	recognized := err == nil
	currentIndex, indexErr := c.hotspotIndex(link.Name)
	switched := initialMAC != "" && neighbor != "" && initialMAC != neighbor || indexErr != nil || currentIndex != index
	switch {
	case switched:
		err = wifiGatewayChanged()
	case err != nil:
	case neighbor == "":
		err = &api.Error{Code: api.ErrInterfaceUnavailable, Detail: "the Wi-Fi hotspot has no resolved gateway hardware address"}
	default:
		account := "wifi:" + strings.ToLower(link.HardwareAddr) + "/" + neighbor
		session, err = c.inseegoSession(link, index, base, "M2000", account, func(ctx context.Context) error {
			if c.wifiGatewayMAC(ctx, index, gateway) != neighbor {
				return wifiGatewayChanged()
			}
			return nil
		})
	}
	c.exitMu.Lock()
	defer c.exitMu.Unlock()
	if c.wifiHotspots[link.Name] != entry {
		if session != nil {
			session.transport.CloseIdleConnections()
		}
		session, err = nil, wifiGatewayChanged()
	} else if switched {
		c.dropWiFiHotspot(link.Name, entry)
	}
	if neighbor != "" {
		entry.gatewayMAC = neighbor
	}
	entry.session, entry.err, entry.at = session, err, c.exitTime()
	entry.recognized = recognized
	close(entry.done)
}

func wifiGatewayChanged() error {
	return &api.Error{Code: api.ErrInterfaceUnavailable, Detail: "the Wi-Fi hotspot gateway changed"}
}
