package client

import (
	"context"
	"errors"
	"io"
	"net"
	"net/netip"
	"os"
	"slices"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/Kel077/pith/internal/api"
	"github.com/Kel077/pith/internal/proxy"
)

// routeFixture is a client whose interfaces and macOS routing are scripted:
// it tells what each connection by the rules went out through.
type routeFixture struct {
	c       *Client
	route   func(context.Context, string, proxy.Target) (net.Conn, error)
	mu      sync.Mutex
	went    []string         // "example0 host:port", "system host:port"
	fail    map[string]error // by interface name, or "system"
	looked  int              // names looked up
	address map[string]string
}

func ruleRef(name string) api.InterfaceRef { return api.InterfaceRef{Name: name} }

func routeConfig() api.Config {
	cfg := api.DefaultConfig()
	cfg.Scenes = []api.Scene{
		{ID: "home", Rules: []api.Rule{
			{ID: "video", Enabled: true, Targets: []string{"*.video.example.com"}, Protocol: api.AnyProtocol, Action: api.Route, Via: []api.InterfaceRef{ruleRef("example0"), ruleRef("example1")}},
			{ID: "ads", Enabled: true, Targets: []string{"ads.example.com"}, Protocol: api.AnyProtocol, Action: api.Block},
			{ID: "app", Enabled: true, Apps: []api.AppRef{{Path: "/Applications/Safari.app"}}, Targets: []string{"app.example.com"}, Protocol: api.AnyProtocol, Action: api.Route, Via: []api.InterfaceRef{ruleRef("example1")}},
			{ID: "lan", Enabled: true, Targets: []string{"198.51.100.0/24"}, Protocol: api.TCP, Action: api.Route, Via: []api.InterfaceRef{ruleRef("example1")}, Fallback: true},
			{ID: "off", Enabled: false, Targets: []string{"off.example.com"}, Protocol: api.AnyProtocol, Action: api.Route, Via: []api.InterfaceRef{ruleRef("example1")}},
			{ID: "datagrams", Enabled: true, Targets: []string{"udp.example.com"}, Protocol: api.UDP, Action: api.Block},
		}},
		{ID: "names", Rules: []api.Rule{
			{ID: "all-names", Enabled: true, Targets: []string{"*.example.com"}, Protocol: api.AnyProtocol, Action: api.Route, Via: []api.InterfaceRef{ruleRef("example1")}},
			{ID: "an-app", Enabled: true, Apps: []api.AppRef{{Path: "/Applications/Safari.app"}}, Targets: []string{"203.0.113.0/24"}, Protocol: api.AnyProtocol, Action: api.Block},
		}},
		{ID: "unused"},
	}
	cfg.Selected = "home"
	cfg.Proxies = []api.Proxy{{Port: 1080, Scene: "home", Enabled: true}, {Port: 1081, Scene: "names", Enabled: true}}
	return cfg
}

func newRouteFixture(t *testing.T) *routeFixture {
	t.Helper()
	f := &routeFixture{c: testClient(t), fail: map[string]error{}, address: map[string]string{}}
	conn := func() net.Conn {
		a, b := net.Pipe()
		t.Cleanup(func() { a.Close(); b.Close() })
		return a
	}
	note := func(through string, address string) error {
		f.mu.Lock()
		defer f.mu.Unlock()
		f.went = append(f.went, through+" "+address)
		return f.fail[through]
	}
	f.c.lookup = func(_ context.Context, _, host string) ([]netip.Addr, error) {
		f.mu.Lock()
		defer f.mu.Unlock()
		f.looked++
		if addr, ok := f.address[host]; ok {
			return []netip.Addr{netip.MustParseAddr("2001:db8::99"), netip.MustParseAddr(addr)}, nil
		}
		return nil, &net.DNSError{Err: "no such host", Name: host, IsNotFound: true}
	}
	f.c.systemDial = func(_ context.Context, network, address string) (net.Conn, error) {
		if network != "tcp" {
			t.Errorf("system dial over %q", network)
		}
		if err := note("system", address); err != nil {
			return nil, err
		}
		return conn(), nil
	}
	f.route = f.c.routeByRules(func(_ context.Context, via api.InterfaceRef, target proxy.Target) (net.Conn, error) {
		kind := "address"
		if target.IsDomain {
			kind = "name"
		}
		if err := note(via.Name, kind+" "+target.String()); err != nil {
			return nil, err
		}
		return conn(), nil
	})
	if err := f.c.compileScenes(routeConfig()); err != nil {
		t.Fatal(err)
	}
	return f
}

