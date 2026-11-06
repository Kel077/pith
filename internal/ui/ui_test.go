package ui

import (
	"bytes"
	"context"
	"flag"
	"hash/fnv"
	"image"
	"image/color"
	"image/png"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/egoist/mygo/i18n"
	"github.com/egoist/mygo/ui"

	"github.com/Kel077/pith/internal/api"
	"github.com/Kel077/pith/internal/mock"
	"github.com/Kel077/pith/locales"
)

var (
	snapshots    = flag.String("snapshots", "", "write PNG snapshots of the views to this directory")
	snapshotLang = flag.String("lang", "en", "the language of the snapshots: en or zh-Hans")
)

// fakeDesktop stands in for the operating system: apps from the mock, and
// icons that are colored squares.
type fakeDesktop struct{ quit bool }

func (fakeDesktop) InstalledApps() ([]api.AppRef, error) {
	return []api.AppRef{
		{Path: "/Applications/Google Chrome.app", BundleID: "com.google.Chrome", Name: "Google Chrome"},
		{Path: "/System/Applications/Mail.app", BundleID: "com.apple.mail", Name: "Mail"},
		{Path: "/System/Applications/Music.app", BundleID: "com.apple.Music", Name: "Music"},
		{Path: "/Applications/Safari.app", BundleID: "com.apple.Safari", Name: "Safari"},
		{Path: "/Applications/Slack.app", BundleID: "com.tinyspeck.slackmacgap", Name: "Slack"},
		{Path: "/Applications/WeChat.app", BundleID: "com.tencent.xinWeChat", Name: "WeChat"},
	}, nil
}

func (fakeDesktop) Icon(path string, size int) ([]byte, error) {
	h := fnv.New32a()
	h.Write([]byte(path))
	v := h.Sum32()
	fill := color.NRGBA{R: uint8(80 + v%150), G: uint8(80 + (v>>8)%150), B: uint8(80 + (v>>16)%150), A: 255}
	img := image.NewNRGBA(image.Rect(0, 0, size, size))
	r := float64(size) * 0.22
	for y := range size {
		for x := range size {
			// A rounded square, inset a little as macOS icons are.
			inset := float64(size) * 0.08
			fx, fy := float64(x)+0.5, float64(y)+0.5
			lo, hi := inset+r, float64(size)-inset-r
			dx := max(lo-fx, 0, fx-hi)
			dy := max(lo-fy, 0, fy-hi)
			if dx*dx+dy*dy <= r*r && fx >= inset && fx <= float64(size)-inset && fy >= inset && fy <= float64(size)-inset {
				img.Set(x, y, fill)
			}
		}
	}
	var b bytes.Buffer
	err := png.Encode(&b, img)
	return b.Bytes(), err
}

func (fakeDesktop) ChooseProgram() (string, error) { return "", nil }
func (fakeDesktop) Save(string, []byte) error      { return nil }
func (fakeDesktop) Open() ([]byte, error)          { return nil, nil }
func (fakeDesktop) OpenURL(string)                 {}
func (fakeDesktop) Reveal(string)                  {}
func (fakeDesktop) SetOpenAtLogin(bool) error      { return nil }
func (fakeDesktop) SetLanguage(tag string)         { i18n.SetLocaleFunc(func() string { return tag }) }
func (d *fakeDesktop) Quit()                       { d.quit = true }
func (fakeDesktop) ReadState(string) string        { return "" }
func (fakeDesktop) WriteState(string, string)      {}

// testApp is an App on a mock that has routed for a minute.
func testApp(t *testing.T, o mock.Options) (*App, *mock.Service) {
	t.Helper()
	o.Seed = 7
	svc := mock.New(o)
	now := time.Date(2026, 11, 4, 9, 30, 0, 0, time.Local)
	if o.Running {
		svc.Simulate(70 * time.Second)
		now = now.Add(70 * time.Second)
	}
	a := New(Options{Service: svc, Desktop: &fakeDesktop{}, Post: func(fn func()) { fn() }, Version: "0.1.0", Now: func() time.Time { return now }, Language: *snapshotLang})
	a.Sync(context.Background())
	return a, svc
}

