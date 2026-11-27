package client

import (
	"context"
	"crypto/sha256"
	"crypto/tls"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/netip"
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"time"

	"github.com/Kel077/pith/internal/api"
	"github.com/Kel077/pith/internal/keychain"
	"github.com/Kel077/pith/internal/router"
)

// routerKeychain is the keychain service of the routers' admin passwords. The
// account is "sha256:" and the hex SHA-256 of the public key of the router's
// web admin, so that the password goes to that router alone.
const routerKeychain = "io.github.kel077.pith.router"

// errRouterReplaced: the router's web admin has another public key than the
// one Pith keeps its password for.
var errRouterReplaced = errors.New("router: its web admin has another key")

// A router's session lasts while its interface keeps its name, hardware
// address, router and kernel index, and its web admin its public key.
type routerSession struct {
	key       string
	device    *router.Verizon // nil: a router Pith doesn't know
	account   string
	transport *http.Transport

	mu   sync.Mutex
	pin  string // the key connections must have; none while identifying
	seen string // the key the last connection had
}

// verify pins the router's public key: routers sign their web admin
// themselves, so there is no certificate to check.
func (s *routerSession) verify(cs tls.ConnectionState) error {
	if len(cs.PeerCertificates) == 0 {
		return errors.New("router: no certificate")
	}
	sum := sha256.Sum256(cs.PeerCertificates[0].RawSubjectPublicKeyInfo)
	seen := "sha256:" + hex.EncodeToString(sum[:])
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.pin != "" && seen != s.pin {
		return errRouterReplaced
	}
	s.seen = seen
	return nil
}

func (c *Client) Router(ctx context.Context, iface string) (api.Router, error) {
	if upstream, err := c.openWrtFor(ctx, api.InterfaceRef{Name: iface}); err != nil {
		return api.Router{Interface: iface}, err
	} else if upstream != nil {
		return upstream.ssh.Status(ctx)
	}
	for range 2 {
		s, err := c.router(ctx, iface)
		if err != nil {
			return api.Router{Interface: iface}, err
		}
		r, err := s.device.Status(ctx)
		r.Interface = iface
		if !errors.Is(err, errRouterReplaced) {
			return r, err
		}
		// Another router, or a new key: identify it again, which locks it.
		c.dropRouter(s)
	}
	return api.Router{Interface: iface}, errRouterReplaced
}

func (c *Client) SetRouterPassword(ctx context.Context, iface, password string) error {
	if upstream, err := c.openWrtFor(ctx, api.InterfaceRef{Name: iface}); err != nil {
		return err
	} else if upstream != nil {
		return &api.Error{Code: api.ErrUnsupported, Detail: "OpenWrt uses SSH authentication"}
	}
	s, err := c.router(ctx, iface)
	if err != nil {
		return err
	}
	if err := s.device.TryPassword(ctx, password); err != nil {
		if errors.Is(err, errRouterReplaced) {
			c.dropRouter(s)
		}
		return err
	}
	return c.secretSet(ctx, routerKeychain, s.account, password)
}

// router finds the router behind an interface, or ErrUnsupported.
func (c *Client) router(ctx context.Context, name string) (*routerSession, error) {
	ifaces, err := c.Interfaces(ctx)
	if err != nil {
		return nil, err
	}
	var link *api.Interface
	for i := range ifaces {
		if ifaces[i].Name == name {
			link = &ifaces[i]
		}
	}
	unsupported := &api.Error{Code: api.ErrUnsupported, Detail: name + " leads to no router Pith knows"}
	if link == nil || !link.Up || link.HardwareAddr == "" || link.Kind != api.KindEthernet && link.Kind != api.KindWiFi {
		return nil, unsupported
	}
	gateway, err := netip.ParseAddr(link.Router)
	if err != nil || !gateway.Is4() || !gateway.IsPrivate() && !gateway.IsLoopback() {
		return nil, unsupported
	}
	index, err := c.hotspotIndex(name)
	if err != nil || index <= 0 {
		return nil, unsupported
	}
	key := name + "|" + link.HardwareAddr + "|" + link.Router + "|" + strconv.Itoa(index)
	c.mu.Lock()
	s := c.routers[key]
	c.mu.Unlock()
	if s != nil {
		if s.device == nil {
			return nil, unsupported
		}
		return s, nil
	}
	port := c.routerPort
	if port == "" {
		port = "443"
	}
	base := "https://" + net.JoinHostPort(gateway.String(), port)
	s = &routerSession{key: key}
	client, transport, err := c.deviceClient(name, *link, index, &tls.Config{InsecureSkipVerify: true, MinVersion: tls.VersionTLS12, VerifyConnection: s.verify})
	if err != nil {
		return nil, err
	}
	s.transport = transport
	identifyCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	model, err := router.Identify(identifyCtx, client, base)
	cancel()
	if err != nil {
		transport.CloseIdleConnections()
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		var dial *net.OpError
		if api.Code(err) != api.ErrUnsupported && !errors.As(err, &dial) && !errors.As(err, new(tls.AlertError)) && !errors.As(err, new(tls.RecordHeaderError)) {
			return nil, err // a router that may answer later
		}
		c.keepRouter(name, s) // a router Pith doesn't know: no need to ask again
		return nil, unsupported
	}
	s.mu.Lock()
	s.pin, s.account = s.seen, s.seen
	s.mu.Unlock()
	account := s.account
	s.device = &router.Verizon{Base: base, Model: model, HTTP: client,
		Password: func() (string, error) {
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			secret, err := c.secretGet(ctx, routerKeychain, account)
			if errors.Is(err, keychain.ErrNotFound) {
				return "", nil
			}
			return secret, err
		}}
	c.keepRouter(name, s)
	return s, nil
}

