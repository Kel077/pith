package ui

import (
	"time"

	"github.com/egoist/mygo/ui"

	"github.com/Kel077/pith/internal/api"
	"github.com/Kel077/pith/locales"
)

// PanelWidth is the width of the menu bar panel.
const PanelWidth = 320

// The heights of the panel's parts, in DIPs, for its first guess at its
// height.
const (
	panelChrome   = 174 // the padding, the header, the titles, the gaps and the buttons
	panelSceneRow = 33
	panelIfaceRow = 44
	panelTotals   = 30
	panelNotice   = 36 // a line; 8 more with an action, and 6 for the notices' gap to the scenes
	panelScenes   = 6  // the scenes the panel shows at once; more scroll
)

// PanelHeight is a first guess at the height of the menu bar panel; once
// shown, the panel asks for the height its content takes (ResizePanel).
func (a *App) PanelHeight() int {
	if a.panelH > 0 {
		return a.panelH
	}
	h := panelChrome + panelSceneRow*min(len(a.cfg.Scenes), panelScenes) + panelIfaceRow*len(a.panelIfaces())
	if len(a.cfg.Scenes) > panelScenes {
		h += panelSceneRow / 2
	}
	if ns := a.notices(); len(ns) > 0 {
		h += 6
		for _, n := range ns {
			h += panelNotice
			if n.action != "" {
				h += 8
			}
		}
	}
	if a.isRunning() {
		h += panelTotals
	}
	return h
}

// PanelView is the panel under the menu bar icon: the switch, the scenes,
// the interfaces and their speed. It draws on the material of the
// system's popover, and so on a background of the same light where there
// is none, as in tests.
func (a *App) PanelView(c *ui.Context) {
	t := theme(c)
	c.SetTheme(t)
	a.flushToasts(c)
	if c.Vibrancy() {
		c.Root().Background(ui.Transparent)
	} else {
		c.Root().LinearGradient(panelBackdrop(t))
	}
	if c.Shortcut(0, ui.KeyEscape) && a.ClosePanel != nil {
		a.ClosePanel()
	}
	on := a.isRunning() || a.status.State == api.Starting
	panel := ui.Column(c).FillWidth().Padding(14, 12, 12).Gap(12).Draw(func(p *ui.Painter, r ui.Rect) {
		// A glow of tangerine behind the mark, as on the window.
		col := t.Accent.Alpha(0.13)
		if t.Dark {
			col = t.Accent.Alpha(0.10)
		}
		p.FillRadialGradient(r, ui.RadialGradient{
			Center: ui.Point{X: 0.08, Y: 0},
			Radius: 260,
			Oklab:  true,
			Stops:  []ui.GradientStop{{Color: col, At: 0}, {Color: col.Alpha(0), At: 1}},
		}, 0)
	})
	if h := int(panel.Bounds().H + 0.99); h > 0 && h != a.panelH {
		a.panelH = h
		if a.ResizePanel != nil {
			a.ResizePanel(h)
		}
	}
	panel.Children(func() {
		a.panelHeader(c, on)
		if ns := a.notices(); len(ns) > 0 {
			ui.Column(c).Gap(6).Children(func() {
				for _, n := range ns {
					noticeBar(c, n, true)
				}
			})
		}
		panelSection(c, locales.Scene(), func() {
			scenes := ui.Column(c).Gap(2)
			if len(a.cfg.Scenes) > panelScenes {
				// Half a row more shows that the rest scrolls.
				scenes = ui.Scroll(c).Gap(2).MaxHeight(panelScenes*panelSceneRow + panelSceneRow/2)
			}
			scenes.Children(func() { a.panelScenes(c) })
		})
		if ifs := a.panelIfaces(); len(ifs) > 0 || a.isRunning() {
			panelSection(c, locales.Interfaces(), func() { a.panelIfaceRows(c, ifs) })
		}
		a.panelFooter(c)
	})
	glassToasts(c)
}

// panelBackdrop is the light of the system's popover, for the panel where
// it has none.
func panelBackdrop(t *ui.Theme) ui.LinearGradient {
	if t.Dark {
		return ui.LinearGradient{From: ui.Hex("#2b2a2c"), To: ui.Hex("#232326"), Angle: 180}
	}
	return ui.LinearGradient{From: ui.Hex("#f7f5f2"), To: ui.Hex("#f0eff0"), Angle: 180}
}

// panelHeader is the top of the panel: the app's icon and name, whether
// routing runs, and the switch.
func (a *App) panelHeader(c *ui.Context, on bool) {
	t := c.Theme()
	ui.Row(c).Gap(11).Padding(0, 2, 2, 0).Children(func() {
		ui.Image(c, appIconSVG).Size(38, 38).Shrink(0)
		ui.Column(c).Grow(1).Gap(2).MinWidth(0).Children(func() {
			ui.Text(c, locales.AppName()).FontSize(15).FontWeight(700).SingleLine()
			ui.Row(c).Gap(6).Children(func() {
				col := t.TextMuted
				switch {
				case on:
					col = running(t)
				case a.status.State == api.Failed:
					col = t.Danger
				}
				dot(c, col, 7, !on)
				ui.Text(c, a.stateText()).FontSize(12).TextColor(t.TextMuted).SingleLine()
			})
		})
		sw := on
		if ui.Switch(c, &sw).Label(locales.RoutingSwitch()).Disabled(a.busy != "" || !a.loaded).Changed() {
			a.toggle() // an install prompt opens the window
		}
	})
}

