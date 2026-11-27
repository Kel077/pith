package client

import (
	"context"
	"time"

	"github.com/Kel077/pith/internal/api"
	"github.com/Kel077/pith/internal/netif"
	"github.com/Kel077/pith/internal/router"
)

type openWrtExit struct {
	ssh   router.OpenWrt
	link  api.Interface
	index int
}

func (c *Client) openWrtConfig(links []api.Interface, iface string) (api.OpenWrtRouter, bool) {
	c.exitMu.Lock()
	defer c.exitMu.Unlock()
	if i := api.OpenWrtIndex(c.openWrt, links, iface); i >= 0 {
		return c.openWrt[i], true
	}
	return api.OpenWrtRouter{}, false
}

func (c *Client) openWrtFor(ctx context.Context, via api.InterfaceRef) (*openWrtExit, error) {
	c.exitMu.Lock()
	configured := len(c.openWrt) != 0
	c.exitMu.Unlock()
	if !configured {
		return nil, nil
	}
	links, err := c.Interfaces(ctx)
	if err != nil {
		return nil, err
	}
	link := via.Resolve(links)
	if link == nil {
		return nil, nil
	}
	cfg, ok := c.openWrtConfig(links, link.Name)
	if !ok {
		return nil, nil
	}
	if !link.Up || !link.CanProxy() || link.Kind != api.KindEthernet && link.Kind != api.KindWiFi {
		return nil, &api.Error{Code: api.ErrInterfaceUnavailable, Detail: "the OpenWrt interface is unavailable"}
	}
	index, err := c.hotspotIndex(link.Name)
	if err != nil || index <= 0 {
		return nil, &api.Error{Code: api.ErrInterfaceUnavailable, Detail: "the OpenWrt interface has no kernel index"}
	}
	return &openWrtExit{ssh: router.OpenWrt{Config: cfg, Interface: link.Name, Run: c.openWrtSSH}, link: *link, index: index}, nil
}

func (c *Client) renewOpenWrt(ctx context.Context, target *openWrtExit) (r api.RouterRenewal, err error) {
	started := time.Now()
	defer func() { r.Took = time.Since(started) }()
	status, err := target.ssh.Status(ctx)
	if err != nil {
		return r, err
	}
	if !status.Connected {
		return r, &api.Error{Code: api.ErrInterfaceUnavailable, Detail: "the OpenWrt WAN has no active IPv4 lease"}
	}
	via := api.InterfaceRef{Name: target.link.Name, HardwareAddr: target.link.HardwareAddr}
	r.Before, err = c.openWrtProbe(ctx, target, probeOptions{})
	if err != nil {
		return r, err
	}
	current, err := c.openWrtFor(ctx, via)
	if err != nil {
		return r, err
	}
	if current == nil || current.ssh.Config != target.ssh.Config || current.link.Router != target.link.Router ||
		current.link.Name != via.Name || current.index != target.index {
		return r, &api.Error{Code: api.ErrInterfaceUnavailable, Detail: "the OpenWrt interface or configuration changed before rotation"}
	}
	// The router owns failover and recovery. Keep macOS routing and the
	// interface's listeners active while the WAN changes upstream.
	c.forgetExitAddresses(via.Name)
	defer func() {
		c.forgetExitAddresses(via.Name)
		if err == nil {
			c.publishExitProbe(via, r.After)
		}
	}()
	r.Tries = 1
	r.WAN, err = target.ssh.Rotate(ctx)
	if err != nil {
		// A lost SSH reply can follow a successful change. Only an explicit
		// new request can authorize another rotation.
		return r, err
	}
	waitOpenWrtWAN(ctx, target.ssh, r.WAN)
	r.After, err = c.retestPublicSeeded(ctx, r.Before, api.Probe{}, func(pctx context.Context, skip4, skip6 bool, _ int, limit time.Duration) (api.Probe, error) {
		return c.openWrtProbe(pctx, target, probeOptions{timeout: limit, skipV4: skip4, skipV6: skip6})
	})
	return r, err
}

// If the script already found a lease but its failover monitor still marks
// that WAN offline, give it a short chance to switch back before probing.
func waitOpenWrtWAN(ctx context.Context, ssh router.OpenWrt, wan *api.WANRotation) {
	if wan.WANState == "" || wan.WANState == "online" {
		return
	}
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	timer := time.NewTicker(time.Second)
	defer timer.Stop()
	for {
		status, err := ssh.Status(ctx)
		if err == nil && status.Connected && status.PublicIPv4 == wan.IPv4 {
			wan.WANState, wan.BackupState = status.OpenWrt.WANState, status.OpenWrt.BackupState
			if status.OpenWrt.IPv6Prefix != "" {
				wan.IPv6Prefix = status.OpenWrt.IPv6Prefix
			}
			if wan.WANState == "online" || wan.WANState == "" {
				return
			}
		}
		select {
		case <-ctx.Done():
			return
		case <-timer.C:
		}
	}
}

func (c *Client) openWrtProbe(ctx context.Context, target *openWrtExit, opts probeOptions) (api.Probe, error) {
	links, err := c.Interfaces(ctx)
	if err != nil {
		return api.Probe{}, err
	}
	name := target.link.Name
	link := (api.InterfaceRef{Name: name, HardwareAddr: target.link.HardwareAddr}).Resolve(links)
	check := func() error {
		index, err := c.hotspotIndex(name)
		if err != nil || index != target.index {
			return &api.Error{Code: api.ErrInterfaceUnavailable, Detail: "the OpenWrt interface index changed"}
		}
		return nil
	}
	if link == nil || !link.Up || link.Name != name || link.Router != target.link.Router || link.Kind != target.link.Kind {
		return api.Probe{}, &api.Error{Code: api.ErrInterfaceUnavailable, Detail: "the OpenWrt interface changed"}
	}
	if err := check(); err != nil {
		return api.Probe{}, err
	}
	p, err := c.probeWith(netif.WithBinding(ctx, target.index, check), name, "", links, opts)
	if err != nil {
		return api.Probe{}, err
	}
	if err := check(); err != nil {
		return api.Probe{}, err
	}
	return p, nil
}