func save(t *testing.T, img *image.RGBA, name string) {
	t.Helper()
	f, err := os.Create(filepath.Join(*snapshots, name+".png"))
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if err := png.Encode(f, img); err != nil {
		t.Fatal(err)
	}
}

// TestSnapshots renders every page, light and dark, for a person to look at:
// go test ./internal/ui -run Snapshots -snapshots=DIR
func TestSnapshots(t *testing.T) {
	if *snapshots == "" {
		t.Skip("pass -snapshots=DIR")
	}
	if err := os.MkdirAll(*snapshots, 0o755); err != nil {
		t.Fatal(err)
	}
	for _, dark := range []bool{false, true} {
		mode := "light"
		if dark {
			mode = "dark"
		}
		for _, page := range []string{"rules", "activity", "apps", "traffic", "network", "settings"} {
			a, _ := testApp(t, mock.Options{Sample: true, HelperInstalled: true, Running: true})
			a.page = page
			switch page {
			case "apps":
				a.page, a.activity.view = "activity", 1
			case "traffic":
				a.page, a.activity.view, a.activity.span = "activity", 2, 1
			}
			if page == "network" {
				a.probes["en0"] = api.Probe{Interface: "en0", OK: true, Latency: 21 * time.Millisecond, PublicIPv4: "203.0.113.24"}
			}
			tt := ui.NewTester(a.WindowView, 1040, 680)
			tt.SetDark(dark)
			tt.SetScale(2)
			tt.Frame()
			save(t, tt.Image(), page+"-"+mode)
		}
		a, _ := testApp(t, mock.Options{Sample: true, HelperInstalled: true, Running: true})
		tt := ui.NewTester(a.PanelView, PanelWidth, a.PanelHeight())
		tt.SetDark(dark)
		tt.SetScale(2)
		tt.Frame()
		save(t, tt.Image(), "panel-"+mode)

		// A helper Pith can't verify: Settings, and the panel.
		a, _ = testApp(t, mock.Options{Sample: true, HelperInstalled: true, HelperUntrusted: true})
		a.page = "settings"
		tt = ui.NewTester(a.WindowView, 1040, 1240) // down to the helper
		tt.SetDark(dark)
		tt.SetScale(2)
		tt.Frame()
		save(t, tt.Image(), "untrusted-"+mode)
		tt = ui.NewTester(a.PanelView, PanelWidth, a.PanelHeight())
		tt.SetDark(dark)
		tt.SetScale(2)
		tt.Frame()
		save(t, tt.Image(), "untrusted-panel-"+mode)

		// A helper that doesn't answer: Settings, and the panel.
		a, _ = testApp(t, mock.Options{Sample: true, HelperInstalled: true, HelperSilent: true})
		a.page = "settings"
		tt = ui.NewTester(a.WindowView, 1040, 1240)
		tt.SetDark(dark)
		tt.SetScale(2)
		tt.Frame()
		save(t, tt.Image(), "silent-"+mode)
		tt = ui.NewTester(a.PanelView, PanelWidth, a.PanelHeight())
		tt.SetDark(dark)
		tt.SetScale(2)
		tt.Frame()
		save(t, tt.Image(), "silent-panel-"+mode)

		// The rule editor, and the checker.
		a, _ = testApp(t, mock.Options{Sample: true, HelperInstalled: true, Running: true})
		a.openEditor("home", 2)
		tt = ui.NewTester(a.WindowView, 1040, 780)
		tt.SetDark(dark)
		tt.SetScale(2)
		tt.Frame()
		save(t, tt.Image(), "editor-"+mode)

		a, _ = testApp(t, mock.Options{Sample: true, HelperInstalled: true, Running: true})
		a.openChecker()
		a.checker.target = "www.youtube.com"
		a.checker.app = api.AppRef{Path: "/Applications/Google Chrome.app", BundleID: "com.google.Chrome", Name: "Google Chrome"}
		a.runCheck()
		tt = ui.NewTester(a.WindowView, 1040, 780)
		tt.SetDark(dark)
		tt.SetScale(2)
		tt.Frame()
		save(t, tt.Image(), "check-"+mode)

		// The hotspot: its card after a change of its IP, and locked, with
		// the sheet that asks for its admin password.
		a, svc := testApp(t, mock.Options{Sample: true, HelperInstalled: true, Running: true})
		a.page = "network"
		a.hotspot.renewals["en9"] = api.HotspotRenewal{Before: api.Probe{PublicIPv4: "198.51.100.80"}, After: api.Probe{PublicIPv4: "198.51.100.81"}}
		tt = ui.NewTester(a.WindowView, 1040, 1180)
		tt.SetDark(dark)
		tt.SetScale(2)
		tt.Frame()
		save(t, tt.Image(), "hotspot-"+mode)
		svc.LockHotspot()
		a.Sync(context.Background())
		a.hotspot.unlock = unlockDialog{open: true, iface: "en9"}
		tt = ui.NewTester(a.WindowView, 1040, 1180)
		tt.SetDark(dark)
		tt.SetScale(2)
		tt.Frame()
		save(t, tt.Image(), "hotspot-locked-"+mode)
	}
	// Warnings: an interface of a rule is down, and a catch-all rule on
	// top shadows the rules under it.
	a, svc := testApp(t, mock.Options{Sample: true, HelperInstalled: true, Running: true})
	svc.SetInterfaceUp("en7", false)
	a.Sync(context.Background())
	a.cfg.Scenes[0].Rules = append([]api.Rule{{ID: "all", Enabled: true, Protocol: api.AnyProtocol, Action: api.Route,
		Via: []api.InterfaceRef{{Name: "en0", HardwareAddr: "02:00:5e:10:00:01"}}}}, a.cfg.Scenes[0].Rules...)
	tt := ui.NewTester(a.WindowView, 1040, 760)
	tt.SetScale(2)
	tt.Frame()
	save(t, tt.Image(), "rules-warnings")
	tp := ui.NewTester(a.PanelView, PanelWidth, a.PanelHeight())
	tp.SetScale(2)
	tp.Frame()
	tp.SetSize(PanelWidth, a.PanelHeight())
	tp.Frame()
	save(t, tp.Image(), "panel-warnings")

	// A first launch: nothing set up, the helper missing.
	a, _ = testApp(t, mock.Options{})
	tt = ui.NewTester(a.WindowView, 1040, 680)
	tt.SetScale(2)
	tt.Frame()
	save(t, tt.Image(), "first-launch")
	a.start(a.cfg.Selected)
	tt.Frame()
	save(t, tt.Image(), "install-prompt")
}

