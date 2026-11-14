package mock

import (
	"context"
	"testing"
	"time"

	"github.com/Kel077/pith/internal/api"
)

func TestSimulation(t *testing.T) {
	s := New(Options{Sample: true, HelperInstalled: true, Running: true, Seed: 1})
	s.Simulate(20 * time.Second)
	conns, err := s.Connections(context.Background(), api.ConnectionQuery{})
	if err != nil || len(conns) == 0 {
		t.Fatalf("Connections: %d, %v", len(conns), err)
	}
	seen := map[api.Outcome]int{}
	for _, c := range conns {
		seen[c.Outcome]++
		if c.Outcome == api.Routed && c.Interface == "" {
			t.Errorf("routed connection without an interface: %+v", c)
		}
	}
	if seen[api.Routed] == 0 || seen[api.System] == 0 {
		t.Errorf("outcomes %v: want routed and system connections", seen)
	}
	tr, _ := s.Traffic(context.Background(), api.TrafficQuery{Step: time.Second})
	if len(tr.Apps) == 0 || len(tr.Interfaces) == 0 || len(tr.Series) == 0 {
		t.Errorf("traffic %+v", tr)
	}
	res, err := s.Check(context.Background(), api.CheckInput{App: appWeChat, Target: "weixin.qq.com"})
	if err != nil || res.RuleID != "r-wechat" || res.Interface != "en7" {
		t.Errorf("Check(WeChat) = %+v, %v", res, err)
	}
	res, _ = s.Check(context.Background(), api.CheckInput{Target: "stats.doubleclick.net"})
	if res.Outcome != api.Blocked {
		t.Errorf("Check(ads) = %+v, want blocked", res)
	}
}

// A rule change ends the open connections it sends elsewhere, and only
// those, as the engine does: normally, with no problem, for the app to
// reconnect.
func TestRuleChangeEndsAffectedConnections(t *testing.T) {
	ctx := context.Background()
	s := New(Options{Sample: true, HelperInstalled: true, Running: true, Seed: 1})
	var open []api.Connection
	wechat := func() bool {
		open, _ = s.Connections(ctx, api.ConnectionQuery{Open: true})
		for _, c := range open {
			if c.RuleID == "r-wechat" {
				return true
			}
		}
		return false
	}
	for i := 0; i < 120 && !wechat(); i++ {
		s.Simulate(time.Second)
	}
	st, _ := s.Status(ctx)
	cfg, _ := s.LoadConfig(ctx)
	changed := false
	for i := range cfg.Scenes {
		for j := range cfg.Scenes[i].Rules {
			if r := &cfg.Scenes[i].Rules[j]; cfg.Scenes[i].ID == st.SceneID && r.ID == "r-wechat" {
				r.Action, r.Via, changed = api.Block, nil, true
			}
		}
	}
	if !changed || !wechat() {
		t.Fatal("no open connection of the running scene's WeChat rule")
	}
	if err := s.SaveConfig(ctx, cfg); err != nil {
		t.Fatal(err)
	}
	after, _ := s.Connections(ctx, api.ConnectionQuery{})
	now := map[uint64]api.Connection{}
	for _, c := range after {
		now[c.ID] = c
	}
	for _, c := range open {
		n, affected := now[c.ID], c.RuleID == "r-wechat"
		if affected == n.Ended.IsZero() || affected != (n.EndReason == api.EndRulesChanged) || n.Problem != "" || n.Outcome != c.Outcome {
			t.Errorf("connection %d of rule %q: ended %v, reason %q, problem %q, outcome %s", c.ID, c.RuleID, !n.Ended.IsZero(), n.EndReason, n.Problem, n.Outcome)
		}
	}
}

// The sample's local proxies report as the client's do: listening, an
// interface that isn't connected, a port another program holds; counters
// move, and a proxy sent through another interface starts them over.
func TestProxies(t *testing.T) {
	ctx := context.Background()
	s := New(Options{Sample: true, ProxyPortInUse: 1081, Seed: 1})
	s.Simulate(3 * time.Second)
	st, _ := s.Status(ctx)
	if len(st.Proxies) != 3 {
		t.Fatalf("proxies %+v", st.Proxies)
	}
	if p := st.Proxies[0]; !p.Listening || p.Error != nil || p.Received == 0 || p.Open == 0 {
		t.Errorf("1080 %+v", p)
	}
	if p := st.Proxies[1]; p.Listening || api.Code(p.Error) != api.ErrPortInUse {
		t.Errorf("1081 %+v", p)
	}
	if p := st.Proxies[2]; !p.Listening || api.Code(p.Error) != api.ErrInterfaceUnavailable || p.Received != 0 {
		t.Errorf("1082 %+v", p)
	}

	cfg, _ := s.LoadConfig(ctx)
	cfg.Proxies[0].Via = cfg.Proxies[2].Via
	if err := s.SaveConfig(ctx, cfg); err != nil {
		t.Fatal(err)
	}
	if st, _ = s.Status(ctx); st.Proxies[0].Received != 0 {
		t.Errorf("counters kept through a new interface: %+v", st.Proxies[0])
	}

	s.ifaces = append(s.ifaces, api.Interface{Name: "utun5", Kind: api.KindVPN, Up: true})
	cfg.Proxies[0].Via = api.InterfaceRef{Name: "utun5"}
	if err := s.SaveConfig(ctx, cfg); api.Code(err) != api.ErrInvalidConfig {
		t.Errorf("a VPN as a proxy's exit: %v", err)
	}
}