// keepRouter keeps the interface's session, closing the one it had before.
func (c *Client) keepRouter(name string, s *routerSession) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.routers == nil {
		c.routers = map[string]*routerSession{}
	}
	for k, old := range c.routers {
		if old.key != s.key && len(k) > len(name) && k[:len(name)+1] == name+"|" {
			old.transport.CloseIdleConnections()
			delete(c.routers, k)
		}
	}
	c.routers[s.key] = s
}

func (c *Client) dropRouter(s *routerSession) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.routers[s.key] == s {
		delete(c.routers, s.key)
		s.transport.CloseIdleConnections()
	}
}

// routerPauses are how long the router stays without an address on each
// try, until it gets another: a provider may give the one it had back to a
// router that asks again at once.
var routerPauses = []time.Duration{5 * time.Second, 20 * time.Second, 60 * time.Second}

func (c *Client) RenewRouter(ctx context.Context, iface string) (api.RouterRenewal, error) {
	change, err := c.changeAddress(ctx, api.InterfaceRef{Name: iface}, exitRouter, false)
	if change == nil {
		return api.RouterRenewal{}, err
	}
	return change.router, err
}

func (c *Client) renewRouter(ctx context.Context, via api.InterfaceRef) (r api.RouterRenewal, err error) {
	if upstream, err := c.openWrtFor(ctx, via); err != nil {
		return r, err
	} else if upstream != nil {
		return c.renewOpenWrt(ctx, upstream)
	}
	iface := via.Name
	s, err := c.router(ctx, iface)
	if err != nil {
		return r, err
	}
	if !c.renewing.TryLock() {
		return r, &api.Error{Code: api.ErrBusy, Detail: "another router is renewing its address"}
	}
	defer c.renewing.Unlock()
	started := time.Now()
	ifaces, err := c.Interfaces(ctx)
	if err != nil {
		return r, err
	}
	var link api.Interface
	var standbys []string
	for _, i := range ifaces {
		switch {
		case i.Name == iface:
			link = i
		case i.Up && i.Router != "" && i.CanProxy():
			standbys = append(standbys, i.Name)
		}
	}
	// Before anything changes: the interface's public address, which other
	// interfaces have internet, and that Pith can log in to the router.
	var wg sync.WaitGroup
	var wan router.WAN
	var wanErr error
	working := make([]bool, len(standbys))
	wg.Go(func() { r.Before, _ = c.Probe(ctx, iface, "") })
	wg.Go(func() { wan, wanErr = s.device.WAN(ctx) })
	for i, name := range standbys {
		wg.Go(func() {
			p, err := c.Probe(ctx, name, "")
			working[i] = err == nil && p.OK
		})
	}
	wg.Wait()
	switch {
	case wanErr != nil:
		return r, wanErr
	case ctx.Err() != nil:
		return r, ctx.Err()
	case !wan.Connected:
		return r, errors.New("router: not connected to the internet")
	}
	usable := map[string]bool{}
	for i, name := range standbys {
		if working[i] {
			usable[name] = true
		}
	}
	if len(usable) == 0 {
		return r, &api.Error{Code: api.ErrNoStandby, Detail: "no other connected interface has internet"}
	}
	// macOS makes another interface primary; then rules skip this one and its
	// local proxies refuse connections, until it works again.
	primary, ended, release, err := c.yield(ctx, iface)
	if err != nil {
		return r, err
	}
	defer release()
	r.Standby = primary
	if !usable[primary] {
		return r, &api.Error{Code: api.ErrNoStandby, Detail: primary + " is primary now but has no internet"}
	}
	var standby api.InterfaceRef
	for _, i := range ifaces {
		if i.Name == primary {
			standby = api.InterfaceRef{Name: i.Name, Label: i.Label, HardwareAddr: i.HardwareAddr}
		}
	}
	undrain, err := c.drain(ctx, link, standby)
	if err != nil {
		return r, err
	}
	defer undrain()
	// Pith quitting ends the renewal too, once the router is asked for an
	// address again.
	renewCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	defer context.AfterFunc(c.lifetime, cancel)()
	go func() {
		select {
		case <-ended:
			cancel()
		case <-renewCtx.Done():
		}
	}()
	// Apps that watch the network move to the new primary meanwhile.
	settle := c.routerSettle
	if settle == 0 {
		settle = 2 * time.Second
	}
	select {
	case <-renewCtx.Done():
		return r, c.renewalErr(ctx, ended)
	case <-time.After(settle):
	}
	pauses := c.routerPauses
	if pauses == nil {
		pauses = routerPauses
	}
	// Should Pith end before it asks for an address again, it does when it
	// starts again.
	// Without the mark, nothing would: the router stays without an address
	// until it is asked, and so does everything behind it.
	mark := filepath.Join(c.opts.ConfigDir, routerMark)
	if err := writeMark(c.opts.ConfigDir, mark, releasedRouter{Interface: iface, HardwareAddr: link.HardwareAddr}); err != nil {
		return r, fmt.Errorf("router: can't mark the renewal for Pith to finish should it quit: %w", err)
	}
	c.forgetExitAddresses(iface)
	for _, pause := range pauses {
		r.Tries++
		var w router.WAN
		if w, err = s.device.Renew(renewCtx, pause); err != nil || w.IPv4 != wan.IPv4 {
			break
		}
	}
	if err != nil && renewCtx.Err() == nil {
		// Slow to connect again: wait for it while the other interface carries
		// the Mac.
		if _, ok := s.device.Reconnected(renewCtx, 2*time.Minute); ok {
			err = nil
		}
	}
	if err == nil {
		_ = os.Remove(mark)
	}
	if renewCtx.Err() != nil {
		return r, errors.Join(err, c.renewalErr(ctx, ended))
	}
	if err != nil {
		return r, err
	}
	r.After, err = c.retest(renewCtx, iface, r.Before)
	r.Took = time.Since(started)
	if renewCtx.Err() != nil {
		return r, c.renewalErr(ctx, ended)
	}
	return r, err
}

