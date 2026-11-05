// Package mock implements api.Service in memory, for building and testing
// the interface without the helper: Ethernet and an iPhone up, Wi-Fi off,
// and, while a scene runs, connections from a handful of apps routed by its
// rules. Addresses are made up, from private and documentation ranges.
package mock

import (
	"context"
	"fmt"
	"hash/fnv"
	"math/rand/v2"
	"net/netip"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/Kel077/pith/internal/api"
)

// Options say how the mock starts.
type Options struct {
	Sample          bool // a configuration with scenes and rules, rather than DefaultConfig
	HelperInstalled bool
	Running         bool      // routing the selected scene from the start (needs HelperInstalled)
	Seed            uint64    // of the simulation's randomness
	Now             time.Time // the clock's start; zero: the real clock
	Live            bool      // simulate on a ticker; off, only Simulate advances it
}

// Service is the mock backend.
type Service struct {
	mu     sync.Mutex
	cfg    api.Config
	status api.Status
	ifaces []api.Interface
	conns  []*api.Connection // oldest first
	open   map[uint64]*flow
	nextID uint64
	subs   map[chan api.Event]struct{}
	rnd    *rand.Rand
	clock  time.Time // simulated time when !live
	live   bool
	ticker *time.Ticker
	done   chan struct{}
	since  time.Time
	series []api.TrafficSample
}

// flow is an open connection's plan: how long it lasts and how fast it goes.
type flow struct {
	c           *api.Connection
	ends        time.Time
	upRate      uint64
	downRate    uint64
	lastTouched time.Time
}

// New returns a mock backend.
func New(o Options) *Service {
	s := &Service{
		cfg:    api.DefaultConfig(),
		open:   map[uint64]*flow{},
		subs:   map[chan api.Event]struct{}{},
		rnd:    rand.New(rand.NewPCG(o.Seed, o.Seed^0x9e3779b97f4a7c15)),
		clock:  o.Now,
		live:   o.Live,
		ifaces: sampleInterfaces(),
	}
	if s.clock.IsZero() && !o.Live {
		s.clock = time.Date(2026, 11, 4, 9, 30, 0, 0, time.Local)
	}
	if o.Sample {
		s.cfg = SampleConfig()
	}
	s.status = api.Status{State: api.Stopped, Helper: api.HelperStatus{Installed: o.HelperInstalled, Running: o.HelperInstalled, Version: "0.1.0"}}
	if o.Running && o.HelperInstalled {
		s.startLocked(s.cfg.Selected)
	}
	if o.Live {
		s.ticker = time.NewTicker(250 * time.Millisecond)
		s.done = make(chan struct{})
		go s.loop()
	}
	return s
}

// Close stops the simulation's ticker.
func (s *Service) Close() {
	if s.ticker != nil {
		s.ticker.Stop()
		close(s.done)
	}
}

func (s *Service) now() time.Time {
	if s.live {
		return time.Now()
	}
	return s.clock
}

func (s *Service) loop() {
	last := time.Now()
	for {
		select {
		case <-s.done:
			return
		case t := <-s.ticker.C:
			s.mu.Lock()
			s.stepLocked(t.Sub(last))
			last = t
			s.mu.Unlock()
		}
	}
}

// Simulate advances the simulated clock by d in steps of a quarter second,
// opening, updating and closing connections as time passes.
func (s *Service) Simulate(d time.Duration) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for d > 0 {
		step := min(d, 250*time.Millisecond)
		s.clock = s.clock.Add(step)
		s.stepLocked(step)
		d -= step
	}
}

// ---- Service ----------------------------------------------------------------

func (s *Service) LoadConfig(ctx context.Context) (api.Config, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return clone(s.cfg), nil
}

func (s *Service) SaveConfig(ctx context.Context, cfg api.Config) error {
	if err := cfg.Validate(); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.cfg = clone(cfg)
	if s.status.State == api.Running && s.scene(s.status.SceneID) == nil {
		s.stopLocked()
	}
	s.refreshWarningsLocked()
	return nil
}

func (s *Service) Status(ctx context.Context) (api.Status, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.status, nil
}