// A speed test gives a connected interface speeds of its kind, and says
// why one that isn't connected or doesn't exist has none.
func TestSpeedTest(t *testing.T) {
	s := New(Options{Sample: true, Seed: 1})
	ctx := context.Background()
	eth, err := s.SpeedTest(ctx, "en0")
	phone, _ := s.SpeedTest(ctx, "en7")
	if err != nil || !eth.OK || eth.Server == "" || eth.PublicIP == "" || eth.Download <= phone.Download || phone.Latency <= eth.Latency || phone.Upload <= 0 {
		t.Fatalf("Ethernet %+v, iPhone %+v, %v", eth, phone, err)
	}
	if wifi, err := s.SpeedTest(ctx, "en1"); err != nil || wifi.OK || wifi.Problem == "" {
		t.Fatalf("Wi-Fi, not connected: %+v %v", wifi, err)
	}
	if _, err := s.SpeedTest(ctx, "en99"); err == nil {
		t.Fatal("tested an interface that doesn't exist")
	}
}

func TestRouter(t *testing.T) {
	s := New(Options{Seed: 1})
	ctx := context.Background()
	if _, err := s.Router(ctx, hotspotIface); api.Code(err) != api.ErrUnsupported {
		t.Fatalf("a hotspot's router: %v", err)
	}
	s.LockRouter()
	if r, err := s.Router(ctx, routerIface); err != nil || !r.Locked || r.PublicIPv4 != "" {
		t.Fatalf("locked: %+v %v", r, err)
	}
	if _, err := s.RenewRouter(ctx, routerIface); api.Code(err) != api.ErrWrongPassword {
		t.Fatalf("renewed a locked router: %v", err)
	}
	if err := s.SetRouterPassword(ctx, routerIface, "wrong"); api.Code(err) != api.ErrWrongPassword {
		t.Fatalf("a wrong password: %v", err)
	}
	if err := s.SetRouterPassword(ctx, routerIface, "example-password"); err != nil {
		t.Fatal(err)
	}
	r, err := s.RenewRouter(ctx, routerIface)
	if err != nil || r.Before.PublicIPv4 == r.After.PublicIPv4 || r.After.PublicIPv4 == "" || r.Standby != hotspotIface {
		t.Fatalf("renewal: %+v %v", r, err)
	}
	if got, _ := s.Router(ctx, routerIface); got.PublicIPv4 != r.After.PublicIPv4 {
		t.Fatalf("the router says %s, the test %s", got.PublicIPv4, r.After.PublicIPv4)
	}
}

// The sample shares a proxy with a laptop: the devices it lists report
// what they move, and a stranger is refused until the configuration lists
// it. With nothing shared, there are no devices; a proxy can go out through
// another proxy rather than an interface.
func TestDevices(t *testing.T) {
	ctx := context.Background()
	s := New(Options{Sample: true, Seed: 1})
	s.Simulate(3 * time.Second)
	device := func(address string) (api.DeviceStatus, bool) {
		st, _ := s.Status(ctx)
		for _, d := range st.Devices {
			if d.Address == address {
				return d, true
			}
		}
		return api.DeviceStatus{}, false
	}
	st, _ := s.Status(ctx)
	if len(st.Devices) != 2 || st.Devices[0].Address != "192.0.2.21" {
		t.Fatalf("devices %+v", st.Devices)
	}
	if d, _ := device("192.0.2.21"); !d.Allowed || d.Open == 0 || d.Received == 0 || d.Seen.IsZero() {
		t.Errorf("laptop %+v", d)
	}
	if d, ok := device(stranger); !ok || d.Allowed || d.Open != 0 || d.Received != 0 {
		t.Errorf("stranger %+v", d)
	}

	cfg, _ := s.LoadConfig(ctx)
	cfg.Devices = append(cfg.Devices, api.Device{Address: stranger})
	if err := s.SaveConfig(ctx, cfg); err != nil {
		t.Fatal(err)
	}
	s.Simulate(time.Second)
	if d, _ := device(stranger); !d.Allowed || d.Received == 0 {
		t.Errorf("stranger once listed %+v", d)
	}

	cfg.Devices = cfg.Devices[1:]
	if err := s.SaveConfig(ctx, cfg); err != nil {
		t.Fatal(err)
	}
	if _, ok := device("192.0.2.21"); ok {
		t.Error("a device no longer listed is still reported")
	}

	cfg.Proxies[0].Shared = false
	cfg.Proxies[1] = api.Proxy{Port: 1081, Upstream: "studio.example.com:1080", Enabled: true}
	cfg.Proxies[2] = api.Proxy{Port: 1082, Scene: "office", Enabled: true}
	if err := s.SaveConfig(ctx, cfg); err != nil {
		t.Fatal(err)
	}
	s.Simulate(time.Second)
	st, _ = s.Status(ctx)
	if st.Devices != nil {
		t.Errorf("devices with nothing shared: %+v", st.Devices)
	}
	if p := st.Proxies[1]; !p.Listening || p.Error != nil || p.Received == 0 {
		t.Errorf("a proxy through another proxy: %+v", p)
	}
	if p := st.Proxies[2]; !p.Listening || p.Error != nil || p.Received == 0 {
		t.Errorf("a proxy by a scene's rules: %+v", p)
	}
	cfg.Proxies[2].Scene = "gone"
	if err := s.SaveConfig(ctx, cfg); api.Code(err) != api.ErrInvalidConfig {
		t.Errorf("a proxy by the rules of a scene that doesn't exist: %v", err)
	}
}
