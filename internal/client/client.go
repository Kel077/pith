// Package client implements api.Service for the menu bar app. Configuration,
// interface discovery, probes and rule checks do not need the privileged helper.
package client

import (
	"context"
	"net"
	"net/netip"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/Kel077/pith/internal/api"
	"github.com/Kel077/pith/internal/netif"
)

type Options struct {
	// ResourcesDir is the app's Contents/Resources directory.
	ResourcesDir string
	// ConfigDir defaults to ~/Library/Application Support/Pith.
	ConfigDir string
	// Version is the app/helper build version; an empty value means "dev".
	Version string
	// SocketPath overrides the helper socket, primarily for development tests.
	SocketPath string
}

type Client struct {
	opts       Options
	configPath string
	mu         sync.Mutex
	interfaces func(context.Context) ([]api.Interface, error)
	lookup     func(context.Context, string, string) ([]netip.Addr, error)
	dialer     func(string, time.Duration, []string) (*net.Dialer, error)
	probeURLs  [2]string
}

var _ api.Service = (*Client)(nil)

// New does not create files, launch processes or install/start the helper.
func New(opts Options) (*Client, error) {
	if opts.ConfigDir == "" {
		homeDir, err := os.UserHomeDir()
		if err != nil {
			return nil, err
		}
		opts.ConfigDir = filepath.Join(homeDir, "Library", "Application Support", "Pith")
	}
	if opts.Version == "" {
		opts.Version = "dev"
	}
	return &Client{opts: opts, configPath: filepath.Join(opts.ConfigDir, "config.json"), interfaces: netif.List, lookup: net.DefaultResolver.LookupNetIP, dialer: netif.Dialer,
		probeURLs: [2]string{"https://api.ipify.org", "https://api6.ipify.org"}}, nil
}

func (c *Client) Interfaces(ctx context.Context) ([]api.Interface, error) {
	interfaces, err := c.interfaces(ctx)
	if err != nil {
		return nil, err
	}
	cfg, err := c.LoadConfig(ctx)
	if err != nil {
		return nil, err
	}
	for i := range interfaces {
		iface := &interfaces[i]
		ref := api.InterfaceRef{Name: iface.Name, HardwareAddr: iface.HardwareAddr}
		if alias := cfg.Aliases[ref.Key()]; alias != "" {
			iface.Label = alias
		}
	}
	return interfaces, nil
}

func helperMissing() error { return &api.Error{Code: api.ErrHelperMissing} }

// Helper-backed operations are wired in the IPC milestone. Missing-helper
// errors are deliberate: the app must never mistake a local mock for routing.
func (c *Client) Status(ctx context.Context) (api.Status, error) {
	return api.Status{State: api.Stopped}, ctx.Err()
}
func (c *Client) InstallHelper(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	return helperMissing()
}
func (c *Client) UninstallHelper(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	return helperMissing()
}
func (c *Client) Start(ctx context.Context, sceneID string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	return helperMissing()
}
func (c *Client) Stop(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	return helperMissing()
}
func (c *Client) Subscribe(ctx context.Context) (<-chan api.Event, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return nil, helperMissing()
}
func (c *Client) Connections(ctx context.Context, q api.ConnectionQuery) ([]api.Connection, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return nil, helperMissing()
}
func (c *Client) ClearConnections(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	return helperMissing()
}
func (c *Client) Traffic(ctx context.Context, q api.TrafficQuery) (api.Traffic, error) {
	if err := ctx.Err(); err != nil {
		return api.Traffic{}, err
	}
	return api.Traffic{}, helperMissing()
}
