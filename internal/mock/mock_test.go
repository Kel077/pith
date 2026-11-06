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