// connect asks for a connection by the rules of scene, and returns its
// error with what it went out through, in order.
func (f *routeFixture) connect(scene, host string, port uint16, domain bool) ([]string, error) {
	f.mu.Lock()
	f.went = nil
	f.mu.Unlock()
	conn, err := f.route(context.Background(), scene, proxy.Target{Host: host, Port: port, IsDomain: domain})
	if (conn == nil) == (err == nil) {
		err = errors.Join(err, errors.New("a connection and an error, or neither"))
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.went), err
}

func TestProxyByRulesGoesWhereTheRulesSend(t *testing.T) {
	f := newRouteFixture(t)
	f.address["nas.example.com"] = "198.51.100.7"
	f.address["other.example.com"] = "203.0.113.5"
	for _, tc := range []struct {
		name   string
		host   string
		port   uint16
		domain bool
		fail   map[string]error
		went   []string
		err    error
	}{
		{name: "a rule's first interface", host: "cdn.video.example.com", port: 443, domain: true,
			went: []string{"example0 name cdn.video.example.com:443"}},
		{name: "its next interface when the first doesn't connect", host: "cdn.video.example.com", port: 443, domain: true,
			fail: map[string]error{"example0": syscall.ECONNREFUSED},
			went: []string{"example0 name cdn.video.example.com:443", "example1 name cdn.video.example.com:443"}},
		{name: "no fallback: it fails", host: "cdn.video.example.com", port: 443, domain: true,
			fail: map[string]error{"example0": syscall.ECONNREFUSED, "example1": syscall.EHOSTUNREACH},
			went: []string{"example0 name cdn.video.example.com:443", "example1 name cdn.video.example.com:443"}, err: syscall.EHOSTUNREACH},
		{name: "a block rule", host: "ads.example.com", port: 443, domain: true, err: syscall.EACCES},
		{name: "a rule that names an app never matches", host: "app.example.com", port: 443, domain: true,
			went: []string{"system app.example.com:443"}},
		{name: "a rule turned off", host: "off.example.com", port: 80, domain: true,
			went: []string{"system off.example.com:80"}},
		{name: "a rule for UDP", host: "udp.example.com", port: 443, domain: true,
			went: []string{"system udp.example.com:443"}},
		{name: "an address a rule names", host: "198.51.100.9", port: 22,
			went: []string{"example1 address 198.51.100.9:22"}},
		{name: "fallback: where macOS sends it", host: "198.51.100.9", port: 22,
			fail: map[string]error{"example1": syscall.ENETUNREACH},
			went: []string{"example1 address 198.51.100.9:22", "system 198.51.100.9:22"}},
		{name: "a name whose address a rule names goes to that address", host: "nas.example.com", port: 445, domain: true,
			went: []string{"example1 address 198.51.100.7:445"}},
		{name: "a name no rule names, nor its address", host: "other.example.com", port: 443, domain: true,
			went: []string{"system other.example.com:443"}},
		{name: "a name that isn't found", host: "nowhere.example.com", port: 443, domain: true,
			went: []string{"system nowhere.example.com:443"}},
		{name: "an address no rule names", host: "2001:db8::7", port: 443,
			went: []string{"system [2001:db8::7]:443"}},
		{name: "macOS can't reach it either", host: "203.0.113.9", port: 443,
			fail: map[string]error{"system": syscall.ENETDOWN},
			went: []string{"system 203.0.113.9:443"}, err: syscall.ENETDOWN},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f.mu.Lock()
			f.fail = tc.fail
			f.mu.Unlock()
			went, err := f.connect("home", tc.host, tc.port, tc.domain)
			if !slices.Equal(went, tc.went) {
				t.Errorf("went %q, want %q", went, tc.went)
			}
			if tc.err == nil && err != nil || tc.err != nil && !errors.Is(err, tc.err) {
				t.Errorf("error %v, want %v", err, tc.err)
			}
		})
	}
}

