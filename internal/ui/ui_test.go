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

	"github.com/egoist/mygo/ui"
	"github.com/Kel077/pith/internal/api"
	"github.com/Kel077/pith/internal/mock"
)

var snapshots = flag.String("snapshots", "", "write PNG snapshots of the views to this directory")

// fakeDesktop stands in for the operating system: apps from the mock, and
// icons that are colored squares.
type fakeDesktop struct{ quit bool }

func (fakeDesktop) InstalledApps() ([]api.AppRef, error) {
	return []api.AppRef{
		{Path: "/Applications/Google Chrome.app", BundleID: "com.google.Chrome", Name: "Google Chrome"},
		{Path: "/System/Applications/Mail.app", BundleID: "com.apple.mail", Name: "邮件"},
		{Path: "/System/Applications/Music.app", BundleID: "com.apple.Music", Name: "音乐"},
		{Path: "/Applications/Safari.app", BundleID: "com.apple.Safari", Name: "Safari"},
		{Path: "/Applications/Slack.app", BundleID: "com.tinyspeck.slackmacgap", Name: "Slack"},
		{Path: "/Applications/WeChat.app", BundleID: "com.tencent.xinWeChat", Name: "微信"},
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
func (fakeDesktop) PreferredLanguages() []string   { return []string{"zh-Hans"} }
func (d *fakeDesktop) Quit()                       { d.quit = true }

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
	a := New(Options{Service: svc, Desktop: &fakeDesktop{}, Post: func(fn func()) { fn() }, Version: "0.1.0", Now: func() time.Time { return now }})
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
	}
	// A first launch: nothing set up, the helper missing.
	a, _ := testApp(t, mock.Options{})
	tt := ui.NewTester(a.WindowView, 1040, 680)
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
	if got := a.editor.targets; len(got) != 2 || got[0] != "example.com" || got[1] != "10.0.0.0/8" {
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