func (s *Service) InstallHelper(ctx context.Context) error {
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-time.After(600 * time.Millisecond):
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.status.Helper = api.HelperStatus{Installed: true, Running: true, Version: "0.1.0"}
	s.publishStatusLocked()
	return nil
}

func (s *Service) UninstallHelper(ctx context.Context) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.stopLocked()
	s.status.Helper = api.HelperStatus{}
	s.publishStatusLocked()
	return nil
}

func (s *Service) Start(ctx context.Context, sceneID string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	switch {
	case !s.status.Helper.Installed:
		return &api.Error{Code: api.ErrHelperMissing}
	case s.scene(sceneID) == nil:
		return &api.Error{Code: api.ErrNoScene, Detail: sceneID}
	}
	s.startLocked(sceneID)
	return nil
}

func (s *Service) Stop(ctx context.Context) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.stopLocked()
	return nil
}

func (s *Service) Subscribe(ctx context.Context) (<-chan api.Event, error) {
	ch := make(chan api.Event, 256)
	s.mu.Lock()
	st, ifs := s.status, slices.Clone(s.ifaces)
	ch <- api.Event{Status: &st}
	ch <- api.Event{Interfaces: ifs}
	s.subs[ch] = struct{}{}
	s.mu.Unlock()
	go func() {
		<-ctx.Done()
		s.mu.Lock()
		delete(s.subs, ch)
		close(ch)
		s.mu.Unlock()
	}()
	return ch, nil
}

func (s *Service) Interfaces(ctx context.Context) ([]api.Interface, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return slices.Clone(s.ifaces), nil
}

func (s *Service) Probe(ctx context.Context, iface, target string) (api.Probe, error) {
	s.mu.Lock()
	i := s.iface(iface)
	var p api.Probe
	switch {
	case i == nil:
		s.mu.Unlock()
		return api.Probe{}, &api.Error{Code: api.ErrInternal, Detail: "no interface " + iface}
	case !i.Up:
		p = api.Probe{Interface: iface, Target: target, At: s.now(), Problem: "interface is down"}
	default:
		base := 18 * time.Millisecond
		if i.Kind == api.KindCellular {
			base = 46 * time.Millisecond
		}
		lat := base + time.Duration(s.rnd.IntN(25))*time.Millisecond
		p = api.Probe{Interface: iface, Target: target, At: s.now(), OK: true, Latency: lat}
		if target == "" {
			if i.Kind == api.KindCellular {
				p.PublicIPv4, p.PublicIPv6 = "198.51.100.77", "2001:db8:77::1"
			} else {
				p.PublicIPv4, p.PublicIPv6 = "203.0.113.24", "2001:db8:24::1"
			}
		} else {
			host, port, _ := strings.Cut(target, ":")
			if port == "" {
				port = "443"
			}
			p.Address = fakeAddr(host).String() + ":" + port
		}
	}
	s.mu.Unlock()
	select { // a test takes a moment
	case <-ctx.Done():
		return api.Probe{}, ctx.Err()
	case <-time.After(p.Latency + 300*time.Millisecond):
	}
	return p, nil
}

func (s *Service) Connections(ctx context.Context, q api.ConnectionQuery) ([]api.Connection, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	limit := q.Limit
	if limit <= 0 {
		limit = 500
	}
	search := strings.ToLower(q.Search)
	var out []api.Connection
	for i := len(s.conns) - 1; i >= 0 && len(out) < limit; i-- {
		c := s.conns[i]
		switch {
		case !q.Since.IsZero() && c.Started.Before(q.Since),
			q.Open && !c.Ended.IsZero(),
			q.App != "" && c.App.Path != q.App,
			search != "" && !strings.Contains(strings.ToLower(c.App.Name+" "+c.Domain+" "+c.Remote), search):
			continue
		}
		out = append(out, *c)
	}
	return out, nil
}

func (s *Service) ClearConnections(ctx context.Context) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	kept := s.conns[:0]
	for _, c := range s.conns {
		if c.Ended.IsZero() {
			kept = append(kept, c)
		}
	}
	s.conns = kept
	return nil
}