// renewalErr tells why a renewal stopped early.
func (c *Client) renewalErr(ctx context.Context, ended <-chan struct{}) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	select {
	case <-ended:
		return errors.New("the helper ended the yield of macOS's primary early")
	default:
		return context.Canceled
	}
}

// drainFile marks a drain in progress, so that a Pith that quit during one
// gives the helper the saved rules back when it starts again.
const drainFile = ".draining"

// routerMark marks a router that may have given its address up without
// being asked for one again: Pith asks it when it starts again.
const routerMark = ".router-released"

type releasedRouter struct {
	Interface    string `json:"interface"`
	HardwareAddr string `json:"hardwareAddr"`
}

// recoverRouter asks a router that a renewal cut short left without an
// address for one again, off the caller's thread, once.
func (c *Client) recoverRouter() {
	c.recovery.Do(func() {
		mark := filepath.Join(c.opts.ConfigDir, routerMark)
		data, err := os.ReadFile(mark)
		if err != nil {
			return
		}
		var m releasedRouter
		if json.Unmarshal(data, &m) != nil || m.Interface == "" {
			_ = os.Remove(mark)
			return
		}
		go func() {
			ctx, cancel := context.WithTimeout(c.lifetime, 3*time.Minute)
			defer cancel()
			// The interface may still be coming up as Pith starts.
			for range 12 {
				if ifaces, err := c.Interfaces(ctx); err == nil {
					for _, i := range ifaces {
						if i.Name == m.Interface && i.HardwareAddr != m.HardwareAddr {
							_ = os.Remove(mark) // another adapter: not that router
							return
						}
					}
				}
				if s, err := c.router(ctx, m.Interface); err == nil {
					if _, err := s.device.Restore(ctx); err == nil {
						_ = os.Remove(mark)
					}
					return
				}
				select {
				case <-ctx.Done():
					return
				case <-time.After(5 * time.Second):
				}
			}
		}()
	})
}