// panelSection is a part of the panel under a small title.
func panelSection(c *ui.Context, title string, body func()) {
	ui.Column(c).Gap(5).Children(func() {
		ui.Text(c, title).FontSize(11).FontWeight(600).TextColor(c.Theme().TextMuted).Padding(0, 10).SingleLine()
		body()
	})
}

// panelScenes lists the scenes as rows to choose from: the one that runs,
// or would, on a bright pill with a check. A click switches to one.
func (a *App) panelScenes(c *ui.Context) {
	t := c.Theme()
	for k := range a.cfg.Scenes {
		s := &a.cfg.Scenes[k]
		id, name := s.ID, sceneName(s)
		active := a.isRunning() && a.status.SceneID == id || !a.isRunning() && a.cfg.Selected == id
		row := panelRow(c, "scene-"+id, active)
		row.Children(func() {
			mark := ui.Icon(c, icCheck).FontSize(14).TextColor(t.Accent)
			if !active {
				mark.Opacity(0)
			}
			text := ui.Text(c, name).SingleLine().Grow(1).MinWidth(0)
			if active {
				text.FontWeight(600)
			}
			ui.Text(c, locales.RuleCount(len(s.Rules))).FontSize(12).TextColor(t.TextMuted).SingleLine()
		})
		if row.Clicked() && !active {
			if a.isRunning() {
				a.start(id)
			} else {
				a.showScene(id)
			}
		}
	}
}

// panelIfaceRows lists the interfaces with their speed now, in one card,
// and under them what routing has carried.
func (a *App) panelIfaceRows(c *ui.Context, ifs []*api.Interface) {
	t := c.Theme()
	rates := a.ifaceRates()
	panelCard(c).Children(func() {
		for _, i := range ifs {
			ref := refOf(i)
			col := a.colorOf(i.Name)
			ui.Row(c.Key("if-"+i.Name)).Gap(10).Padding(8, 12, 8, 9).Children(func() {
				panelIfaceBadge(c, i.Kind, col, i.Up)
				name := ui.Text(c, a.ifaceLabel(ref)).FontWeight(500).SingleLine().Grow(1).MinWidth(0)
				if !i.Up {
					name.TextColor(t.TextMuted)
				}
				switch r, ok := rates[i.Name]; {
				case !i.Up:
					ui.Text(c, locales.NotConnected()).FontSize(12).TextColor(t.TextMuted).SingleLine()
				case ok && a.isRunning():
					ui.Column(c).Gap(1).AlignItems(ui.End).Shrink(0).Children(func() {
						ui.Text(c, "↓ "+rate(r[0])).FontSize(12).FontFeatures("tnum").SingleLine()
						ui.Text(c, "↑ "+rate(r[1])).FontSize(11).FontFeatures("tnum").TextColor(t.TextMuted).SingleLine()
					})
				default:
					ui.Text(c, firstOf(i.IPv4)).FontSize(12).FontFeatures("tnum").TextColor(t.TextMuted).SingleLine()
				}
			})
		}
		if a.isRunning() {
			ui.Row(c.Key("totals")).Padding(8, 12).Gap(6).Background(subtle(t, 0.025)).Children(func() {
				ui.Text(c, locales.PanelTotals(a.status.Open, byteSize(a.status.Received), byteSize(a.status.Sent))).
					FontSize(12).TextColor(t.TextMuted).SingleLine()
			})
		}
	})
}

// panelIfaceBadge is the mark of an interface in the panel: its kind's
// icon on a disc of its color, or in a ring of it, faded, when it is not
// connected.
func panelIfaceBadge(c *ui.Context, kind api.InterfaceKind, col ui.Color, up bool) {
	t := c.Theme()
	b := ui.Box(c).Size(28, 28).Radius(14).Center().Shrink(0)
	fill := col.Alpha(0.15)
	if t.Dark {
		fill = col.Alpha(0.24)
	}
	if up {
		b.Background(fill)
	} else {
		b.Border(1.5, col.Alpha(0.45))
	}
	b.Children(func() {
		ic := ui.Icon(c, kindIcon(kind)).FontSize(15)
		if up {
			ic.TextColor(col)
		} else {
			ic.TextColor(t.TextMuted)
		}
	})
}

// panelFooter opens the window or quits.
func (a *App) panelFooter(c *ui.Context) {
	ui.Row(c).Gap(8).Margin(2, 0, 0).Children(func() {
		t := c.Theme()
		open := panelCapsule(c, "open", pithMark, t.Accent, locales.OpenPith()).Grow(1)
		if open.Clicked() && a.OpenWindow != nil {
			a.OpenWindow()
		}
		quit := panelCapsule(c, "quit", icPower, t.TextMuted, locales.Quit())
		if quit.Clicked() {
			a.quit()
		}
	})
}