func (s *Service) Traffic(ctx context.Context, q api.TrafficQuery) (api.Traffic, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	t := api.Traffic{Since: q.Since}
	apps := map[string]*api.AppTraffic{}
	ifs := map[string]*api.InterfaceTraffic{}
	for _, c := range s.conns {
		if c.Started.Before(q.Since) && !c.Ended.IsZero() && c.Ended.Before(q.Since) {
			continue
		}
		a := apps[c.App.Path]
		if a == nil {
			a = &api.AppTraffic{App: c.App}
			apps[c.App.Path] = a
		}
		a.Sent += c.Sent
		a.Received += c.Received
		a.Connections++
		if c.Interface != "" {
			f := ifs[c.Interface]
			if f == nil {
				f = &api.InterfaceTraffic{Interface: c.Interface}
				ifs[c.Interface] = f
			}
			f.Sent += c.Sent
			f.Received += c.Received
		}
	}
	for _, a := range apps {
		t.Apps = append(t.Apps, *a)
	}
	slices.SortFunc(t.Apps, func(a, b api.AppTraffic) int { return int(int64(b.Sent+b.Received) - int64(a.Sent+a.Received)) })
	for _, f := range ifs {
		t.Interfaces = append(t.Interfaces, *f)
	}
	slices.SortFunc(t.Interfaces, func(a, b api.InterfaceTraffic) int {
		return int(int64(b.Sent+b.Received) - int64(a.Sent+a.Received))
	})
	if q.Step > 0 {
		for _, p := range s.series {
			if !p.At.Before(q.Since) {
				t.Series = append(t.Series, p)
			}
		}
	}
	return t, nil
}

func (s *Service) Check(ctx context.Context, in api.CheckInput) (api.CheckResult, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	id := in.SceneID
	if id == "" {
		id = s.cfg.Selected
	}
	sc := s.scene(id)
	if sc == nil {
		return api.CheckResult{}, &api.Error{Code: api.ErrNoScene, Detail: id}
	}
	t, err := api.ParseTarget(in.Target)
	if err != nil {
		return api.CheckResult{}, &api.Error{Code: api.ErrInvalidConfig, Detail: "target: " + err.Error()}
	}
	proto := in.Protocol
	if proto == api.AnyProtocol || proto == "" {
		proto = api.TCP
	}
	host := t.Domain
	addr := t.Prefix.Addr()
	var res api.CheckResult
	if in.Address != "" {
		addr, _ = netip.ParseAddr(in.Address)
	} else if host != "" {
		addr = fakeAddr(host)
		res.Addresses = []string{addr.String()}
	}
	rule, checks := match(sc.Rules, in.App, host, addr, proto)
	res.Rules = checks
	res.Outcome, res.Interface = s.decide(rule)
	if rule != nil {
		res.RuleID = rule.ID
	}
	return res, nil
}

// ---- simulation -------------------------------------------------------------

func (s *Service) startLocked(sceneID string) {
	s.cfg.Selected = sceneID
	s.status.State = api.Running
	s.status.SceneID = sceneID
	s.status.Since = s.now()
	s.status.Error = nil
	s.status.Sent, s.status.Received = 0, 0
	s.since = s.now()
	s.refreshWarningsLocked()
	s.publishStatusLocked()
}

func (s *Service) stopLocked() {
	if s.status.State != api.Running {
		return
	}
	now := s.now()
	for id, f := range s.open {
		f.c.Ended = now
		s.publishConnLocked(f.c)
		delete(s.open, id)
	}
	s.status = api.Status{Helper: s.status.Helper, State: api.Stopped}
	s.publishStatusLocked()
}