func TestProxyByRulesLooksNamesUpOnlyForAddressRules(t *testing.T) {
	f := newRouteFixture(t)
	// The scene's only rule that names addresses names an app too.
	went, err := f.connect("names", "www.example.net", 443, true)
	if err != nil || !slices.Equal(went, []string{"system www.example.net:443"}) || f.looked != 0 {
		t.Fatalf("went %q, %v, after %d lookups", went, err, f.looked)
	}
	went, err = f.connect("names", "www.example.com", 443, true)
	if err != nil || !slices.Equal(went, []string{"example1 name www.example.com:443"}) || f.looked != 0 {
		t.Fatalf("went %q, %v, after %d lookups", went, err, f.looked)
	}
	// A name a rule names isn't looked up either.
	if _, err := f.connect("home", "cdn.video.example.com", 443, true); err != nil || f.looked != 0 {
		t.Fatalf("%v, after %d lookups", err, f.looked)
	}
	if _, err := f.connect("home", "www.example.net", 443, true); err != nil || f.looked != 1 {
		t.Fatalf("%v, after %d lookups", err, f.looked)
	}
	// A scene no proxy goes out by, or that is gone, connects nothing.
	for _, scene := range []string{"unused", "gone", ""} {
		if went, err := f.connect(scene, "www.example.com", 443, true); api.Code(err) != api.ErrNoScene || len(went) != 0 {
			t.Fatalf("scene %q: went %q, %v", scene, went, err)
		}
	}
}

