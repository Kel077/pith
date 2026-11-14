package client

import (
	"context"
	"slices"
	"time"

	"github.com/Kel077/pith/internal/api"
)

const exitFailureAge = 30 * time.Second

// exitState belongs to one observed interface configuration. Replacing it
// prevents an old lookup from publishing addresses for a different link.
type exitState struct {
	link       api.Interface
	public     api.Probe
	failed     time.Time
	lookup     *exitLookup
	capability *exitCapability
}

type exitLookup struct {
	done   chan struct{}
	result api.Probe
	err    error
}

type exitCapability struct {
	done chan struct{}
	can  bool
}

type exitDevice uint8

const (
	exitAutomatic exitDevice = iota
	exitHotspot
	exitRouter
)

type exitChange struct {
	link    api.Interface
	done    chan struct{}
	hotspot api.HotspotRenewal
	router  api.RouterRenewal
	public  api.ProxyIPChange
	err     error
}

// observeExits forgets addresses and capabilities when the interface they
// described changes, including a disappearance followed by a return.
func (c *Client) observeExits(links []api.Interface) {
	c.exitMu.Lock()
	defer c.exitMu.Unlock()
	if c.exits == nil {
		c.exits = map[string]*exitState{}
	}
	for name, old := range c.exits {
		found := false
		for _, link := range links {
			if link.Name == name {
				found = sameExitLink(old.link, link)
				break
			}
		}
		if !found {
			delete(c.exits, name)
		}
	}
	for _, link := range links {
		if c.exits[link.Name] == nil {
			link.IPv4 = slices.Clone(link.IPv4)
			link.IPv6 = slices.Clone(link.IPv6)
			c.exits[link.Name] = &exitState{link: link}
		}
	}
}

func sameExitLink(a, b api.Interface) bool {
	return a.Name == b.Name && a.HardwareAddr == b.HardwareAddr && a.Up == b.Up &&
		a.Router == b.Router && a.Router6 == b.Router6 && a.Label == b.Label && a.Kind == b.Kind &&
		slices.Equal(a.IPv4, b.IPv4) && slices.Equal(a.IPv6, b.IPv6)
}

func (c *Client) forgetExitAddresses(name string) {
	c.exitMu.Lock()
	defer c.exitMu.Unlock()
	if state := c.exits[name]; state != nil {
		state.public, state.failed, state.lookup = api.Probe{}, time.Time{}, nil
	}
}

// publicProbe shares one test of a link. A nil maxAge always asks for a
// fresh test, as the interface's Test button and renewals do.
func (c *Client) publicProbe(ctx context.Context, link api.Interface, links []api.Interface, maxAge *time.Duration) (api.Probe, error) {
	c.exitMu.Lock()
	state := c.exits[link.Name]
	if state == nil || !sameExitLink(state.link, link) {
		c.exitMu.Unlock()
		return c.probe(ctx, link.Name, "", links) // an uncached, superseded inventory
	}
	if maxAge != nil {
		known := publicIPs(state.public) > 0
		fresh := known && (*maxAge < 0 || time.Since(state.public.At) <= *maxAge)
		if fresh || *maxAge < 0 && !state.failed.IsZero() && time.Since(state.failed) < exitFailureAge {
			p := state.public
			c.exitMu.Unlock()
			return p, nil
		}
	}
	if pending := state.lookup; pending != nil {
		c.exitMu.Unlock()
		select {
		case <-ctx.Done():
			return api.Probe{}, ctx.Err()
		case <-pending.done:
			return pending.result, pending.err
		}
	}
	pending := &exitLookup{done: make(chan struct{})}
	state.lookup = pending
	c.exitMu.Unlock()

	pending.result, pending.err = c.probe(ctx, link.Name, "", links)
	// A test started before a link changed must not repopulate its cache.
	_, inventoryErr := c.Interfaces(ctx)
	c.exitMu.Lock()
	if c.exits[link.Name] == state && state.lookup == pending {
		state.lookup = nil
		if pending.err == nil && inventoryErr == nil && link.Up {
			if publicIPs(pending.result) > 0 {
				if !pending.result.At.Before(state.public.At) {
					state.public = pending.result
				}
				state.failed = time.Time{}
			} else {
				if maxAge != nil {
					state.public = api.Probe{}
				}
				state.failed = time.Now()
			}
		}
	}
	close(pending.done)
	c.exitMu.Unlock()
	return pending.result, pending.err
}

