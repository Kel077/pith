// Package client implements api.Service for the menu bar app. Configuration,
// interface discovery, probes and rule checks do not need the privileged helper.
package client

import (
	"context"
	"net"
	"net/http"
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
	http       *http.Client
	peerUID    uint32
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
	c := &Client{opts: opts, configPath: filepath.Join(opts.ConfigDir, "config.json"), interfaces: netif.List, lookup: net.DefaultResolver.LookupNetIP, dialer: netif.Dialer,
		probeURLs: [2]string{"https://api.ipify.org", "https://api6.ipify.org"}}
	c.initIPC()
	return c, nil
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