func TestProxyByRulesSkipsAnInterfaceThatIsSuspended(t *testing.T) {
	f := newRouteFixture(t)
	manager, err := proxy.New(proxy.Options{Dial: func(context.Context, api.InterfaceRef, proxy.Target) (net.Conn, error) {
		return nil, errors.New("unused")
	}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { manager.Close() })
	f.c.proxies = manager
	manager.Suspend(func(via api.InterfaceRef) bool { return via.Name == "example0" })
	went, err := f.connect("home", "cdn.video.example.com", 443, true)
	if err != nil || !slices.Equal(went, []string{"example1 name cdn.video.example.com:443"}) {
		t.Fatalf("went %q, %v", went, err)
	}
	manager.Suspend(func(via api.InterfaceRef) bool { return true })
	if went, err := f.connect("home", "cdn.video.example.com", 443, true); api.Code(err) != api.ErrInterfaceUnavailable || len(went) != 0 {
		t.Fatalf("with every interface suspended: went %q, %v", went, err)
	}
	// A rule that falls back still goes where macOS sends it.
	if went, err := f.connect("home", "198.51.100.9", 22, false); err != nil || !slices.Equal(went, []string{"system 198.51.100.9:22"}) {
		t.Fatalf("a rule that falls back: went %q, %v", went, err)
	}
	manager.Resume()
	if went, _ := f.connect("home", "cdn.video.example.com", 443, true); !slices.Equal(went, []string{"example0 name cdn.video.example.com:443"}) {
		t.Fatalf("after Resume: went %q", went)
	}
}

// A port by rules runs as any local proxy does; rules saved later apply to
// its next connections, and a scene can't go while a port goes out by it.
func TestLocalProxyByRulesFollowsSavedRules(t *testing.T) {
	c := testClient(t)
	installProxyFixture(t, c)
	echo := proxyEchoServer(t)
	var mu sync.Mutex
	var went []string
	dial := func(ctx context.Context, via api.InterfaceRef, _ proxy.Target) (net.Conn, error) {
		mu.Lock()
		went = append(went, via.Name)
		mu.Unlock()
		return (&net.Dialer{}).DialContext(ctx, "tcp4", echo)
	}
	c.newProxies = func() (*proxy.Manager, error) {
		return proxy.New(proxy.Options{Dial: dial, Route: c.routeByRules(dial)})
	}
	cfg := api.DefaultConfig()
	cfg.Scenes[0].Rules = []api.Rule{{ID: "docs", Enabled: true, Targets: []string{"192.0.2.0/24"}, Protocol: api.AnyProtocol, Action: api.Route, Via: []api.InterfaceRef{ruleRef("example1")}}}
	cfg.Proxies = []api.Proxy{{Port: proxyPort(t), Scene: "default", Enabled: true}}
	if err := c.SaveConfig(context.Background(), cfg); err != nil {
		t.Fatal(err)
	}
	conn := openLocalProxy(t, cfg.Proxies[0].Port)
	proxyExchange(t, conn, "by the rules")
	mu.Lock()
	if !slices.Equal(went, []string{"example1"}) {
		t.Fatalf("went through %q", went)
	}
	mu.Unlock()
	if st, _ := c.Status(context.Background()); len(st.Proxies) != 1 || !st.Proxies[0].Listening || st.Proxies[0].Error != nil || st.Proxies[0].Open != 1 {
		t.Fatal("status of the port by rules", st.Proxies)
	}

	cfg.Scenes[0].Rules[0].Action, cfg.Scenes[0].Rules[0].Via = api.Block, nil
	if err := c.SaveConfig(context.Background(), cfg); err != nil {
		t.Fatal(err)
	}
	proxyExchange(t, conn, "an open connection stays")
	blocked, err := net.Dial("tcp4", net.JoinHostPort("127.0.0.1", portString(cfg.Proxies[0].Port)))
	if err != nil {
		t.Fatal(err)
	}
	defer blocked.Close()
	if _, err := blocked.Write([]byte{5, 1, 0, 5, 1, 0, 1, 192, 0, 2, 80, 1, 187}); err != nil {
		t.Fatal(err)
	}
	var reply [4]byte
	if _, err := io.ReadFull(blocked, reply[:]); err != nil || reply != [4]byte{5, 0, 5, 2} {
		t.Fatal("a blocked connection was not refused as not allowed", reply, err)
	}

	cfg.Scenes = []api.Scene{{ID: "other"}}
	cfg.Selected = "other"
	if err := c.SaveConfig(context.Background(), cfg); api.Code(err) != api.ErrInvalidConfig {
		t.Fatal("a scene a port goes out by was removed", err)
	}
}

// What macOS would send to this Mac itself is refused: a port by rules
// reaches no more of this Mac than a port through an interface does.
func TestProxyByRulesNeverConnectsToLoopback(t *testing.T) {
	for address, refused := range map[string]bool{
		"127.0.0.1:8080": true, "127.9.9.9:80": true, "[::1]:8080": true, "[::ffff:127.0.0.1]:80": true,
		"0.0.0.0:80": true, "[::]:80": true, "224.0.0.251:5353": true, "[ff02::fb]:5353": true,
		"192.0.2.9:443": false, "[2001:db8::9]:443": false, "[fe80::1%lo0]:80": false,
	} {
		if err := notLoopback("tcp", address, nil); errors.Is(err, syscall.EACCES) != refused || !refused && err != nil {
			t.Errorf("notLoopback(%s) = %v, refused %v", address, err, refused)
		}
	}
	if notLoopback("tcp", "example.com:80", nil) == nil || notLoopback("tcp", "192.0.2.9", nil) == nil {
		t.Error("what isn't a literal address and a port was let through")
	}
	// A listener of this Mac alone, asked for by address and by name.
	ln, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	port := uint16(ln.Addr().(*net.TCPAddr).Port)
	c := testClient(t)
	route := c.routeByRules(func(context.Context, api.InterfaceRef, proxy.Target) (net.Conn, error) {
		return nil, errors.New("no rule sends anything through an interface")
	})
	cfg := api.DefaultConfig()
	cfg.Proxies = []api.Proxy{{Port: 1080, Scene: "default", Enabled: true}}
	if err := c.compileScenes(cfg); err != nil {
		t.Fatal(err)
	}
	for _, target := range []proxy.Target{{Host: "127.0.0.1", Port: port}, {Host: "localhost", Port: port, IsDomain: true}} {
		conn, err := route(context.Background(), "default", target)
		if conn != nil {
			conn.Close()
		}
		if !errors.Is(err, syscall.EACCES) {
			t.Errorf("%v: %v, want it refused", target, err)
		}
	}
}

// A port goes out by the rules of any scene of the configuration, not the
// default one alone; the same save takes a device off the list, and a
// client that starts on the saved configuration runs the port again.
func TestLocalProxyByRulesOfAnotherSceneSavesReloadsAndRevokes(t *testing.T) {
	c := testClient(t)
	installProxyFixture(t, c)
	echo := proxyEchoServer(t)
	c.newProxies = func() (*proxy.Manager, error) {
		dial := func(ctx context.Context, _ api.InterfaceRef, _ proxy.Target) (net.Conn, error) {
			return (&net.Dialer{}).DialContext(ctx, "tcp4", echo)
		}
		return proxy.New(proxy.Options{Dial: dial, Route: c.routeByRules(dial), Listen: loopbackListen("192.0.2.10")})
	}
	cfg := api.DefaultConfig()
	cfg.Scenes = append(cfg.Scenes, api.Scene{ID: "office", Name: "Office", Rules: []api.Rule{
		{ID: "docs", Enabled: true, Targets: []string{"192.0.2.0/24"}, Protocol: api.AnyProtocol, Action: api.Route, Via: []api.InterfaceRef{ruleRef("example1")}},
	}})
	port := proxyPort(t)
	cfg.Proxies = []api.Proxy{{Port: port, Via: ruleRef("example0"), Enabled: true, Shared: true}}
	cfg.Devices = []api.Device{{Address: "192.0.2.10"}}
	if err := c.SaveConfig(context.Background(), cfg); err != nil {
		t.Fatal(err)
	}
	listed := openLocalProxy(t, port)
	proxyExchange(t, listed, "listed")

	// One save: the port goes out by the office scene's rules, and the
	// device is no longer listed.
	cfg.Proxies[0].Via, cfg.Proxies[0].Scene = api.InterfaceRef{}, "office"
	cfg.Devices = nil
	if err := c.SaveConfig(context.Background(), cfg); err != nil {
		t.Fatal("a port by the rules of a scene other than the default one was not applied: ", err)
	}
	_ = listed.SetDeadline(time.Now().Add(time.Second))
	if _, err := listed.Read(make([]byte, 1)); err == nil || errors.Is(err, os.ErrDeadlineExceeded) {
		t.Fatal("the device taken off the list kept its stream", err)
	}
	refused, err := net.DialTimeout("tcp4", net.JoinHostPort("127.0.0.1", portString(port)), time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer refused.Close()
	_ = refused.SetDeadline(time.Now().Add(3 * time.Second))
	var reply [2]byte
	if _, err := refused.Write([]byte{5, 1, 0}); err != nil {
		t.Fatal(err)
	}
	if _, err := io.ReadFull(refused, reply[:]); err != nil || reply != [2]byte{5, 0xff} {
		t.Fatal("the device taken off the list was served again", reply, err)
	}
	if st, _ := c.Status(context.Background()); len(st.Proxies) != 1 || !st.Proxies[0].Listening || st.Proxies[0].Error != nil {
		t.Fatal("status of the port by another scene's rules", st.Proxies)
	}
	if err := c.Close(); err != nil {
		t.Fatal(err)
	}

	// A client that starts on what was saved.
	again, err := New(Options{ConfigDir: c.opts.ConfigDir, SocketPath: c.opts.SocketPath})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { again.Close() })
	again.binaryPath, again.identityErr = c.binaryPath, nil
	again.checkFile = func(string, bool) error { return os.ErrNotExist }
	installProxyFixture(t, again)
	again.newProxies = func() (*proxy.Manager, error) {
		dial := func(context.Context, api.InterfaceRef, proxy.Target) (net.Conn, error) {
			return nil, errors.New("unused")
		}
		return proxy.New(proxy.Options{Dial: dial, Route: again.routeByRules(dial), Listen: loopbackListen("192.0.2.10")})
	}
	loaded, err := again.LoadConfig(context.Background())
	if err != nil || len(loaded.Proxies) != 1 || loaded.Proxies[0].Scene != "office" {
		t.Fatal("the saved configuration did not load: ", loaded.Proxies, err)
	}
	if st, _ := again.Status(context.Background()); len(st.Proxies) != 1 || !st.Proxies[0].Listening {
		t.Fatal("the port did not start again", st.Proxies)
	}
}