func (c *Client) exitLabel(via api.InterfaceRef, link *api.Interface) string {
	var aliases map[string]string
	if saved := c.exitAliases.Load(); saved != nil {
		aliases = *saved
	}
	if alias := aliases[via.Key()]; alias != "" {
		return alias
	}
	if link != nil {
		ref := api.InterfaceRef{Name: link.Name, HardwareAddr: link.HardwareAddr}
		if alias := aliases[ref.Key()]; alias != "" {
			return alias
		}
		if link.Label != "" {
			return link.Label
		}
		return link.Name
	}
	if via.Label != "" {
		return via.Label
	}
	return via.Name
}

// exitCanChange identifies a device once per interface configuration. A
// missing password doesn't change whether the device supports a renewal.
func (c *Client) exitCanChange(ctx context.Context, link api.Interface) bool {
	c.exitMu.Lock()
	state := c.exits[link.Name]
	if state == nil || !sameExitLink(state.link, link) {
		c.exitMu.Unlock()
		return false
	}
	if pending := state.capability; pending != nil {
		c.exitMu.Unlock()
		select {
		case <-pending.done:
			return pending.can
		default:
		}
		select {
		case <-ctx.Done():
			return false
		case <-pending.done:
			return pending.can
		}
	}
	pending := &exitCapability{done: make(chan struct{})}
	state.capability = pending
	c.exitMu.Unlock()
	_, err := c.hotspot(ctx, link.Name)
	if api.Code(err) == api.ErrUnsupported {
		_, err = c.router(ctx, link.Name)
	}
	pending.can = err == nil
	c.exitMu.Lock()
	// Cancellation isn't evidence about the device. Other failures are kept
	// until the interface changes, just as an unsupported device is.
	if ctx.Err() != nil && state.capability == pending {
		state.capability = nil
	}
	close(pending.done)
	c.exitMu.Unlock()
	return pending.can
}

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
	links, err := c.Interfaces(ctx)
	link := via.Resolve(links)
	info := api.ProxyInfo{Interface: c.exitLabel(via, link), Changing: c.exitChanging(via)}
	if err != nil {
		return info, err
	}
	info.Online = link != nil && link.Up && !c.proxySuspended(via)
	if link == nil || !link.Up {
		info.CanChangeIP = info.Changing
		return info, ctx.Err()
	}
	info.CanChangeIP = info.Changing || c.exitCanChange(ctx, *link)
	if info.Online && !info.Changing {
		_, _ = c.publicProbe(ctx, *link, links, &maxAge)
	}
	if ctx.Err() != nil {
		return info, ctx.Err()
	}
	suspended := c.proxySuspended(via)
	c.exitMu.Lock()
	current := make([]api.Interface, 0, len(c.exits))
	for _, state := range c.exits {
		current = append(current, state.link)
	}
	resolved := via.Resolve(current)
	info.Interface = c.exitLabel(via, resolved)
	info.Online = resolved != nil && resolved.Up && !suspended
	if resolved != nil {
		state := c.exits[resolved.Name]
		info.PublicIPv4, info.PublicIPv6, info.At = state.public.PublicIPv4, state.public.PublicIPv6, state.public.At
		if !sameExitLink(*link, *resolved) {
			info.CanChangeIP = info.Changing
		}
	} else {
		info.CanChangeIP = info.Changing
	}
	c.exitMu.Unlock()
	// Failure to find a public address isn't a failure to describe the port.
	return info, nil
}

// changeExitIP changes the public address of the interface a local proxy
// goes out through: proxy.Options.ChangeIP.
func (c *Client) changeExitIP(ctx context.Context, via api.InterfaceRef) (api.ProxyIPChange, error) {
	change, err := c.changeAddress(ctx, via, exitAutomatic, true)
	if change == nil {
		return api.ProxyIPChange{}, err
	}
	return change.public, err
}

// exitChanging tells whether the public address of an interface is being
// changed now: proxy.Options.Changing.
func (c *Client) exitChanging(via api.InterfaceRef) bool {
	c.exitMu.Lock()
	defer c.exitMu.Unlock()
	return c.changingExit(via) != nil
}

// changingExit resolves against the links being renewed, even while one is
// down or absent. exitMu must be held.
func (c *Client) changingExit(via api.InterfaceRef) *exitChange {
	if len(c.exitChanges) == 0 {
		return nil
	}
	// Include inactive links: a shared hardware address alone must not join
	// a different interface when the reference's exact name is still there.
	links := make([]api.Interface, 0, len(c.exits)+len(c.exitChanges))
	for _, state := range c.exits {
		links = append(links, state.link)
	}
	// A device may temporarily disappear while reconnecting. Include the
	// saved identity of its change without hiding other links with its MAC.
	for _, pending := range c.exitChanges {
		found := slices.ContainsFunc(links, func(link api.Interface) bool {
			return link.Name == pending.link.Name && link.HardwareAddr == pending.link.HardwareAddr
		})
		if !found {
			links = append(links, pending.link)
		}
	}
	if link := via.Resolve(links); link != nil {
		pending := c.exitChanges[link.Name]
		if pending != nil && link.HardwareAddr == pending.link.HardwareAddr {
			return pending
		}
		return nil
	}
	return nil
}

