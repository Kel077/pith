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
	"time"

	"github.com/egoist/mygo"
	"github.com/egoist/mygo/ui"
	pithui "github.com/Kel077/pith/internal/ui"
)

// version is the app's version, set by the build.
var version = "0.1.0"

func main() {
	useMock := flag.Bool("mock", false, "use an in-memory backend that simulates routing")
	page := flag.String("page", "", "open the window on this page: rules, activity, network or settings")
	language := flag.String("lang", "", "speak this language (en or zh-Hans) instead of the setting's")
	showPanel := flag.Bool("panel", false, "show the menu bar panel at launch")
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
		s.ready(svc, *page, *language, !*showPanel)
		if *showPanel {
			// As a click on the icon would, once the icon is in the menu bar:
			// activate Pith, then open the panel.
			go func() {
				time.Sleep(time.Second)
				s.window.Update(func() {
					mygo.App.Focus()
					s.togglePanel()
				})
			}()
		}
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
	window   *mygo.Window // the main window, made hidden at launch
	panel    *mygo.Window // the flyout under the menu bar icon, while open
	tray     *mygo.Tray
	quitting bool
}

func (s *shell) ready(svc service, page, language string, openWindow bool) {
	s.app = pithui.New(pithui.Options{
		Page:     page,
		Language: language,
		Service:  svc,
		Desktop:  desktop{},
		Version:  version,
		Post: func(fn func()) {
			s.window.Update(func() {
				fn()
				if s.panel != nil {
					s.panel.Invalidate()
				}
			})
		},
		Tray: func(on bool, title string) {
			if s.tray != nil {
				s.tray.SetIcon(pithui.TrayIcon(on), true)
				s.tray.SetTitle(title)
			}
		},
		OpenWindow: s.showWindow,
		ClosePanel: s.closePanel,
		ResizePanel: func(h int) {
			if p := s.panel; p != nil {
				go p.SetSize(pithui.PanelWidth, h)
			}
		},
	})
	// The window exists from the start, hidden: backend updates reach the
	// interface through it whether it shows or not.
	s.window = mygo.NewWindow(mygo.WindowOptions{
		Title:         "橘核 Pith",
		Width:         1040,
		Height:        680,
		MinWidth:      880,
		MinHeight:     560,
		Hidden:        true,
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
		s.closePanel()
		menu := mygo.NewMenu([]*mygo.MenuItem{
			{Label: pithui.Tr("Open Pith…", "打开橘核…"), Click: func(*mygo.MenuItem, *mygo.Window) { s.showWindow() }},
			mygo.Separator(),
			{Role: mygo.RoleQuit},
		})
		tray.SetMenu(menu)
		defer tray.SetMenu(nil)
		tray.PopUpMenu()
	})
	mygo.App.OnWillQuit(func(*mygo.QuitEvent) { tray.Destroy() })
	s.app.Run(context.Background())
	if openWindow && !mygo.App.WasOpenedAtLogin() {
		s.showWindow()
	}
}

// togglePanel opens the panel under the menu bar icon, or closes it. A
// flyout closes by itself as the user clicks elsewhere, and a click on the
// icon that closes it does not open it again.
func (s *shell) togglePanel() {
	if s.panel != nil {
		s.panel.Close()
		return
	}
	p := mygo.NewFlyout(mygo.FlyoutOptions{
		Tray:      s.tray,
		Placement: mygo.PlacementBottom,
		Gap:       4,
		Width:     pithui.PanelWidth,
		Height:    s.app.PanelHeight(),
		Focusable: true,
		Shadow:    true,
		Popover:   true,
		Content:   ui.View(s.app.PanelView),
	})
	s.panel = p
	p.OnClosed(func() {
		if s.panel == p {
			s.panel = nil
		}
	})
}

func (s *shell) closePanel() {
	if s.panel != nil {
		s.panel.Close()
	}
}

// showWindow shows the main window. While it shows, Pith is in the Dock
// and the app switcher.
func (s *shell) showWindow() {
	if s.window == nil {
		return
	}
	s.closePanel()
	mygo.App.SetActivationPolicy(mygo.ActivationPolicyRegular)
	s.window.Show()
	s.window.Focus()
	mygo.App.Focus()
}
