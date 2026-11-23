package client

import (
	"context"
	"errors"
	"net"
	"net/http"
	"net/netip"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/Kel077/pith/internal/api"
	"github.com/Kel077/pith/internal/hotspot"
	"github.com/Kel077/pith/internal/keychain"
	"github.com/Kel077/pith/internal/netif"
)

// hotspotKeychain is the keychain service of the hotspots' admin passwords;
// the account is the hardware address of the interface a hotspot serves.
const hotspotKeychain = "io.github.kel077.pith.hotspot"

type hotspotController interface {
	Status(context.Context) (api.Hotspot, error)
	Renew(context.Context, func(context.Context) error) error
}

// A web session belongs to one name, MAC, gateway and index. RMNet has a
// single local controller and can follow the modem across interface changes.
type hotspotSession struct {
	key       string
	device    *hotspot.Inseego
	account   string
	transport *http.Transport
	rmnet     hotspotController
	ref       api.InterfaceRef
}

func (s *hotspotSession) controller() hotspotController {
	if s.rmnet != nil {
		return s.rmnet
	}
	return s.device
}

func (c *Client) Hotspot(ctx context.Context, iface string) (api.Hotspot, error) {
	s, err := c.hotspot(ctx, iface)
	if err != nil {
		return api.Hotspot{Interface: iface}, err
	}
	h, err := s.controller().Status(ctx)
	h.Interface = iface
	return h, err
}

func (c *Client) SetHotspotPassword(ctx context.Context, iface, password string) error {
	s, err := c.hotspot(ctx, iface)
	if err != nil {
		return err
	}
	if s.rmnet != nil {
		return &api.Error{Code: api.ErrUnsupported, Detail: "RMNet does not use a hotspot admin password"}
	}
	if err := s.device.TryPassword(ctx, password); err != nil {
		return err
	}
	return c.secretSet(ctx, hotspotKeychain, s.account, password)
}

func (c *Client) RenewHotspot(ctx context.Context, iface string) (api.HotspotRenewal, error) {
	change, err := c.changeAddress(ctx, api.InterfaceRef{Name: iface}, exitHotspot, false)
	if change == nil {
		return api.HotspotRenewal{}, err
	}
	return change.hotspot, err
}

const (
	retestAttemptTimeout = 2 * time.Second
	retestFastPace       = 400 * time.Millisecond
	readyInitialDelay    = 3 * time.Second
	readyPollInterval    = 750 * time.Millisecond
)