// changeAddress registers before identifying or contacting the device, so
// every entry point joins the same work. Only a port's request is detached
// from its caller; an interface renewal retains its caller's cancellation.
func (c *Client) changeAddress(ctx context.Context, via api.InterfaceRef, device exitDevice, detached bool) (*exitChange, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	c.exitMu.Lock()
	pending := c.changingExit(via)
	c.exitMu.Unlock()
	if pending != nil {
		return waitExitChange(ctx, pending)
	}
	links, err := c.Interfaces(ctx)
	if err != nil {
		return nil, err
	}
	link := via.Resolve(links)
	if link == nil {
		return nil, &api.Error{Code: api.ErrUnsupported, Detail: "the interface leads to no hotspot or router Pith knows"}
	}
	// Close and registration share this lock: Wait cannot race an Add.
	c.proxyMu.Lock()
	if c.closed {
		c.proxyMu.Unlock()
		return nil, errClientClosed
	}
	c.exitMu.Lock()
	pending = c.changingExit(via)
	if pending == nil && c.exitChanges[link.Name] != nil {
		c.exitMu.Unlock()
		c.proxyMu.Unlock()
		return nil, &api.Error{Code: api.ErrInterfaceUnavailable, Detail: "the interface changed during an address renewal"}
	}
	if pending == nil {
		pending = &exitChange{link: *link, done: make(chan struct{})}
		if c.exitChanges == nil {
			c.exitChanges = map[string]*exitChange{}
		}
		c.exitChanges[link.Name] = pending
		c.renewals.Add(1)
		c.exitMu.Unlock()
		c.proxyMu.Unlock()
		if detached {
			go c.runExitChange(c.lifetime, pending, device, true)
		} else {
			c.runExitChange(ctx, pending, device, false)
		}
	} else {
		c.exitMu.Unlock()
		c.proxyMu.Unlock()
	}
	return waitExitChange(ctx, pending)
}

func waitExitChange(ctx context.Context, pending *exitChange) (*exitChange, error) {
	select {
	case <-pending.done:
		return pending, pending.err
	default:
	}
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-pending.done:
		return pending, pending.err
	}
}

func (c *Client) runExitChange(parent context.Context, pending *exitChange, device exitDevice, detached bool) {
	defer c.renewals.Done()
	started := time.Now()
	ctx, cancel := context.WithCancel(parent)
	defer cancel()
	defer context.AfterFunc(c.lifetime, cancel)()
	if detached {
		var bound context.CancelFunc
		ctx, bound = context.WithTimeout(ctx, 8*time.Minute)
		defer bound()
	}
	iface := pending.link.Name
	if device == exitAutomatic {
		_, pending.err = c.hotspot(ctx, iface)
		if pending.err == nil {
			device = exitHotspot
		} else if api.Code(pending.err) == api.ErrUnsupported {
			device, pending.err = exitRouter, nil
		}
	}
	var before, after api.Probe
	if pending.err == nil {
		if device == exitHotspot {
			if detached {
				var bound context.CancelFunc
				ctx, bound = context.WithTimeout(ctx, 3*time.Minute)
				defer bound()
			}
			pending.hotspot, pending.err = c.renewHotspot(ctx, iface)
			before, after = pending.hotspot.Before, pending.hotspot.After
			pending.router = api.RouterRenewal{Before: before, After: after, Took: time.Since(started)}
		} else {
			pending.router, pending.err = c.renewRouter(ctx, iface)
			before, after = pending.router.Before, pending.router.After
			pending.hotspot = api.HotspotRenewal{Before: before, After: after, Took: pending.router.Took}
		}
	}
	pending.public = api.ProxyIPChange{BeforeIPv4: before.PublicIPv4, BeforeIPv6: before.PublicIPv6,
		PublicIPv4: after.PublicIPv4, PublicIPv6: after.PublicIPv6, At: after.At, Seconds: time.Since(started).Seconds()}
	c.exitMu.Lock()
	delete(c.exitChanges, iface)
	close(pending.done)
	c.exitMu.Unlock()
}