func (s *Service) stepLocked(d time.Duration) {
	if s.status.State != api.Running || d <= 0 {
		return
	}
	now := s.now()
	sc := s.scene(s.status.SceneID)
	// New connections: about three a second.
	for n := s.rnd.IntN(3); sc != nil && n > 0; n-- {
		s.openLocked(sc, now)
	}
	var sent, recv uint64
	perIface := map[string][2]uint64{}
	for id, f := range s.open {
		secs := d.Seconds()
		up := uint64(float64(f.upRate) * secs * (0.6 + s.rnd.Float64()*0.8))
		down := uint64(float64(f.downRate) * secs * (0.6 + s.rnd.Float64()*0.8))
		f.c.Sent += up
		f.c.Received += down
		sent += up
		recv += down
		v := perIface[f.c.Interface]
		perIface[f.c.Interface] = [2]uint64{v[0] + up, v[1] + down}
		if !now.Before(f.ends) {
			f.c.Ended = now
			delete(s.open, id)
			s.publishConnLocked(f.c)
		} else if now.Sub(f.lastTouched) >= time.Second {
			f.lastTouched = now
			s.publishConnLocked(f.c)
		}
	}
	s.status.Sent += sent
	s.status.Received += recv
	s.status.Open = len(s.open)
	s.status.SendRate = uint64(float64(sent) / d.Seconds())
	s.status.ReceiveRate = uint64(float64(recv) / d.Seconds())
	sec := now.Truncate(time.Second)
	for name, v := range perIface {
		if name == "" {
			continue
		}
		if n := len(s.series); n > 0 && s.series[n-1].At.Equal(sec) && s.series[n-1].Interface == name {
			s.series[n-1].Sent += v[0]
			s.series[n-1].Received += v[1]
			continue
		}
		found := false
		for i := len(s.series) - 1; i >= 0 && s.series[i].At.Equal(sec); i-- {
			if s.series[i].Interface == name {
				s.series[i].Sent += v[0]
				s.series[i].Received += v[1]
				found = true
				break
			}
		}
		if !found {
			s.series = append(s.series, api.TrafficSample{At: sec, Interface: name, Sent: v[0], Received: v[1]})
		}
	}
	if cut := now.Add(-10 * time.Minute); len(s.series) > 0 && s.series[0].At.Before(cut) {
		i := 0
		for i < len(s.series) && s.series[i].At.Before(cut) {
			i++
		}
		s.series = slices.Delete(s.series, 0, i)
	}
	if len(s.conns) > 3000 {
		s.conns = slices.Delete(s.conns, 0, len(s.conns)-3000)
	}
	s.publishStatusLocked()
}

func (s *Service) openLocked(sc *api.Scene, now time.Time) {
	a := sampleApps[s.rnd.IntN(len(sampleApps))]
	host := a.hosts[s.rnd.IntN(len(a.hosts))]
	proto := api.TCP
	port := 443
	if a.udp && s.rnd.IntN(3) == 0 {
		proto = api.UDP
	}
	if a.port != 0 {
		port = a.port
	}
	addr := fakeAddr(host)
	domain := host
	if _, err := netip.ParseAddr(host); err == nil {
		domain = ""
	}
	rule, _ := match(sc.Rules, a.ref, domain, addr, proto)
	outcome, iface := s.decide(rule)
	s.nextID++
	c := &api.Connection{
		ID:        s.nextID,
		Started:   now,
		App:       a.ref,
		Process:   api.Process{PID: 400 + int(hash(a.ref.Path)%9000), Name: a.process, Path: a.exec},
		Protocol:  proto,
		Remote:    netip.AddrPortFrom(addr, uint16(port)).String(),
		Domain:    domain,
		Outcome:   outcome,
		Interface: iface,
	}
	if rule != nil {
		c.RuleID = rule.ID
	}
	life := time.Duration(1+s.rnd.IntN(a.life)) * time.Second
	f := &flow{c: c, ends: now.Add(life), upRate: a.up / 4 * uint64(1+s.rnd.IntN(4)), downRate: a.down / 4 * uint64(1+s.rnd.IntN(4)), lastTouched: now}
	switch outcome {
	case api.Blocked, api.Unreachable:
		c.Ended = now
		if outcome == api.Unreachable {
			c.Problem = "connect: network is unreachable"
		}
	default:
		s.open[c.ID] = f
	}
	s.conns = append(s.conns, c)
	s.publishConnLocked(c)
}

// decide says where a connection a rule decided goes now.
func (s *Service) decide(rule *api.Rule) (api.Outcome, string) {
	primary := ""
	for _, i := range s.ifaces {
		if i.Primary {
			primary = i.Name
		}
	}
	if rule == nil {
		return api.System, primary
	}
	if rule.Action == api.Block {
		return api.Blocked, ""
	}
	for k, v := range rule.Via {
		if i := s.iface(v.Name); i != nil && i.Up {
			if k == 0 {
				return api.Routed, i.Name
			}
			return api.Fallback, i.Name
		}
	}
	if rule.Fallback {
		return api.System, primary
	}
	return api.Unreachable, ""
}