func (c *Client) renewHotspot(ctx context.Context, via api.InterfaceRef) (api.HotspotRenewal, error) {
	var r api.HotspotRenewal
	s, err := c.hotspotFor(ctx, via)
	if err != nil {
		return r, err
	}
	started := time.Now()
	if s.rmnet != nil {
		probe := func(ctx context.Context) (api.Probe, error) { return c.rmnetProbe(ctx, s.ref) }
		err = s.rmnet.Renew(ctx, func(ctx context.Context) (err error) {
			r.Before, err = probe(ctx)
			if err == nil && !r.Before.OK {
				return &api.Error{Code: api.ErrInterfaceUnavailable, Detail: "RMNet public-address test before renewal: " + r.Before.Problem}
			}
			if err == nil {
				c.forgetExitAddresses(r.Before.Interface)
			}
			return err
		})
		if err != nil {
			return r, err
		}
		r.After, err = c.retestPublic(ctx, r.Before, probe)
		// A registered modem may not have a usable IP session yet.
		r.Took = time.Since(started)
		return r, err
	}

	dnsCache := &probeDNSCache{}
	probeM2000 := func(pctx context.Context, opts probeOptions) (api.Probe, error) {
		links, err := c.Interfaces(pctx)
		if err != nil {
			return api.Probe{Interface: via.Name, At: c.exitTime()}, err
		}
		return c.probeWith(pctx, via.Name, "", links, opts)
	}
	var resume func()
	defer func() {
		if resume != nil {
			resume()
		}
	}()
	var (
		earlyMu    sync.Mutex
		earlyAfter api.Probe
	)
	// Its public IP before, tested while the hotspot prepares its session.
	before := func(bctx context.Context) (err error) {
		r.Before, err = probeM2000(bctx, probeOptions{timeout: min(c.probeLimit(), 2500*time.Millisecond), dnsCache: dnsCache})
		if err == nil {
			c.forgetExitAddresses(r.Before.Interface)
			resume = c.suspendHotspotProxies(via)
		}
		return err
	}
	ready := func(wctx context.Context) bool {
		select {
		case <-wctx.Done():
			return false
		case <-time.After(readyInitialDelay):
		}
		for attempt := 0; ; attempt++ {
			earlyMu.Lock()
			skipV4, skipV6 := earlyAfter.PublicIPv4 != "", earlyAfter.PublicIPv6 != ""
			earlyMu.Unlock()
			p, err := probeM2000(wctx, probeOptions{
				timeout:        min(c.probeLimit(), 1500*time.Millisecond),
				skipV4:         skipV4,
				skipV6:         skipV6,
				preferFallback: true,
				dnsCache:       dnsCache,
				cachedOnly:     attempt < 3,
			})
			if err == nil && p.OK {
				earlyMu.Lock()
				earlyAfter = mergeProbe(earlyAfter, p)
				done := coversBefore(r.Before, earlyAfter) && changedFrom(r.Before, earlyAfter)
				earlyMu.Unlock()
				if done {
					return true
				}
			}
			select {
			case <-wctx.Done():
				return false
			case <-time.After(readyPollInterval):
			}
		}
	}
	if err = s.device.RenewReady(ctx, before, ready); err != nil {
		return r, err
	}
	r.Took = time.Since(started)
	earlyMu.Lock()
	seed := earlyAfter
	earlyMu.Unlock()
	// Connected again, a hotspot may still be setting up its addresses and
	// its DNS: the M2000's refuses queries for some ten seconds more, and in
	// 5G SA its 464XLAT IPv4 finishes several seconds after IPv6.
	if publicIPs(seed) > 0 && coversBefore(r.Before, seed) {
		r.After = seed
	} else {
		r.After, err = c.retestPublicSeeded(ctx, r.Before, seed, func(pctx context.Context, skipV4, skipV6 bool, attempt int, timeout time.Duration) (api.Probe, error) {
			return probeM2000(pctx, probeOptions{
				timeout:        timeout,
				skipV4:         skipV4,
				skipV6:         skipV6,
				preferFallback: true,
				dnsCache:       dnsCache,
				cachedOnly:     attempt < 4,
			})
		})
	}
	if err == nil {
		c.publishExitProbe(via, r.After)
	}
	if resume != nil {
		resume()
		resume = nil
	}
	return r, err
}

func (c *Client) suspendHotspotProxies(via api.InterfaceRef) func() {
	c.proxyMu.Lock()
	manager := c.proxies
	c.proxyMu.Unlock()
	if manager == nil {
		return func() {}
	}
	links, _ := c.Interfaces(c.lifetime)
	target := via.Resolve(links)
	match := func(ref api.InterfaceRef) bool {
		if target != nil && ref.Resolve([]api.Interface{*target}) != nil {
			return true
		}
		return via.Name != "" && ref.Name == via.Name && (via.HardwareAddr == "" || ref.HardwareAddr == "" || ref.HardwareAddr == via.HardwareAddr)
	}
	manager.Suspend(match)
	var once sync.Once
	return func() { once.Do(manager.Resume) }
}

func (c *Client) rmnetProbe(ctx context.Context, ref api.InterfaceRef) (api.Probe, error) {
	links, err := c.Interfaces(ctx)
	if err != nil {
		return api.Probe{}, err
	}
	link := ref.Resolve(links)
	unavailable := api.Probe{Interface: ref.Name, At: c.exitTime(), Problem: "waiting for the RMNet interface"}
	if link == nil || !link.Up || !strings.EqualFold(link.HardwarePort, api.HotspotRM520N) {
		return unavailable, nil
	}
	name := link.Name
	index, err := c.hotspotIndex(name)
	if err != nil {
		return unavailable, nil
	}
	check := func() error {
		current, err := c.hotspotIndex(name)
		if err != nil || current != index {
			return errors.New("RMNet interface changed during its public-address test")
		}
		return nil
	}
	p, err := c.publicProbe(netif.WithBinding(ctx, index, check), *link, links, nil)
	if err == nil && check() != nil {
		return unavailable, nil
	}
	return p, err
}

// retest tests the internet through an interface that just got new public
// addresses, every pace until it finds as many as before did, for 25 paces at
// most, and 6 more once a family works: one that doesn't come back with the
// other may not come back at all. It returns the test that found the most.
func (c *Client) retest(ctx context.Context, iface string, before api.Probe) (api.Probe, error) {
	return c.retestPublic(ctx, before, func(ctx context.Context) (api.Probe, error) { return c.Probe(ctx, iface, "") })
}