func TestRuleEditing(t *testing.T) {
	a, svc := testApp(t, mock.Options{Sample: true, HelperInstalled: true})
	n := len(a.scene("home").Rules)
	a.openEditor("home", -1)
	a.editor.draft = "example.com, 10.0.0.0/8 nope!"
	a.editor.addDraft()
	if got := a.editor.targets; len(got) != 2 || got[0] != "*.example.com" || got[1] != "10.0.0.0/8" {
		t.Fatalf("targets %q", got)
	}
	if a.editor.draft != "nope!" || !a.editor.draftErr {
		t.Errorf("draft %q, err %v: want the bad target kept", a.editor.draft, a.editor.draftErr)
	}
	a.editor.draft = ""
	r := a.editor.prepared()
	if len(r.Problems()) != 0 {
		t.Fatalf("problems %v", r.Problems())
	}
	a.saveRule("home", "", r)
	cfg, _ := svc.LoadConfig(context.Background())
	got := cfg.Scenes[0].Rules
	if len(got) != n+1 || got[0].Targets[0] != "*.example.com" || got[0].Via[0].Name != "en0" {
		t.Errorf("saved rules %+v", got[0])
	}
	// Editing a rule keeps each target's subdomains as they were.
	a.openEditor("home", 0)
	a.editor.targets = []string{"example.com", "*.cdn.example.net"}
	a.editor.subdomains = true
	a.editor.draft = "example.org"
	a.editor.addDraft()
	if got := a.editor.prepared().Targets; len(got) != 3 || got[0] != "example.com" || got[1] != "*.cdn.example.net" || got[2] != "*.example.org" {
		t.Errorf("targets after editing %q", got)
	}
	moved := move([]int{0, 1, 2, 3}, []int{3}, 0)
	if moved[0] != 3 || moved[3] != 2 {
		t.Errorf("move = %v", moved)
	}
}

