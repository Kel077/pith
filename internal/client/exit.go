package client

import (
	"context"
	"slices"
	"time"

	"github.com/Kel077/pith/internal/api"
)

const (
	exitFailureAge  = 30 * time.Second
	exitFailureMax  = 10 * time.Minute
	exitIdentifyAge = time.Minute
)

// exitState belongs to one observed interface configuration. Replacing it
// prevents an old lookup from publishing addresses for a different link.
type exitState struct {
	link       api.Interface
	public     api.Probe
	failed     time.Time
	backoff    time.Duration
	lookup     *exitLookup
	capability *exitCapability
}

type exitLookup struct {
	done          chan struct{}
	result        api.Probe
	err           error
	forPoll       bool
	ownerCanceled bool
	owned         bool
}

type exitCapability struct {
	done chan struct{}
	can  bool
	err  error
	at   time.Time
}

func (c *Client) exitTime() time.Time {
	if c.exitNow != nil {
		return c.exitNow()
	}
	return time.Now()
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
		state.backoff = 0
	}
}

// publicProbe shares one test of a link. A nil maxAge always asks for a
// fresh test, as the interface's Test button and renewals do.
func (c *Client) publicProbe(ctx context.Context, link api.Interface, links []api.Interface, maxAge *time.Duration) (api.Probe, error) {
	for {
		if err := ctx.Err(); err != nil {
			return api.Probe{}, err
		}
		c.proxyMu.Lock()
		if c.closed {
			c.proxyMu.Unlock()
			return api.Probe{}, errClientClosed
		}
		c.exitMu.Lock()
		state := c.exits[link.Name]
		if state == nil || !sameExitLink(state.link, link) {
			c.exitMu.Unlock()
			c.proxyMu.Unlock()
			if maxAge == nil {
				// Another reader observed a different snapshot. Test the
				// supplied interface without publishing into its new record.
				return c.probe(ctx, link.Name, "", links)
			}
			return api.Probe{}, &api.Error{Code: api.ErrInterfaceUnavailable, Detail: "the interface changed before its public-address test"}
		}
		if maxAge != nil {
			known := publicIPs(state.public) > 0
			fresh := known && (*maxAge < 0 || c.exitTime().Sub(state.public.At) <= *maxAge)
			if fresh || *maxAge < 0 && !state.failed.IsZero() && c.exitTime().Sub(state.failed) < state.backoff {
				p := state.public
				c.exitMu.Unlock()
				c.proxyMu.Unlock()
				return p, nil
			}
		}
		pending := state.lookup
		if pending == nil {
			pending = &exitLookup{done: make(chan struct{}), owned: maxAge == nil}
			state.lookup = pending
			c.exitJobs.Add(1)
			parent := ctx
			if maxAge != nil {
				parent = c.lifetime
			}
			go c.runExitLookup(parent, state, pending, links)
		}
		if maxAge != nil {
			pending.forPoll = true
		}
		c.exitMu.Unlock()
		c.proxyMu.Unlock()
		select {
		case <-ctx.Done():
			return api.Probe{}, ctx.Err()
		case <-pending.done:
		}
		if !pending.ownerCanceled || ctx.Err() != nil {
			return pending.result, pending.err
		}
		// A reader doesn't inherit cancellation from a test it joined. Read
		// the inventory again before starting a test under its own context.
		var err error
		links, err = c.Interfaces(ctx)
		if err != nil {
			return api.Probe{}, err
		}
		resolved := (api.InterfaceRef{Name: link.Name, HardwareAddr: link.HardwareAddr}).Resolve(links)
		if resolved == nil || !resolved.Up {
			return api.Probe{}, &api.Error{Code: api.ErrInterfaceUnavailable, Detail: "the interface left during its public-address test"}
		}
		link = *resolved
	}
}

func (c *Client) runExitLookup(parent context.Context, state *exitState, pending *exitLookup, links []api.Interface) {
	defer c.exitJobs.Done()
	ctx, cancel := context.WithCancel(parent)
	defer cancel()
	defer context.AfterFunc(c.lifetime, cancel)()
	pending.result, pending.err = c.probe(ctx, state.link.Name, "", links)
	// Use the client's context to check identity even when the test timed out.
	_, inventoryErr := c.Interfaces(c.lifetime)
	c.exitMu.Lock()
	defer c.exitMu.Unlock()
	pending.ownerCanceled = pending.owned && parent.Err() != nil
	if c.exits[state.link.Name] == state && state.lookup == pending {
		state.lookup = nil
		if c.lifetime.Err() == nil && !pending.ownerCanceled && state.link.Up {
			if pending.err == nil && inventoryErr == nil && publicIPs(pending.result) > 0 {
				state.public = pending.result
				state.failed, state.backoff = time.Time{}, 0
			} else {
				if pending.forPoll {
					state.public = api.Probe{}
				}
				state.failed = c.exitTime()
				state.backoff = min(exitFailureMax, max(exitFailureAge, 2*state.backoff))
			}
		}
	}
	close(pending.done)
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

// exitCanChange shares identification independent of its readers. Only an
// unsupported device is permanently negative for this interface configuration.
func (c *Client) exitCanChange(ctx context.Context, link api.Interface) bool {
	if ctx.Err() != nil {
		return false
	}
	c.proxyMu.Lock()
	if c.closed {
		c.proxyMu.Unlock()
		return false
	}
	c.exitMu.Lock()
	state := c.exits[link.Name]
	if state == nil || !sameExitLink(state.link, link) {
		c.exitMu.Unlock()
		c.proxyMu.Unlock()
		return false
	}
	pending := state.capability
	if pending != nil {
		select {
		case <-pending.done:
			if pending.err != nil && api.Code(pending.err) != api.ErrUnsupported && c.exitTime().Sub(pending.at) >= exitIdentifyAge {
				pending = nil
			}
		default:
		}
	}
	if pending == nil {
		pending = &exitCapability{done: make(chan struct{})}
		state.capability = pending
		c.exitJobs.Add(1)
		go c.runExitIdentification(state, pending)
	}
	c.exitMu.Unlock()
	c.proxyMu.Unlock()
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

func (c *Client) runExitIdentification(state *exitState, pending *exitCapability) {
	defer c.exitJobs.Done()
	ctx, cancel := context.WithTimeout(c.lifetime, 5*time.Second)
	defer cancel()
	_, err := c.hotspot(ctx, state.link.Name)
	if api.Code(err) == api.ErrUnsupported {
		_, err = c.router(ctx, state.link.Name)
	}
	c.exitMu.Lock()
	pending.can, pending.err, pending.at = err == nil, err, c.exitTime()
	close(pending.done)
	c.exitMu.Unlock()
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
	if info.Changing {
		info.CanChangeIP = true
	} else {
		// Start identification and the public-address test together, so the
		// first GET waits for the slower one rather than both bounds in turn.
		identified := make(chan bool, 1)
		go func() { identified <- c.exitCanChange(ctx, *link) }()
		if info.Online {
			_, _ = c.publicProbe(ctx, *link, links, &maxAge)
		}
		info.CanChangeIP = <-identified
	}
	// Even a canceled reader gets the state known so far. A port can still
	// describe an offline interface when its work deadline has passed.
	info.Changing = c.exitChanging(via)
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
	return info, ctx.Err()
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
	if pending == nil && len(c.exitChanges) > 0 {
		// A router's renewal needs another interface to carry the Mac. Its
		// address must stay put until the first interface works again.
		c.exitMu.Unlock()
		c.proxyMu.Unlock()
		return nil, &api.Error{Code: api.ErrBusy, Detail: "another interface is changing its address"}
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
