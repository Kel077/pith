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
