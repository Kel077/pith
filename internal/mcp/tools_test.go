package mcp

import (
	"context"
	"encoding/json"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/Kel077/pith/internal/api"
	"github.com/Kel077/pith/internal/mock"
)

// mockHost is a Host on the mock backend alone: it edits what is saved.
type mockHost struct{ *mock.Service }

// Interfaces adds what a real Mac has beside the mock's: links that aren't
// connected and that macOS gives no name, and a VPN's tunnel that is.
func (h mockHost) Interfaces(ctx context.Context) ([]api.Interface, error) {
	ifaces, err := h.Service.Interfaces(ctx)
	return append(ifaces,
		api.Interface{Name: "en5", Label: "en5", Kind: api.KindEthernet},
		api.Interface{Name: "utun3", Label: "utun3", Kind: api.KindVPN},
		api.Interface{Name: "utun4", Label: "Example VPN", Kind: api.KindVPN},
		api.Interface{Name: "utun9", Label: "Example VPN", Kind: api.KindVPN, Up: true, IPv4: []string{"198.51.100.9"}},
	), err
}

func (h mockHost) Edit(ctx context.Context, fn func(*api.Config) error) (api.Config, error) {
	cfg, err := h.LoadConfig(ctx)
	if err != nil {
		return api.Config{}, err
	}
	if err := fn(&cfg); err != nil {
		return api.Config{}, err
	}
	return cfg, h.SaveConfig(ctx, cfg)
}

func (mockHost) Apps(context.Context) ([]api.AppRef, error) {
	return []api.AppRef{
		{Path: "/Applications/Google Chrome.app", BundleID: "com.google.Chrome", Name: "Google Chrome"},
		{Path: "/Applications/Safari.app", BundleID: "com.apple.Safari", Name: "Safari"},
		{Path: "/Applications/Safari Technology Preview.app", BundleID: "com.apple.SafariTechnologyPreview", Name: "Safari Technology Preview"},
		{Path: "/Applications/Notes.app", BundleID: "com.example.notes", Name: "Notes"},
		{Path: "/System/Applications/Notes.app", BundleID: "com.apple.Notes", Name: "Notes"},
	}, nil
}

// newHost is the mock with its sample configuration, routing its "home"
// scene: interfaces en0 (Ethernet, behind a router), en7 (iPhone USB), en9
// (M2000, a hotspot) and en1 (Wi-Fi, down).
func newHost(t *testing.T, o mock.Options) mockHost {
	t.Helper()
	o.Sample, o.HelperInstalled, o.Seed = true, true, 7
	svc := mock.New(o)
	t.Cleanup(svc.Close)
	return mockHost{svc}
}

// run calls a tool and decodes what it answers into out, when out isn't nil.
func run(t *testing.T, h Host, name, arguments string, out any) {
	t.Helper()
	res := Tools{Host: h}.Call(context.Background(), name, json.RawMessage(arguments))
	if res.Failed {
		t.Fatalf("%s %s failed: %s", name, arguments, res.Text)
	}
	if out != nil {
		reflect.ValueOf(out).Elem().SetZero() // nothing of an earlier answer stays
		if err := json.Unmarshal([]byte(res.Text), out); err != nil {
			t.Fatalf("%s answered %s: %v", name, res.Text, err)
		}
	}
}

// fails calls a tool that must fail, with every one of the words in what
// it says.
func fails(t *testing.T, h Host, name, arguments string, words ...string) {
	t.Helper()
	res := Tools{Host: h}.Call(context.Background(), name, json.RawMessage(arguments))
	if !res.Failed {
		t.Fatalf("%s %s didn't fail: %s", name, arguments, res.Text)
	}
	for _, w := range words {
		if !strings.Contains(res.Text, w) {
			t.Errorf("%s %s said %q, without %q", name, arguments, res.Text, w)
		}
	}
}

