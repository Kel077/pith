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
	"github.com/Kel077/pith/internal/helper"
	"github.com/Kel077/pith/internal/helper/identity"
	"github.com/Kel077/pith/internal/keychain"
	"github.com/Kel077/pith/internal/netif"
	"github.com/Kel077/pith/internal/proxy"
)

type Options struct {
	// ResourcesDir is the app's Contents/Resources directory.
	ResourcesDir string
	// ConfigDir defaults to ~/Library/Application Support/Pith.
	ConfigDir string
	// Version is the app's informational version, never a helper identity check.
	Version string
	// ExpectedHelper and HelperPath are for a controller with an independently
	// frozen artifact (such as an acceptance session). The app leaves them empty
	// and uses its compiled identity and bundled resources. They never skip checks.
	ExpectedHelper *identity.Identity
	HelperPath     string
	// SocketPath overrides the helper socket, primarily for development tests.
	SocketPath string
	// Prompts supplies localized authorization dialog text. Empty fields use
	// English defaults; the client does not choose or load a UI locale.
	Prompts Prompts
}

type Prompts struct {
	Install   string
	Update    string
	Uninstall string
}

type Client struct {
	opts           Options
	configPath     string
	mu             sync.Mutex
	installMu      sync.Mutex
	executePlan    func(context.Context, helper.InstallPlan) error
	interfaces     func(context.Context) ([]api.Interface, error)
	lookup         func(context.Context, string, string) ([]netip.Addr, error)
	dialer         func(string, time.Duration, []string) (*net.Dialer, error)
	probeURLs      [2]string
	http           *http.Client
	peerUID        uint32
	expectedHelper identity.Identity
	identityErr    error
	binaryPath     string
	artifacts      artifactCache
	artifactOpen   func(string, bool) (*os.File, artifactStamp, error)
	checkFile      func(string, bool) error
	hotspots       map[string]*hotspotSession
	hotspotPort    string        // of the hotspots' web admin: 80 when empty
	hotspotPace    time.Duration // between tests after a renewal: a second when zero
	hotspotIndex   func(string) (int, error)
	secretGet      func(ctx context.Context, service, account string) (string, error)
	secretSet      func(ctx context.Context, service, account, secret string) error
	proxyMu        sync.Mutex
	proxies        *proxy.Manager
	newProxies     func() (*proxy.Manager, error) // tests inject loopback-only forwarding
	proxiesLoaded  bool
	closed         bool
	lifetime       context.Context
	closeLifetime  context.CancelFunc
	subscriptions  sync.WaitGroup
	routingPending bool // guarded by mu; persisted configuration has not reached the helper
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
		probeURLs: [2]string{"https://api.ipify.org", "https://api6.ipify.org"}, secretGet: keychain.Get, secretSet: keychain.Set,
		hotspotIndex: func(name string) (int, error) {
			link, err := net.InterfaceByName(name)
			if err != nil {
				return 0, err
			}
			return link.Index, nil
		}}
	c.initIdentity()
	c.lifetime, c.closeLifetime = context.WithCancel(context.Background())
	c.executePlan = func(ctx context.Context, plan helper.InstallPlan) error { return plan.Execute(ctx) }
	c.initIPC()
	return c, nil
}

func (c *Client) Interfaces(ctx context.Context) ([]api.Interface, error) {
	return c.interfaces(ctx)
}