// drain has link count as down until the returned function is called: its
// local proxies refuse connections and, while routing runs, rules skip it.
// When the helper can't be given the rules without link, or the drain can't
// be marked for a Pith that quits meanwhile to undo, nothing stays drained
// and drain fails: rules that kept sending connections to link would fail
// them while it has no address.
func (c *Client) drain(ctx context.Context, link api.Interface, standby api.InterfaceRef) (undrain func(), err error) {
	match := func(ref api.InterfaceRef) bool { return ref.Resolve([]api.Interface{link}) != nil }
	c.proxyMu.Lock()
	manager := c.proxies
	c.proxyMu.Unlock()
	if manager != nil {
		manager.Suspend(match)
	}
	c.mu.Lock()
	c.drained = &drainState{link: link, standby: standby}
	err = writeMark(c.opts.ConfigDir, filepath.Join(c.opts.ConfigDir, drainFile), nil)
	if err == nil {
		var cfg api.Config
		if cfg, err = c.loadConfig(ctx); err == nil {
			err = c.applyConfig(ctx, cfg)
		}
	}
	c.mu.Unlock()
	undrain = func() {
		if manager != nil {
			manager.Resume()
		}
		c.mu.Lock()
		defer c.mu.Unlock()
		c.drained = nil
		ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 20*time.Second)
		defer cancel()
		if cfg, err := c.loadConfig(ctx); err == nil && c.applyConfig(ctx, cfg) == nil {
			_ = os.Remove(filepath.Join(c.opts.ConfigDir, drainFile))
		}
	}
	if err != nil {
		undrain()
		return nil, fmt.Errorf("router: rules can't skip %s meanwhile: %w", link.Name, err)
	}
	return undrain, nil
}

// writeMark writes path, in dir, with v as JSON, or empty when v is nil.
func writeMark(dir, path string, v any) error {
	var data []byte
	if v != nil {
		var err error
		if data, err = json.Marshal(v); err != nil {
			return err
		}
	}
	if err := os.MkdirAll(dir, 0700); err != nil {
		return err
	}
	return os.WriteFile(path, data, 0600)
}

// drainState is an interface that counts as down, and the one macOS made
// primary instead.
type drainState struct {
	link    api.Interface
	standby api.InterfaceRef
}

// effectiveScene is the scene the helper runs: while an interface counts as
// down, without it.
func (c *Client) effectiveScene(scene api.Scene) api.Scene {
	if c.drained == nil {
		return scene
	}
	return withoutInterface(scene, c.drained.link, c.drained.standby)
}

// withoutInterface is scene with link skipped by its route rules, as if it
// were down: a rule tries its other interfaces; one left with none falls back
// to macOS's routing, through standby, when it may, and keeps link, which
// fails, when it may not.
func withoutInterface(scene api.Scene, link api.Interface, standby api.InterfaceRef) api.Scene {
	rules := make([]api.Rule, len(scene.Rules))
	for i, rule := range scene.Rules {
		rules[i] = rule
		if rule.Action != api.Route {
			continue
		}
		var via []api.InterfaceRef
		for _, v := range rule.Via {
			if v.Resolve([]api.Interface{link}) == nil {
				via = append(via, v)
			}
		}
		switch {
		case len(via) > 0:
			rules[i].Via = via
		case rule.Fallback && standby.Name != "":
			rules[i].Via = []api.InterfaceRef{standby}
		}
	}
	scene.Rules = rules
	return scene
}

// recoverDrain gives the helper the saved rules back after a Pith that quit
// while an interface counted as down; the mark stays until that works.
func (c *Client) recoverDrain(ctx context.Context, cfg api.Config) {
	path := filepath.Join(c.opts.ConfigDir, drainFile)
	if c.drained != nil {
		return
	}
	if _, err := os.Stat(path); err != nil {
		return
	}
	if c.applyConfig(ctx, cfg) == nil {
		_ = os.Remove(path)
	}
}