// match finds the rule that decides a connection, and how each compares.
func match(rules []api.Rule, app api.AppRef, host string, addr netip.Addr, proto api.Protocol) (*api.Rule, []api.RuleCheck) {
	var block, route *api.Rule
	checks := make([]api.RuleCheck, len(rules))
	for i := range rules {
		r := &rules[i]
		checks[i].RuleID = r.ID
		switch {
		case !r.Enabled:
			checks[i].Result = api.CheckDisabled
		case !matchApp(r.Apps, app):
			checks[i].Result = api.CheckApp
		case !matchTarget(r.Targets, host, addr):
			checks[i].Result = api.CheckTarget
		case r.Protocol != api.AnyProtocol && r.Protocol != proto:
			checks[i].Result = api.CheckProtocol
		default:
			checks[i].Result = api.CheckShadowed
			if r.Action == api.Block && block == nil {
				block = r
			} else if r.Action == api.Route && route == nil {
				route = r
			}
		}
	}
	decided := block
	if decided == nil {
		decided = route
	}
	for i := range rules {
		if decided != nil && rules[i].ID == decided.ID {
			checks[i].Result = api.CheckMatched
		}
	}
	return decided, checks
}

func matchApp(apps []api.AppRef, app api.AppRef) bool {
	if len(apps) == 0 {
		return true
	}
	for _, a := range apps {
		if a.Path == app.Path || a.BundleID != "" && a.BundleID == app.BundleID {
			return true
		}
	}
	return false
}

func matchTarget(targets []string, host string, addr netip.Addr) bool {
	if len(targets) == 0 {
		return true
	}
	for _, s := range targets {
		t, err := api.ParseTarget(s)
		if err != nil {
			continue
		}
		if t.MatchHost(host) || addr.IsValid() && t.MatchAddr(addr) {
			return true
		}
	}
	return false
}

func (s *Service) refreshWarningsLocked() {
	s.status.Warnings = nil
	if s.status.State != api.Running {
		return
	}
	sc := s.scene(s.status.SceneID)
	if sc == nil {
		return
	}
	warned := map[string]bool{}
	for _, r := range sc.Rules {
		if !r.Enabled || r.Action != api.Route {
			continue
		}
		for _, v := range r.Via {
			if i := s.iface(v.Name); (i == nil || !i.Up) && !warned[v.Name] {
				warned[v.Name] = true
				s.status.Warnings = append(s.status.Warnings, api.Warning{Code: api.WarnInterfaceDown, Detail: v.Name})
			}
		}
	}
}

func (s *Service) publishStatusLocked() {
	st := s.status
	for ch := range s.subs {
		select {
		case ch <- api.Event{Status: &st}:
		default:
		}
	}
}

func (s *Service) publishConnLocked(c *api.Connection) {
	cp := *c
	for ch := range s.subs {
		select {
		case ch <- api.Event{Connection: &cp}:
		default:
		}
	}
}

func (s *Service) scene(id string) *api.Scene {
	for i := range s.cfg.Scenes {
		if s.cfg.Scenes[i].ID == id {
			return &s.cfg.Scenes[i]
		}
	}
	return nil
}

func (s *Service) iface(name string) *api.Interface {
	for i := range s.ifaces {
		if s.ifaces[i].Name == name {
			return &s.ifaces[i]
		}
	}
	return nil
}

// SetInterfaceUp turns a simulated interface on or off.
func (s *Service) SetInterfaceUp(name string, up bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if i := s.iface(name); i != nil {
		i.Up = up
		ifs := slices.Clone(s.ifaces)
		for ch := range s.subs {
			select {
			case ch <- api.Event{Interfaces: ifs}:
			default:
			}
		}
		s.refreshWarningsLocked()
		s.publishStatusLocked()
	}
}

// ---- sample data ------------------------------------------------------------

