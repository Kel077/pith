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