func (c *Client) retestPublic(ctx context.Context, before api.Probe, probe func(context.Context) (api.Probe, error)) (api.Probe, error) {
	return c.retestPublicSeeded(ctx, before, api.Probe{}, func(pctx context.Context, _, _ bool, _ int, _ time.Duration) (api.Probe, error) {
		return probe(pctx)
	})
}

func (c *Client) retestPublicSeeded(ctx context.Context, before, seed api.Probe, probe func(ctx context.Context, skipV4, skipV6 bool, attempt int, timeout time.Duration) (api.Probe, error)) (api.Probe, error) {
	pace := c.hotspotPace
	wait := pace
	if pace == 0 {
		pace = time.Second
		wait = retestFastPace
	}
	after := seed
	deadline := time.Now().Add(25 * pace)
	if late := time.Now().Add(6 * pace); after.OK && late.Before(deadline) {
		deadline = late
	}
	for attempt := 0; ; attempt++ {
		timeout := min(c.probeLimit(), retestAttemptTimeout)
		pctx := ctx
		var cancel context.CancelFunc
		if after.OK {
			rem := time.Until(deadline)
			if rem <= 0 {
				return after, nil
			}
			if rem < timeout {
				timeout = rem
			}
			pctx, cancel = context.WithDeadline(ctx, deadline)
		}
		p, err := probe(pctx, after.PublicIPv4 != "", after.PublicIPv6 != "", attempt, timeout)
		if cancel != nil {
			cancel()
		}
		if err != nil {
			if ctx.Err() == nil && after.OK {
				return after, nil
			}
			return after, err
		}
		after = mergeProbe(after, p)
		if publicIPs(after) > 0 && coversBefore(before, after) {
			return after, nil
		}
		if late := time.Now().Add(6 * pace); after.OK && late.Before(deadline) {
			deadline = late
		}
		if after.OK && after.PublicIPv6 == "" && c.interfaces == nil && !hasUsableIPv6(ctx, after.Interface) {
			if noV6 := time.Now().Add(2 * pace); noV6.Before(deadline) {
				deadline = noV6
			}
		}
		if time.Now().Add(wait).After(deadline) {
			return after, nil
		}
		select {
		case <-ctx.Done():
			return after, ctx.Err()
		case <-time.After(wait):
		}
	}
}

func coversBefore(before, after api.Probe) bool {
	return (before.PublicIPv4 == "" || after.PublicIPv4 != "") && (before.PublicIPv6 == "" || after.PublicIPv6 != "")
}

func changedFrom(before, after api.Probe) bool {
	return (after.PublicIPv4 != "" && after.PublicIPv4 != before.PublicIPv4) ||
		(after.PublicIPv6 != "" && after.PublicIPv6 != before.PublicIPv6)
}

func mergeProbe(dst, src api.Probe) api.Probe {
	if !src.OK {
		if !dst.OK {
			return src
		}
		return dst
	}
	if !dst.OK {
		return src
	}
	out := dst
	out.OK = true
	out.Problem = ""
	out.At = src.At
	out.Latency = src.Latency
	if src.Address != "" && out.Address == "" {
		out.Address = src.Address
	}
	if src.PublicIPv4 != "" {
		out.PublicIPv4 = src.PublicIPv4
	}
	if src.PublicIPv6 != "" {
		out.PublicIPv6 = src.PublicIPv6
	}
	return out
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
	return c.hotspotFor(ctx, api.InterfaceRef{Name: name})
}

func (c *Client) hotspotFor(ctx context.Context, ref api.InterfaceRef) (*hotspotSession, error) {
	ifaces, err := c.Interfaces(ctx)
	if err != nil {
		return nil, err
	}
	link := ref.Resolve(ifaces)
	unsupported := &api.Error{Code: api.ErrUnsupported, Detail: ref.Name + " leads to no hotspot Pith knows"}
	if link == nil || link.HardwareAddr == "" {
		return nil, unsupported
	}
	name := link.Name
	if strings.EqualFold(link.HardwarePort, api.HotspotRM520N) {
		return &hotspotSession{rmnet: c.rmnet, ref: api.InterfaceRef{Name: name, HardwareAddr: link.HardwareAddr}}, nil
	}
	model := hotspot.Model(link.Label)
	router, err := netip.ParseAddr(link.Router)
	if !link.Up || model == "" || err != nil {
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