func sampleInterfaces() []api.Interface {
	return []api.Interface{
		{Name: "en0", Label: "Ethernet", Kind: api.KindEthernet, HardwareAddr: "02:00:5e:10:00:01", Up: true, Primary: true,
			IPv4: []string{"192.168.1.20"}, IPv6: []string{"2001:db8:24::20"}, Router: "192.168.1.1", DNS: []string{"192.168.1.1", "1.1.1.1"}},
		{Name: "en7", Label: "iPhone USB", Kind: api.KindCellular, HardwareAddr: "02:00:5e:10:00:07", Up: true,
			IPv4: []string{"172.20.10.2"}, IPv6: []string{"2001:db8:77::2"}, Router: "172.20.10.1", DNS: []string{"172.20.10.1"}},
		{Name: "en1", Label: "Wi-Fi", Kind: api.KindWiFi, HardwareAddr: "02:00:5e:10:00:02"},
	}
}

// SampleConfig is a configuration with two scenes and a few rules.
func SampleConfig() api.Config {
	c := api.DefaultConfig()
	eth := api.InterfaceRef{Name: "en0", Label: "Ethernet", HardwareAddr: "02:00:5e:10:00:01"}
	phone := api.InterfaceRef{Name: "en7", Label: "iPhone USB", HardwareAddr: "02:00:5e:10:00:07"}
	wifi := api.InterfaceRef{Name: "en1", Label: "Wi-Fi", HardwareAddr: "02:00:5e:10:00:02"}
	c.Scenes = []api.Scene{
		{ID: "home", Name: "日常", Rules: []api.Rule{
			{ID: "r-wechat", Enabled: true, Apps: []api.AppRef{appWeChat}, Protocol: api.AnyProtocol, Action: api.Route, Via: []api.InterfaceRef{phone, eth}, Fallback: true},
			{ID: "r-nas", Enabled: true, Note: "NAS", Targets: []string{"192.168.50.10"}, Protocol: api.AnyProtocol, Action: api.Route, Via: []api.InterfaceRef{eth}},
			{ID: "r-video", Enabled: true, Apps: []api.AppRef{appChrome, appSafari}, Targets: []string{"*.youtube.com", "*.googlevideo.com"}, Protocol: api.AnyProtocol, Action: api.Route, Via: []api.InterfaceRef{eth, wifi}, Fallback: true},
			{ID: "r-ads", Enabled: true, Targets: []string{"*.doubleclick.net", "*.ads.example.com"}, Protocol: api.AnyProtocol, Action: api.Block},
			{ID: "r-music", Enabled: false, Apps: []api.AppRef{appMusic}, Protocol: api.TCP, Action: api.Route, Via: []api.InterfaceRef{wifi}},
		}},
		{ID: "office", Name: "公司", Rules: []api.Rule{
			{ID: "r-intranet", Enabled: true, Targets: []string{"10.0.0.0/8", "*.corp.example.com"}, Protocol: api.AnyProtocol, Action: api.Route, Via: []api.InterfaceRef{eth}},
			{ID: "r-slack", Enabled: true, Apps: []api.AppRef{appSlack}, Protocol: api.AnyProtocol, Action: api.Route, Via: []api.InterfaceRef{phone}, Fallback: true},
		}},
	}
	c.Selected = "home"
	return c
}

var (
	appSafari = api.AppRef{Path: "/Applications/Safari.app", BundleID: "com.apple.Safari", Name: "Safari"}
	appWeChat = api.AppRef{Path: "/Applications/WeChat.app", BundleID: "com.tencent.xinWeChat", Name: "微信"}
	appChrome = api.AppRef{Path: "/Applications/Google Chrome.app", BundleID: "com.google.Chrome", Name: "Google Chrome"}
	appMusic  = api.AppRef{Path: "/System/Applications/Music.app", BundleID: "com.apple.Music", Name: "音乐"}
	appSlack  = api.AppRef{Path: "/Applications/Slack.app", BundleID: "com.tinyspeck.slackmacgap", Name: "Slack"}
	appMail   = api.AppRef{Path: "/System/Applications/Mail.app", BundleID: "com.apple.mail", Name: "邮件"}
	appFinder = api.AppRef{Path: "/System/Library/CoreServices/Finder.app", BundleID: "com.apple.finder", Name: "访达"}
	appCurl   = api.AppRef{Path: "/usr/bin/curl", Name: "curl"}
)