func config(t *testing.T, h Host) api.Config {
	t.Helper()
	cfg, err := h.LoadConfig(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	return cfg
}

func scene(t *testing.T, h Host, id string) api.Scene {
	t.Helper()
	cfg := config(t, h)
	s, err := findScene(&cfg, id)
	if err != nil {
		t.Fatal(err)
	}
	return *s
}

func ruleIDs(s api.Scene) []string {
	var ids []string
	for _, r := range s.Rules {
		ids = append(ids, r.ID)
	}
	return ids
}

type ruleAnswer struct {
	Scene    string   `json:"scene"`
	Position int      `json:"position"`
	Rule     api.Rule `json:"rule"`
}

func TestAddRule(t *testing.T) {
	h := newHost(t, mock.Options{})
	var got ruleAnswer
	// Names as a person says them: an app by its name, a URL for its host,
	// interfaces by their labels, whatever the case.
	run(t, h, "add_rule", `{"apps": ["safari"], "targets": ["https://Example.com/path", "*.example.org"], "via": ["m2000", "en0"], "note": "work"}`, &got)
	want := api.Rule{
		ID: got.Rule.ID, Enabled: true, Note: "work", Protocol: api.AnyProtocol, Action: api.Route,
		Apps:    []api.AppRef{{Path: "/Applications/Safari.app", BundleID: "com.apple.Safari", Name: "Safari"}},
		Targets: []string{"example.com", "*.example.org"},
		Via: []api.InterfaceRef{
			{Name: "en9", Label: "M2000", HardwareAddr: "02:00:5e:10:00:09"},
			{Name: "en0", Label: "Ethernet", HardwareAddr: "02:00:5e:10:00:01"},
		},
	}
	if got.Scene != "home" || got.Position != 1 || got.Rule.ID == "" {
		t.Errorf("added %+v, want it first in the selected scene", got)
	}
	saved := scene(t, h, "home")
	if len(saved.Rules) != 6 || !equalJSON(saved.Rules[0], want) || !equalJSON(got.Rule, want) {
		t.Errorf("saved %+v\nanswered %+v\nwant %+v", saved.Rules[0], got.Rule, want)
	}

	// A block rule in another scene, by the scene's name, at a place.
	run(t, h, "add_rule", `{"scene": "office", "action": "block", "targets": ["203.0.113.0/24"], "protocol": "udp", "position": 2, "via": ["en0"], "fallback": true}`, &got)
	office := scene(t, h, "office")
	if got.Scene != "office" || got.Position != 2 || office.Rules[1].ID != got.Rule.ID {
		t.Errorf("added %+v to %v", got, ruleIDs(office))
	}
	if r := office.Rules[1]; r.Action != api.Block || r.Protocol != api.UDP || len(r.Via) != 0 || r.Fallback {
		t.Errorf("a block rule keeps no interfaces: %+v", r)
	}
	// Past the end is the end.
	run(t, h, "add_rule", `{"scene": "office", "via": ["Ethernet"], "position": 99}`, &got)
	if office = scene(t, h, "office"); got.Position != 4 || office.Rules[3].ID != got.Rule.ID {
		t.Errorf("added %+v to %v", got, ruleIDs(office))
	}
}

func equalJSON(a, b any) bool {
	x, _ := json.Marshal(a)
	y, _ := json.Marshal(b)
	return string(x) == string(y)
}

func TestRuleMistakes(t *testing.T) {
	h := newHost(t, mock.Options{})
	before := config(t, h)
	fails(t, h, "add_rule", `{"targets": ["example.com"]}`, "via")
	fails(t, h, "add_rule", `{"targets": ["exa mple"], "via": ["en0"]}`, "exa mple")
	fails(t, h, "add_rule", `{"via": ["en8"]}`, "en8", "en0 (Ethernet)", "en9 (M2000)")
	fails(t, h, "add_rule", `{"apps": ["Nonesuch"], "via": ["en0"]}`, "Nonesuch", "list_apps")
	fails(t, h, "add_rule", `{"apps": ["Notes"], "via": ["en0"]}`, "/Applications/Notes.app", "/System/Applications/Notes.app")
	fails(t, h, "add_rule", `{"via": ["en0", "Ethernet"]}`, "via", "twice")
	fails(t, h, "add_rule", `{"action": "drop", "via": ["en0"]}`, "route or block")
	fails(t, h, "add_rule", `{"scene": "Holiday", "via": ["en0"]}`, "Holiday", "home", "office")
	fails(t, h, "add_rule", `{"target": ["example.com"], "via": ["en0"]}`, `"target"`) // a misspelled argument
	fails(t, h, "update_rule", `{"id": "r-none", "enabled": false}`, "r-none")
	fails(t, h, "update_rule", `{"id": "r-nas", "via": []}`, "via")
	fails(t, h, "remove_rule", `{"id": ""}`, "get_config")
	if after := config(t, h); !equalJSON(before, after) {
		t.Errorf("a refused change changed the configuration:\n%+v\n%+v", before, after)
	}
}

func TestAppsByPath(t *testing.T) {
	h := newHost(t, mock.Options{})
	var got ruleAnswer
	// A tool by its executable, and an installed app by its path, which
	// brings its bundle ID.
	run(t, h, "add_rule", `{"apps": ["/usr/bin/curl", "/Applications/Google Chrome.app/"], "via": ["en0"]}`, &got)
	want := []api.AppRef{
		{Path: "/usr/bin/curl", Name: "curl"},
		{Path: "/Applications/Google Chrome.app", BundleID: "com.google.Chrome", Name: "Google Chrome"},
	}
	if !equalJSON(got.Rule.Apps, want) {
		t.Errorf("apps %+v, want %+v", got.Rule.Apps, want)
	}
	run(t, h, "update_rule", `{"id": "`+got.Rule.ID+`", "apps": ["com.apple.safari"]}`, &got)
	if len(got.Rule.Apps) != 1 || got.Rule.Apps[0].Path != "/Applications/Safari.app" {
		t.Errorf("by bundle ID: %+v", got.Rule.Apps)
	}
}

func TestUpdateRule(t *testing.T) {
	h := newHost(t, mock.Options{})
	var got ruleAnswer
	// Only what is given changes.
	run(t, h, "update_rule", `{"id": "r-nas", "enabled": false, "note": ""}`, &got)
	r := scene(t, h, "home").Rules[1]
	if r.Enabled || r.Note != "" || !slices.Equal(r.Targets, []string{"192.168.50.10"}) || len(r.Via) != 1 || got.Position != 2 {
		t.Errorf("updated to %+v at %d", r, got.Position)
	}
	// A list given replaces the list; empty, it matches everything.
	run(t, h, "update_rule", `{"id": "r-video", "apps": [], "targets": ["example.net"], "via": ["iPhone USB"], "fallback": false, "protocol": "tcp"}`, &got)
	if r := got.Rule; len(r.Apps) != 0 || !slices.Equal(r.Targets, []string{"example.net"}) || len(r.Via) != 1 || r.Via[0].Name != "en7" || r.Fallback || r.Protocol != api.TCP {
		t.Errorf("updated to %+v", r)
	}
	// Moving it.
	run(t, h, "update_rule", `{"id": "r-music", "position": 1}`, &got)
	if ids := ruleIDs(scene(t, h, "home")); got.Position != 1 || !slices.Equal(ids, []string{"r-music", "r-wechat", "r-nas", "r-video", "r-ads"}) {
		t.Errorf("moved to %d: %v", got.Position, ids)
	}
	run(t, h, "update_rule", `{"id": "r-music", "position": 4}`, &got)
	if ids := ruleIDs(scene(t, h, "home")); got.Position != 4 || !slices.Equal(ids, []string{"r-wechat", "r-nas", "r-video", "r-music", "r-ads"}) {
		t.Errorf("moved to %d: %v", got.Position, ids)
	}
	// A rule of a scene that isn't selected, found by its ID alone.
	run(t, h, "update_rule", `{"id": "r-slack", "action": "block"}`, &got)
	if got.Scene != "office" || got.Rule.Action != api.Block || len(got.Rule.Via) != 0 {
		t.Errorf("updated %+v", got)
	}

	var removed struct {
		Scene   string   `json:"scene"`
		Removed api.Rule `json:"removed"`
	}
	run(t, h, "remove_rule", `{"id": "r-ads"}`, &removed)
	if ids := ruleIDs(scene(t, h, "home")); removed.Removed.ID != "r-ads" || removed.Scene != "home" || slices.Contains(ids, "r-ads") {
		t.Errorf("removed %+v, left %v", removed, ids)
	}
}

func TestScenes(t *testing.T) {
	h := newHost(t, mock.Options{Running: true})
	var added sceneView
	run(t, h, "add_scene", `{"name": " Travel ", "copy_of": "Home"}`, &added)
	home, travel := scene(t, h, "home"), scene(t, h, added.ID)
	if added.Name != "Travel" || added.Rules != len(home.Rules) || len(travel.Rules) != len(home.Rules) {
		t.Fatalf("added %+v", added)
	}
	for i, r := range travel.Rules {
		if r.ID == home.Rules[i].ID || !equalJSON(r.Targets, home.Rules[i].Targets) {
			t.Errorf("rule %d of the copy: %+v", i, r)
		}
	}
	run(t, h, "rename_scene", `{"scene": "travel", "name": "Abroad"}`, &added)
	if scene(t, h, added.ID).Name != "Abroad" {
		t.Errorf("renamed to %+v", added)
	}
	run(t, h, "add_scene", `{"name": "Empty"}`, &added)
	if s := scene(t, h, added.ID); len(s.Rules) != 0 || s.Name != "Empty" {
		t.Errorf("added %+v", s)
	}

	// A local proxy goes out by the scene that is removed, which routes.
	run(t, h, "set_proxy", `{"port": 1090, "scene": "Home"}`, nil)
	var removed struct {
		Removed  sceneView `json:"removed"`
		Selected string    `json:"selected"`
	}
	run(t, h, "remove_scene", `{"scene": "home"}`, &removed)
	cfg := config(t, h)
	st, _ := h.Status(context.Background())
	if removed.Removed.ID != "home" || removed.Selected != "office" || cfg.Selected != "office" || len(cfg.Scenes) != 3 {
		t.Errorf("removed %+v, left %+v", removed, cfg.Scenes)
	}
	if st.State != api.Stopped {
		t.Errorf("removing the scene that routes left routing %s", st.State)
	}
	if p := cfg.Proxies[len(cfg.Proxies)-1]; p.Port != 1090 || p.Scene != "office" {
		t.Errorf("the proxy by its rules: %+v", p)
	}
	run(t, h, "remove_scene", `{"scene": "Abroad"}`, nil)
	run(t, h, "remove_scene", `{"scene": "Empty"}`, nil)
	fails(t, h, "remove_scene", `{"scene": "office"}`, "one scene at least")
	fails(t, h, "add_scene", `{"name": "Copy", "copy_of": "home"}`, "home")
}

func TestRouting(t *testing.T) {
	h := newHost(t, mock.Options{})
	var got routingView
	// Left out, the selected scene.
	run(t, h, "start_routing", `{}`, &got)
	if got.State != api.Running || got.SceneID != "home" || got.Scene != "Home" {
		t.Errorf("started %+v", got)
	}
	run(t, h, "start_routing", `{"scene": "Office"}`, &got)
	if got.State != api.Running || got.SceneID != "office" || got.Scene != "Office" {
		t.Errorf("started %+v", got)
	}
	var st api.Status
	run(t, h, "status", ``, &st)
	if st.State != api.Running || st.SceneID != "office" || !st.Helper.Installed || len(st.Proxies) != 3 {
		t.Errorf("status %+v", st)
	}
	run(t, h, "stop_routing", `null`, &got)
	if got.State != api.Stopped || got.SceneID != "" {
		t.Errorf("stopped %+v", got)
	}

	// Without the helper, the answer says whose move it is.
	svc := mock.New(mock.Options{Sample: true})
	defer svc.Close()
	fails(t, mockHost{svc}, "start_routing", `{}`, "helper_missing", "The user installs it")
}

func TestCheck(t *testing.T) {
	h := newHost(t, mock.Options{})
	var got struct {
		Scene     string          `json:"scene"`
		Outcome   api.Outcome     `json:"outcome"`
		Interface string          `json:"interface"`
		Rule      *api.Rule       `json:"rule"`
		Matching  []api.RuleCheck `json:"matching"`
	}
	run(t, h, "check", `{"target": "https://ads.doubleclick.net/x", "app": "Safari"}`, &got)
	if got.Outcome != api.Blocked || got.Rule == nil || got.Rule.ID != "r-ads" || got.Scene != "home" {
		t.Errorf("check: %+v", got)
	}
	run(t, h, "check", `{"target": "192.168.50.10", "protocol": "udp"}`, &got)
	if got.Outcome != api.Routed || got.Interface != "en0" || got.Rule == nil || got.Rule.ID != "r-nas" {
		t.Errorf("check: %+v", got)
	}
	for _, m := range got.Matching {
		if m.Result != api.CheckMatched && m.Result != api.CheckShadowed {
			t.Errorf("a rule that doesn't match is told: %+v", m)
		}
	}
	run(t, h, "check", `{"target": "example.com", "scene": "office"}`, &got)
	if got.Outcome != api.System || got.Rule != nil || len(got.Matching) != 0 || got.Scene != "office" {
		t.Errorf("check: %+v", got)
	}
	fails(t, h, "check", `{}`, "target")
}

func TestReading(t *testing.T) {
	h := newHost(t, mock.Options{Running: true})
	h.Simulate(70e9)

	var ifaces []interfaceView
	run(t, h, "list_interfaces", ``, &ifaces)
	// Wi-Fi, though down, has a name of its own; of the rest that are down
	// or unnamed, only the VPN that is connected shows.
	var names []string
	for _, i := range ifaces {
		names = append(names, i.Name)
	}
	if !slices.Equal(names, []string{"en0", "en7", "en9", "en1", "utun9"}) || !ifaces[0].CanProxy || !ifaces[0].Primary || ifaces[4].CanProxy {
		t.Errorf("interfaces: %v", names)
	}
	// One that isn't listed still goes by its BSD name.
	// One that isn't listed still goes by its BSD name: it is found, and
	// leads to nothing Pith knows.
	fails(t, h, "gateway_status", `{"interface": "en5"}`, "unsupported", "en5 leads to no router")
	var p probeView
	fails(t, h, "set_proxy", `{"port": 1090, "via": "utun9"}`, "utun9", "VPN")

	var apps struct {
		Apps []api.AppRef `json:"apps"`
	}
	run(t, h, "list_apps", `{"search": "SAFARI"}`, &apps)
	if len(apps.Apps) != 2 {
		t.Errorf("apps: %+v", apps)
	}

	var conns []connectionView
	run(t, h, "list_connections", `{"limit": 5}`, &conns)
	if len(conns) != 5 || conns[0].App == "" || conns[0].Remote == "" {
		t.Errorf("connections: %+v", conns)
	}
	run(t, h, "list_connections", `{"limit": 100000, "open": true}`, &conns)
	for _, c := range conns {
		if !c.Ended.IsZero() {
			t.Errorf("an ended connection among the open ones: %+v", c)
		}
	}

	var tr api.Traffic
	run(t, h, "traffic", `{"hours": 1}`, &tr)
	if len(tr.Apps) == 0 || len(tr.Interfaces) == 0 || len(tr.Series) != 0 {
		t.Errorf("traffic: %d apps, %d interfaces, %d samples", len(tr.Apps), len(tr.Interfaces), len(tr.Series))
	}

	run(t, h, "test_interface", `{"interface": "Ethernet"}`, &p)
	if !p.OK || p.Interface != "en0" || p.PublicIPv4 == "" || p.LatencyMS <= 0 {
		t.Errorf("test: %+v", p)
	}
	fails(t, h, "test_interface", `{}`, "interface")

	var speed struct {
		OK           bool    `json:"ok"`
		DownloadMbps float64 `json:"downloadMbps"`
	}
	run(t, h, "speed_test", `{"interface": "en0"}`, &speed)
	if !speed.OK || speed.DownloadMbps <= 0 {
		t.Errorf("speed: %+v", speed)
	}

	var gw struct {
		Hotspot *api.Hotspot `json:"hotspot"`
		Router  *api.Router  `json:"router"`
	}
	run(t, h, "gateway_status", `{"interface": "M2000"}`, &gw)
	if gw.Hotspot == nil || gw.Router != nil || gw.Hotspot.Model != "M2000" || gw.Hotspot.Cell == nil {
		t.Errorf("gateway: %+v", gw)
	}
	run(t, h, "gateway_status", `{"interface": "en0"}`, &gw)
	if gw.Router == nil || gw.Hotspot != nil || gw.Router.PublicIPv4 == "" {
		t.Errorf("gateway: %+v", gw)
	}
	fails(t, h, "gateway_status", `{"interface": "en7"}`, "unsupported")
}

// TestAliases: an interface goes by the name its user gave it, before its
// label.
func TestAliases(t *testing.T) {
	h := newHost(t, mock.Options{})
	if _, err := h.Edit(context.Background(), func(cfg *api.Config) error {
		cfg.Aliases = map[string]string{"02:00:5e:10:00:07": "Ethernet"} // the phone, named as en0 is labeled
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	var p probeView
	run(t, h, "test_interface", `{"interface": "ethernet"}`, &p)
	if p.Interface != "en7" {
		t.Errorf("by alias: %+v", p)
	}
	var ifaces []interfaceView
	run(t, h, "list_interfaces", ``, &ifaces)
	if ifaces[1].Name != "en7" || ifaces[1].Alias != "Ethernet" || ifaces[0].Alias != "" {
		t.Errorf("interfaces: %+v", ifaces)
	}
}

func TestProxies(t *testing.T) {
	h := newHost(t, mock.Options{})
	var p api.Proxy
	run(t, h, "set_proxy", `{"port": 1090, "via": "M2000"}`, &p)
	if !p.Enabled || p.Via.Name != "en9" || p.Via.HardwareAddr == "" || p.Shared || p.ChangeIP {
		t.Errorf("added %+v", p)
	}
	// Other things stay as they are; another exit takes the place of the first.
	run(t, h, "set_proxy", `{"port": 1090, "shared": true, "change_ip": true}`, &p)
	if p.Via.Name != "en9" || !p.Shared || !p.ChangeIP || !p.Enabled {
		t.Errorf("changed to %+v", p)
	}
	run(t, h, "set_proxy", `{"port": 1090, "upstream": "192.0.2.7:1080", "enabled": false}`, &p)
	if p.Via != (api.InterfaceRef{}) || p.Upstream != "192.0.2.7:1080" || p.Enabled || !p.Shared {
		t.Errorf("changed to %+v", p)
	}
	cfg := config(t, h)
	if len(cfg.Proxies) != 4 || !equalJSON(cfg.Proxies[3], p) {
		t.Errorf("saved %+v", cfg.Proxies)
	}
	// What the configuration refuses changes nothing: a proxy by a scene's
	// rules has no one address to change.
	fails(t, h, "set_proxy", `{"port": 1090, "scene": "office"}`, "invalid_config")
	fails(t, h, "set_proxy", `{"port": 1091}`, "1091", "via, upstream or scene")
	fails(t, h, "set_proxy", `{"port": 1091, "via": "en0", "scene": "home"}`, "one of")
	fails(t, h, "set_proxy", `{"port": 80, "via": "en0"}`, "invalid_config")
	fails(t, h, "remove_proxy", `{"port": 1091}`, "1091")
	if after := config(t, h); !equalJSON(cfg, after) {
		t.Errorf("a refused change changed the configuration: %+v", after.Proxies)
	}
	run(t, h, "set_proxy", `{"port": 1090, "scene": "office", "change_ip": false}`, &p)
	if p.Scene != "office" || p.Upstream != "" || p.ChangeIP {
		t.Errorf("changed to %+v", p)
	}
	run(t, h, "remove_proxy", `{"port": 1090}`, nil)
	if cfg := config(t, h); len(cfg.Proxies) != 3 {
		t.Errorf("left %+v", cfg.Proxies)
	}
}

func TestDevices(t *testing.T) {
	h := newHost(t, mock.Options{})
	var d api.Device
	run(t, h, "allow_device", `{"address": "198.51.100.0/24", "name": "Office"}`, &d)
	if d.Address != "198.51.100.0/24" || d.Name != "Office" {
		t.Errorf("allowed %+v", d)
	}
	// The one already listed is renamed, not listed twice; without a name,
	// it keeps the one it has.
	run(t, h, "allow_device", `{"address": "192.0.2.21/32", "name": "Laptop"}`, &d)
	run(t, h, "allow_device", `{"address": "192.0.2.21"}`, &d)
	cfg := config(t, h)
	if len(cfg.Devices) != 2 || cfg.Devices[0].Name != "Laptop" || d.Name != "Laptop" {
		t.Errorf("devices %+v", cfg.Devices)
	}
	fails(t, h, "allow_device", `{"address": "127.0.0.1"}`, "invalid_config")
	fails(t, h, "allow_device", `{"address": "2001:db8::1"}`, "invalid_config")
	fails(t, h, "remove_device", `{"address": "203.0.113.9"}`, "203.0.113.9")
	run(t, h, "remove_device", `{"address": "198.51.100.0/24"}`, nil)
	if cfg := config(t, h); len(cfg.Devices) != 1 || cfg.Devices[0].Address != "192.0.2.21" {
		t.Errorf("devices %+v", cfg.Devices)
	}
}

func TestChangeIP(t *testing.T) {
	h := newHost(t, mock.Options{})
	var c ipChangeView
	run(t, h, "change_ip", `{"interface": "M2000"}`, &c)
	if c.Device != "hotspot" || c.Interface != "en9" || !c.Online || !c.Changed || c.BeforeIPv4 == "" || c.PublicIPv4 == c.BeforeIPv4 || c.Seconds != 14 {
		t.Errorf("hotspot: %+v", c)
	}
	run(t, h, "change_ip", `{"interface": "en0"}`, &c)
	if c.Device != "router" || !c.Online || !c.Changed || c.Standby != "en9" || c.Tries != 2 {
		t.Errorf("router: %+v", c)
	}
	run(t, h, "change_ip", `{"port": 1081}`, &c)
	if c.Port != 1081 || !c.Online || !c.Changed || c.BeforeIPv4 == "" {
		t.Errorf("port: %+v", c)
	}
	fails(t, h, "change_ip", `{"interface": "en7"}`, "unsupported")
	fails(t, h, "change_ip", `{"interface": "en0", "port": 1080}`, "not both")
	fails(t, h, "change_ip", `{}`, "interface")
	fails(t, h, "change_ip", `{"port": 1099}`, "unsupported")

	// Without the device's password, the answer says whose move it is.
	h.LockHotspot()
	fails(t, h, "change_ip", `{"interface": "en9"}`, "wrong_password", "Network page")
}

func TestIPChanged(t *testing.T) {
	const (
		old4, new4 = "203.0.113.10", "203.0.113.20"
		old6, new6 = "2001:db8::10", "2001:db8::20"
	)
	for _, c := range []struct {
		name           string
		b4, b6, a4, a6 string
		want           bool
	}{
		{"both new", old4, old6, new4, new6, true},
		{"IPv4 new, IPv6 kept", old4, old6, new4, old6, true},
		{"IPv6 new, IPv4 kept", old4, old6, old4, new6, true},
		{"both kept", old4, old6, old4, old6, false},
		{"IPv4 missed after, IPv6 kept", old4, old6, "", old6, false},
		{"IPv4 missed after, IPv6 new", old4, old6, "", new6, true},
		{"nothing before", "", "", new4, new6, false},
		{"IPv4 only, kept", old4, "", old4, "", false},
		{"IPv6 appeared, IPv4 kept", old4, "", old4, new6, false},
	} {
		if got := ipChanged(c.b4, c.b6, c.a4, c.a6); got != c.want {
			t.Errorf("%s: changed = %v, want %v", c.name, got, c.want)
		}
	}
	// A renewal whose test after found the IPv6 address alone, unchanged.
	v := viewRenewal(ipChangeView{}, api.Probe{OK: true, PublicIPv4: old4, PublicIPv6: old6}, api.Probe{OK: true, PublicIPv6: old6}, 0)
	if v.Changed || !v.Online || v.BeforeIPv4 != old4 || v.PublicIPv4 != "" {
		t.Fatalf("renewal = %+v", v)
	}
}