// panelCapsule is a button of the panel: a capsule a little brighter than
// the panel, which takes the keyboard focus, its icon in iconColor.
func panelCapsule(c *ui.Context, key string, ic *ui.SVG, iconColor ui.Color, label string) ui.Element {
	t := c.Theme()
	fill, line := ui.RGBA(255, 255, 255, 0.66), ui.RGBA(60, 40, 20, 0.08)
	if t.Dark {
		fill, line = ui.RGBA(255, 255, 255, 0.08), ui.RGBA(255, 255, 255, 0.08)
	}
	b := ui.Row(c.Key(key)).Gap(7).Padding(7, 16, 7, 14).Radius(999).Center().Cursor(ui.CursorPointer).
		Label(label).Focusable().Background(fill).Border(1, line)
	switch {
	case b.Pressed() && t.Dark:
		b.Background(ui.RGBA(255, 255, 255, 0.04))
	case b.Pressed():
		b.Background(ui.RGBA(255, 255, 255, 0.4))
	case b.Hovered() && t.Dark:
		b.Background(ui.RGBA(255, 255, 255, 0.13))
	case b.Hovered():
		b.Background(ui.RGBA(255, 255, 255, 0.92))
	}
	if !t.Dark {
		b.Shadow(0, 1, 3, 0, ui.RGBA(60, 40, 20, 0.08))
	}
	b.Children(func() {
		ui.Icon(c, ic).FontSize(14).TextColor(iconColor)
		ui.Text(c, label).FontWeight(500).SingleLine()
	})
	return b
}

// panelCard is a group of the panel's rows: a rounded card a little
// brighter than the panel, its rows divided by hairlines. It is no glass,
// which over the system's material would show the panel's emptiness.
func panelCard(c *ui.Context) ui.Element {
	t := c.Theme()
	fill, line := ui.RGBA(255, 255, 255, 0.58), ui.RGBA(60, 40, 20, 0.07)
	if t.Dark {
		fill, line = ui.RGBA(255, 255, 255, 0.05), ui.RGBA(255, 255, 255, 0.07)
	}
	return ui.Column(c).Radius(14).Background(fill).Border(1, line).Dividers(1, hairline(t)).Clip()
}

// panelIfaces are the interfaces the panel lists: those connected, and
// those the rules of the scene that runs, or would, route through.
func (a *App) panelIfaces() []*api.Interface {
	id := a.cfg.Selected
	if a.isRunning() {
		id = a.status.SceneID
	}
	used := map[string]bool{}
	if s := a.scene(id); s != nil {
		for _, r := range s.Rules {
			if r.Enabled && r.Action == api.Route {
				for _, v := range r.Via {
					if i := a.iface(v); i != nil {
						used[i.Name] = true
					}
				}
			}
		}
	}
	var out []*api.Interface
	for _, i := range a.visibleIfaces() {
		if i.Up || used[i.Name] {
			out = append(out, i)
		}
	}
	return out
}

// ifaceRates is the speed of each interface over the last full second:
// received, then sent.
func (a *App) ifaceRates() map[string][2]uint64 {
	// The second before the one the traffic was read in: it was complete.
	last := a.trafficAt.Truncate(time.Second).Add(-time.Second)
	out := map[string][2]uint64{}
	for _, s := range a.traffic.Series {
		if s.At.Equal(last) {
			out[s.Interface] = [2]uint64{s.Received, s.Sent}
		}
	}
	for _, i := range a.ifaces {
		if _, ok := out[i.Name]; !ok && i.Up {
			out[i.Name] = [2]uint64{}
		}
	}
	return out
}

// quit quits Pith; BeforeQuit runs on the way.
func (a *App) quit() { a.Desktop.Quit() }

// BeforeQuit stops routing as Pith quits, unless it should keep routing.
// It waits for the backend a few seconds at most.
func (a *App) BeforeQuit() {
	active := a.isRunning() || a.status.State == api.Starting || a.busy == "start"
	if !active || a.cfg.Settings.KeepRouting {
		return
	}
	ctx, cancel := contextWithTimeout(3 * time.Second)
	defer cancel()
	_ = a.Service.Stop(ctx)
}

// panelRow is a row of the panel to choose: a bright pill when chosen,
// faintly lit under the pointer otherwise.
func panelRow(c *ui.Context, key string, chosen bool) ui.Element {
	t := c.Theme()
	row := ui.Row(c.Key(key)).Gap(10).Padding(8, 12, 8, 10).Radius(11).Cursor(ui.CursorPointer)
	switch {
	case chosen && t.Dark:
		row.Background(ui.RGBA(255, 255, 255, 0.13)).Shadow(0, 1, 3, 0, ui.RGBA(0, 0, 0, 0.3))
	case chosen:
		row.Background(ui.RGBA(255, 255, 255, 0.9)).Shadow(0, 1, 4, 0, ui.RGBA(60, 40, 20, 0.12))
	case row.Hovered():
		row.Background(subtle(t, 0.05))
	}
	return row
}

func firstOf(s []string) string {
	if len(s) == 0 {
		return ""
	}
	return s[0]
}