type sampleApp struct {
	ref      api.AppRef
	process  string
	exec     string
	hosts    []string
	udp      bool
	port     int
	up, down uint64 // bytes a second, roughly
	life     int    // seconds, at most
}

var sampleApps = []sampleApp{
	{appSafari, "com.apple.WebKit.Networking", "/System/Library/Frameworks/WebKit.framework/Versions/A/XPCServices/com.apple.WebKit.Networking.xpc/Contents/MacOS/com.apple.WebKit.Networking",
		[]string{"www.apple.com", "developer.apple.com", "news.ycombinator.com", "www.youtube.com", "rr3.googlevideo.com"}, true, 0, 8_000, 900_000, 20},
	{appWeChat, "WeChat", "/Applications/WeChat.app/Contents/MacOS/WeChat",
		[]string{"weixin.qq.com", "szextshort.weixin.qq.com", "mmbiz.qpic.cn", "res.wx.qq.com"}, false, 0, 6_000, 120_000, 30},
	{appChrome, "Google Chrome Helper", "/Applications/Google Chrome.app/Contents/Frameworks/Google Chrome Framework.framework/Helpers/Google Chrome Helper.app/Contents/MacOS/Google Chrome Helper",
		[]string{"www.google.com", "www.youtube.com", "rr5.googlevideo.com", "github.com", "stats.doubleclick.net"}, true, 0, 12_000, 2_400_000, 25},
	{appMusic, "Music", "/System/Applications/Music.app/Contents/MacOS/Music",
		[]string{"amp-api.music.apple.com", "aod.itunes.apple.com"}, false, 0, 2_000, 320_000, 40},
	{appSlack, "Slack Helper", "/Applications/Slack.app/Contents/Frameworks/Slack Helper.app/Contents/MacOS/Slack Helper",
		[]string{"app.slack.com", "edgeapi.slack.com", "files.slack.com"}, false, 0, 4_000, 60_000, 30},
	{appMail, "Mail", "/System/Applications/Mail.app/Contents/MacOS/Mail",
		[]string{"imap.mail.example.com", "smtp.mail.example.com"}, false, 993, 3_000, 40_000, 15},
	{appFinder, "Finder", "/System/Library/CoreServices/Finder.app/Contents/MacOS/Finder",
		[]string{"192.168.50.10"}, false, 445, 400_000, 1_200_000, 12},
	{appCurl, "curl", "/usr/bin/curl",
		[]string{"example.com", "api.ipify.org"}, false, 0, 1_000, 20_000, 3},
}

// fakeAddr makes up a stable public address for a host: from the
// documentation ranges, or the host itself when it is an address.
func fakeAddr(host string) netip.Addr {
	if a, err := netip.ParseAddr(host); err == nil {
		return a
	}
	h := hash(host)
	if h%2 == 0 {
		return netip.AddrFrom4([4]byte{203, 0, 113, byte(1 + h%250)})
	}
	return netip.AddrFrom4([4]byte{198, 51, 100, byte(1 + h%250)})
}

func hash(s string) uint64 {
	f := fnv.New64a()
	f.Write([]byte(s))
	return f.Sum64()
}

func clone(c api.Config) api.Config {
	out := c
	out.Scenes = make([]api.Scene, len(c.Scenes))
	for i, s := range c.Scenes {
		out.Scenes[i] = s
		out.Scenes[i].Rules = make([]api.Rule, len(s.Rules))
		for j, r := range s.Rules {
			r.Apps = slices.Clone(r.Apps)
			r.Targets = slices.Clone(r.Targets)
			r.Via = slices.Clone(r.Via)
			out.Scenes[i].Rules[j] = r
		}
	}
	if c.Aliases != nil {
		out.Aliases = make(map[string]string, len(c.Aliases))
		for k, v := range c.Aliases {
			out.Aliases[k] = v
		}
	}
	return out
}

var _ api.Service = (*Service)(nil)

// String describes the mock for logs.
func (s *Service) String() string { return fmt.Sprintf("mock backend (%d scenes)", len(s.cfg.Scenes)) }
