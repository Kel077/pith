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
	"sync/atomic"
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
	speedURL       string // of the speed test server
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
	routers        map[string]*routerSession
	routerPort     string          // of the routers' web admin: 443 when empty
	routerPauses   []time.Duration // a renewing router's pauses: routerPauses when nil
	routerSettle   time.Duration   // for apps to move to another primary: two seconds when zero
	renewing       sync.Mutex      // one router renews at a time
	renewals       sync.WaitGroup  // address changes in progress, which Close waits for
	recovery       sync.Once       // of a router a renewal left without an address
	drained        *drainState     // guarded by mu; an interface that counts as down
	secretGet      func(ctx context.Context, service, account string) (string, error)
	secretSet      func(ctx context.Context, service, account, secret string) error
	proxyMu        sync.Mutex
	proxies        *proxy.Manager
	newProxies     func() (*proxy.Manager, error) // tests inject loopback-only forwarding
	proxiesLoaded  bool
	sceneRules     atomic.Pointer[map[string]sceneRules]                                // of the scenes local proxies go out by
	systemDial     func(ctx context.Context, network, address string) (net.Conn, error) // as macOS routes: a plain dialer when nil
	closed         bool
	lifetime       context.Context
	closeLifetime  context.CancelFunc
	subscriptions  sync.WaitGroup
	routingPending bool // guarded by mu; persisted configuration has not reached the helper
	exitMu         sync.Mutex
	exits          map[string]*exitState
	exitChanges    map[string]*exitChange
	exitAliases    atomic.Pointer[map[string]string]
	exitJobs       sync.WaitGroup // public-address lookups and device identifications
	exitNow        func() time.Time
	probeTimeout   time.Duration // probeTimeout when zero
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
		probeURLs: [2]string{"https://api.ipify.org", "https://api6.ipify.org"}, speedURL: speedServer, secretGet: keychain.Get, secretSet: keychain.Set,
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
	links, err := c.interfaces(ctx)
	if err == nil {
		c.observeExits(links)
	}
	return links, err
}
