// Command pith is 橘核 Pith, the menu bar app: an icon in the menu bar
// whose panel turns routing on and off, and a window for the rules.
//
//	go run ./cmd/pith          the real backend
//	go run ./cmd/pith -mock    an in-memory backend, for working on the interface
package main

import (
	"context"
	"flag"
	"log"

	"github.com/egoist/mygo"
	"github.com/egoist/mygo/ui"
	pithui "github.com/Kel077/pith/internal/ui"
)

// version is the app's version, set by the build.
var version = "0.1.0"

func main() {
	useMock := flag.Bool("mock", false, "use an in-memory backend that simulates routing")
	flag.Parse()
	if !mygo.App.RequestSingleInstanceLock() {
		return // the Pith that runs shows itself
	}
	mygo.App.SetName("Pith")
	mygo.App.SetActivationPolicy(mygo.ActivationPolicyAccessory)
	mygo.App.OnWindowAllClosed(func() {}) // Pith stays in the menu bar
	s := &shell{}
	mygo.App.WhenReady(func() {
		svc, err := newService(*useMock)
		if err != nil {
			log.Fatal(err)
		}
		s.ready(svc)
	})
	mygo.App.OnSecondInstance(func([]string, string) { s.showWindow() })
	mygo.App.OnActivate(func(bool) { s.showWindow() })
	mygo.App.OnBeforeQuit(func(*mygo.QuitEvent) {
		s.quitting = true
		if s.app != nil {
			s.app.BeforeQuit()
		}
	})
	if err := mygo.App.Run(); err != nil {
		log.Fatal(err)
	}
}

// shell holds the windows and the menu bar icon. All of it lives on the
// main thread.
type shell struct {
	app      *pithui.App
	panel    *mygo.Window
	window   *mygo.Window
	tray     *mygo.Tray
	quitting bool
}

func (s *shell) ready(svc service) {
	s.app = pithui.New(pithui.Options{
		Service: svc,
		Desktop: desktop{},
		Version: version,
		Post: func(fn func()) {
			s.panel.Update(func() {
				fn()
				if s.window != nil {
					s.window.Invalidate()
				}
			})
		},
		Tray: func(on bool, title string) {
			if s.tray != nil {
				s.tray.SetIcon(pithui.TrayIcon(on), true)
				s.tray.SetTitle(title)
			}
		},
		OpenWindow:  s.showWindow,
		ClosePanel:  func() { s.panel.Hide() },
		ResizePanel: func(h int) { go s.panel.SetSize(pithui.PanelWidth, h) },
	})
	s.panel = mygo.NewWindow(mygo.WindowOptions{
		Title:             "Pith",
		Width:             pithui.PanelWidth,
		Height:            s.app.PanelHeight(),
		Hidden:            true,
		Panel:             true,
		Frameless:         true,
		Transparent:       true,
		Vibrancy:          mygo.VibrancyPopover,
		CornerRadius:      12,
		DisableResize:     true,
		DisableMinimize:   true,
		DisableMaximize:   true,
		DisableFullScreen: true,
		AlwaysOnTop:       true,
		SkipTaskbar:       true,
		Content:           ui.View(s.app.PanelView),
	})
	s.panel.OnBlur(s.panel.Hide)
	s.panel.OnClose(func(e *mygo.CloseEvent) {
		if !s.quitting {
			e.PreventDefault()
			s.panel.Hide()
		}
	})
	tray, err := mygo.NewTray(mygo.TrayOptions{
		ID:             "pith",
		Icon:           pithui.TrayIcon(false),
		IconIsTemplate: true,
		ToolTip:        "橘核 Pith",
	})
	if err != nil {
		log.Fatal("tray: ", err)
	}
	s.tray = tray
	tray.OnClick(s.togglePanel)
	tray.OnRightClick(func() {
		s.panel.Hide()
		menu := mygo.NewMenu([]*mygo.MenuItem{
			{Label: "Open Pith…", Click: func(*mygo.MenuItem, *mygo.Window) { s.showWindow() }},
			mygo.Separator(),
			{Role: mygo.RoleQuit},
		})
		tray.SetMenu(menu)
		defer tray.SetMenu(nil)
		tray.PopUpMenu()
	})
	mygo.App.OnWillQuit(func(*mygo.QuitEvent) { tray.Destroy() })
	s.app.Run(context.Background())
	if !mygo.App.WasOpenedAtLogin() {
		s.showWindow()
	}
}

// togglePanel shows the panel under the menu bar icon, or hides it.
func (s *shell) togglePanel() {
	if s.panel.IsVisible() {
		s.panel.Hide()
		return
	}
	anchor := s.tray.Bounds()
	b := s.panel.Bounds()
	if anchor.Width > 0 {
		area := mygo.Screen.DisplayNearestPoint(mygo.Point{X: anchor.X + anchor.Width/2, Y: anchor.Y + anchor.Height/2}).WorkArea
		x := anchor.X + anchor.Width/2 - b.Width/2
		y := anchor.Y + anchor.Height + 6
		x = max(area.X+8, min(x, area.X+area.Width-b.Width-8))
		s.panel.SetPosition(x, y)
	} else {
		s.panel.Center()
	}
	s.panel.Show()
	s.panel.Focus()
}

// showWindow shows the main window, making it when it does not exist yet.
// While it shows, Pith is in the Dock and the app switcher.
func (s *shell) showWindow() {
	if s.app == nil {
		return
	}
	if s.window == nil {
		s.window = mygo.NewWindow(mygo.WindowOptions{
			Title:         "橘核 Pith",
			Width:         1040,
			Height:        680,
			MinWidth:      880,
			MinHeight:     560,
			TitleBarStyle: mygo.TitleBarHidden,
			StateKey:      "main",
			Content:       ui.View(s.app.WindowView),
		})
		s.window.OnClose(func(e *mygo.CloseEvent) {
			if s.quitting {
				return
			}
			e.PreventDefault()
			s.window.Hide()
			mygo.App.SetActivationPolicy(mygo.ActivationPolicyAccessory)
		})
	}
	s.panel.Hide()
	mygo.App.SetActivationPolicy(mygo.ActivationPolicyRegular)
	s.window.Show()
	s.window.Focus()
	mygo.App.Focus()
}
