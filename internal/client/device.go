package client

import (
	"context"
	"crypto/tls"
	"errors"
	"net"
	"net/http"
	"net/http/cookiejar"
	"time"

	"github.com/Kel077/pith/internal/api"
	"github.com/Kel077/pith/internal/netif"
)

// deviceClient is an HTTP client for a device on an interface's own network,
// a hotspot or a router: it goes straight to the device through that
// interface, with no proxy and no redirect elsewhere, and only while the
// interface keeps its name, hardware address, router and kernel index. Every
// new socket binds the interface's current index, and pooled ones are checked
// again before each request, so that no session cookie or form reaches a
// device that replaced it.
func (c *Client) deviceClient(name string, link api.Interface, index int, tlsConfig *tls.Config) (*http.Client, *http.Transport, error) {
	jar, err := cookiejar.New(nil)
	if err != nil {
		return nil, nil, err
	}
	checkIdentity := func(ctx context.Context) error {
		current, err := c.Interfaces(ctx)
		if err != nil {
			return err
		}
		found := (api.InterfaceRef{Name: name, HardwareAddr: link.HardwareAddr}).Resolve(current)
		if found == nil || found.Name != name || !found.Up || found.Router != link.Router {
			return errors.New("device interface identity changed")
		}
		currentIndex, err := c.hotspotIndex(name)
		if err != nil || currentIndex != index {
			return errors.New("device interface index changed")
		}
		return nil
	}
	transport := &http.Transport{Proxy: nil, MaxIdleConns: 2, IdleConnTimeout: 30 * time.Second, TLSClientConfig: tlsConfig, DialContext: func(ctx context.Context, network, target string) (net.Conn, error) {
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
	client := &http.Client{
		Jar: jar, Timeout: 10 * time.Second,
		Transport: deviceTransport{transport: transport, check: checkIdentity},
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			if req.URL.Host != via[0].URL.Host || req.URL.Scheme != via[0].URL.Scheme || req.URL.User != nil || len(via) > 3 {
				return http.ErrUseLastResponse
			}
			return nil
		},
	}
	return client, transport, nil
}

type deviceTransport struct {
	transport *http.Transport
	check     func(context.Context) error
}

func (t deviceTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	// Also validate pooled connections before sending session cookies or forms.
	if err := t.check(req.Context()); err != nil {
		return nil, err
	}
	return t.transport.RoundTrip(req)
}