func TestToggle(t *testing.T) {
	a, svc := testApp(t, mock.Options{Sample: true})
	a.toggle()
	if !a.install.open {
		t.Fatal("starting without the helper does not ask to install it")
	}
	a.installHelper(a.install.then)
	st, _ := svc.Status(context.Background())
	if st.State != api.Running {
		t.Errorf("after installing, state %s", st.State)
	}
}

// The pages' capsule sits at the window's center, whatever the traffic
// lights and the routing switch beside it take.
func TestPagesCentered(t *testing.T) {
	for _, o := range []mock.Options{{Sample: true}, {Sample: true, HelperInstalled: true, Running: true}, {Sample: true, HelperInstalled: true, HelperUntrusted: true}} {
		for _, width := range []int{900, 1040, 1400} {
			a, _ := testApp(t, o)
			tt := ui.NewTester(a.WindowView, width, 680)
			tt.SetTitleBar(ui.TitleBar{Height: 28, Left: 76})
			tt.Frame()
			r, ok := tt.Find(locales.Pages())
			if !ok {
				t.Fatal("no pages capsule")
			}
			if off := r.X + r.W/2 - float32(width)/2; off < -0.5 || off > 0.5 {
				t.Errorf("width %d, %+v: the capsule's center is %.1f off the window's", width, o, off)
			}
		}
	}
}

// An unverified helper is never shown as stopped, and turning routing on
// asks for no password: the fix is an explicit reinstall in Settings.
func TestUntrustedHelper(t *testing.T) {
	a, svc := testApp(t, mock.Options{Sample: true, HelperInstalled: true, HelperUntrusted: true})
	if got := a.stateText(); got != locales.CantVerifyHelper() {
		t.Errorf("state %q", got)
	}
	if n := a.notices(); len(n) == 0 || n[0].text != locales.CantVerifyHelper() || n[0].detail != locales.PithCanTVerifyItsHelper() {
		t.Errorf("notices %+v", n)
	}
	a.toggle()
	if a.install.open {
		t.Error("turning routing on with an unverified helper asks for a password")
	}
	a.installHelper("")
	if st, _ := svc.Status(context.Background()); st.Error != nil || !st.Helper.Running {
		t.Errorf("after reinstalling, %+v", st)
	}
}

// A helper that doesn't answer isn't shown as stopped either; Try Again
// asks without a password, and Repair sets it up again.
func TestSilentHelper(t *testing.T) {
	a, svc := testApp(t, mock.Options{Sample: true, HelperInstalled: true, HelperSilent: true})
	if got := a.stateText(); got != locales.HelperNotAnsweringState() {
		t.Errorf("state %q", got)
	}
	n := a.notices()
	if len(n) == 0 || n[0].text != locales.HelperNotAnsweringState() || n[0].detail != locales.TheHelperIsnTAnswering() || n[0].danger {
		t.Fatalf("notices %+v", n)
	}
	n[0].do()
	a.toggle()
	if a.install.open {
		t.Error("turning routing on with a silent helper asks for a password")
	}
	a.installHelper("")
	if st, _ := svc.Status(context.Background()); st.Error != nil || !st.Helper.Running {
		t.Errorf("after repairing, %+v", st)
	}
}

func TestShadowed(t *testing.T) {
	all := api.Rule{ID: "all", Enabled: true, Protocol: api.AnyProtocol, Action: api.Route}
	wechat := api.Rule{ID: "w", Enabled: true, Protocol: api.TCP, Action: api.Route, Apps: []api.AppRef{{Path: "/Applications/WeChat.app"}}}
	nas := api.Rule{ID: "n", Enabled: true, Protocol: api.AnyProtocol, Action: api.Route, Targets: []string{"192.168.50.10"}}
	lan := api.Rule{ID: "l", Enabled: true, Protocol: api.AnyProtocol, Action: api.Route, Targets: []string{"192.168.0.0/16"}}
	sub := api.Rule{ID: "s", Enabled: true, Protocol: api.AnyProtocol, Action: api.Route, Targets: []string{"www.example.com"}}
	dom := api.Rule{ID: "d", Enabled: true, Protocol: api.AnyProtocol, Action: api.Route, Targets: []string{"*.example.com"}}
	ads := api.Rule{ID: "b", Enabled: true, Protocol: api.AnyProtocol, Action: api.Block, Targets: []string{"*.example.com"}}
	for _, tt := range []struct {
		rules []api.Rule
		i     int
		want  int
	}{
		{[]api.Rule{all, wechat}, 1, 1},
		{[]api.Rule{wechat, all}, 1, 0},
		{[]api.Rule{lan, nas}, 1, 1},
		{[]api.Rule{nas, lan}, 1, 0},
		{[]api.Rule{dom, sub}, 1, 1},
		{[]api.Rule{sub, dom}, 1, 0},
		{[]api.Rule{sub, ads}, 0, 2}, // block rules win from anywhere
		{[]api.Rule{wechat, nas}, 1, 0},
	} {
		if got := shadowedBy(tt.rules, tt.i); got != tt.want {
			t.Errorf("shadowedBy(%v, %d) = %d, want %d", ids(tt.rules), tt.i, got, tt.want)
		}
	}
}

func ids(rules []api.Rule) []string {
	var out []string
	for _, r := range rules {
		out = append(out, r.ID)
	}
	return out
}

func TestTrafficBars(t *testing.T) {
	ny, err := time.LoadLocation("America/New_York")
	if err != nil {
		t.Skip(err)
	}
	// The week after DST ended (2026-11-01): local days are 24 or 25 hours.
	now := time.Date(2026, 11, 4, 23, 30, 0, 0, ny)
	week := trafficSpans[2]
	since := week.since(now)
	if want := time.Date(2026, 10, 29, 0, 0, 0, 0, ny); !since.Equal(want) {
		t.Fatalf("since = %v, want %v", since, want)
	}
	for _, tt := range []struct {
		at   time.Time
		want int
	}{
		{time.Date(2026, 10, 28, 23, 0, 0, 0, ny), -1},
		{since, 0},
		{time.Date(2026, 11, 1, 23, 30, 0, 0, ny), 3}, // the 25-hour day
		{time.Date(2026, 11, 2, 0, 30, 0, 0, ny), 4},
		{time.Date(2026, 11, 4, 23, 0, 0, 0, ny), 6},
		{time.Date(2026, 11, 5, 0, 0, 0, 0, ny), -1},
	} {
		if got := week.bar(since, tt.at); got != tt.want {
			t.Errorf("bar(%v) = %d, want %d", tt.at, got, tt.want)
		}
	}
	if got := week.barStart(since, 6).Format("01-02"); got != "11-04" {
		t.Errorf("last bar starts %s, want 11-04", got)
	}
	hour := trafficSpans[0]
	hs := hour.since(now)
	if hour.bar(hs, hs.Add(-time.Second)) != -1 || hour.bar(hs, hs.Add(59*time.Minute+59*time.Second)) != 59 {
		t.Error("hour bars")
	}
}
