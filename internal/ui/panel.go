package ui

import (
	"time"

	"github.com/egoist/mygo/ui"
	"github.com/Kel077/pith/internal/api"
)

// PanelWidth is the width of the menu bar panel.
const PanelWidth = 320

// PanelHeight is a first guess at the height of the menu bar panel; once
// shown, the panel asks for the height its content takes (ResizePanel).
func (a *App) PanelHeight() int {
	if a.panelH > 0 {
		return a.panelH
	}
	return 220 + 33*len(a.cfg.Scenes) + 32*len(a.panelIfaces())
}

// PanelView is the panel under the menu bar icon: the switch, the scenes,
// the interfaces and their speed.
func (a *App) PanelView(c *ui.Context) {
	t := theme(c)
	c.SetTheme(t)
	a.flushToasts(c)
	if c.Vibrancy() {
		c.Root().Background(ui.Transparent)
	}
	if c.Shortcut(0, ui.KeyEscape) && a.ClosePanel != nil {
		a.ClosePanel()
	}
	on := a.isRunning() || a.status.State == api.Starting
	panel := ui.Column(c).FillWidth().Padding(14, 10, 10).Gap(4)
	if h := int(panel.Bounds().H + 0.99); h > 0 && h != a.panelH {
		a.panelH = h
		if a.ResizePanel != nil {
			a.ResizePanel(h)
		}
	}
	panel.Children(func() {
		ui.Row(c).Gap(10).Padding(2, 6, 8).Children(func() {
			ui.Icon(c, pithMark).FontSize(22).TextColor(t.Accent)
			ui.Column(c).Grow(1).Gap(2).MinWidth(0).Children(func() {
				ui.Text(c, tr("Pith", "橘核")).FontSize(15).Bold()
				ui.Row(c).Gap(6).Children(func() {
					col := t.TextMuted
					if on {
						col = running(t)
					} else if a.status.State == api.Failed {
						col = t.Danger
					}
					dot(c, col, 7, !on)
					ui.Text(c, a.stateText()).FontSize(12).TextColor(t.TextMuted).SingleLine()
				})
			})
			sw := on
			if ui.Switch(c, &sw).Label(tr("Routing", "分流")).Disabled(a.busy != "" || !a.loaded).Changed() {
				a.toggle() // an install prompt opens the window
			}
		})
		for _, n := range a.notices() {
			noticeBar(c, n, true)
		}
		panelDivider(c)
		panelTitle(c, tr("Scene", "场景"))
		scenes := ui.Column(c).Gap(0)
		if len(a.cfg.Scenes) > 6 {
			scenes = ui.Scroll(c).MaxHeight(6 * 34)
		}
		scenes.Children(func() { a.panelScenes(c) })
		panelDivider(c)
		panelTitle(c, tr("Interfaces", "网卡"))
		a.panelIfaceRows(c)
		panelDivider(c)
		a.panelFooter(c)
	})
}

// panelScenes lists the scenes; a click switches to one.
func (a *App) panelScenes(c *ui.Context) {
	t := c.Theme()
	for k := range a.cfg.Scenes {
		s := &a.cfg.Scenes[k]
		id := s.ID
		active := a.isRunning() && a.status.SceneID == id || !a.isRunning() && a.cfg.Selected == id
		row := panelRow(c, "scene-"+id)
		row.Children(func() {
			mark := ui.Icon(c, icCheck).FontSize(14).TextColor(t.Accent)
			if !active {
				mark.Opacity(0)
			}
			ui.Text(c, sceneName(s)).SingleLine().Grow(1)
			ui.Text(c, trf("%d rules", "%d 条规则", len(s.Rules))).FontSize(12).TextColor(t.TextMuted).SingleLine()
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

// panelIfaceRows lists the interfaces with their speed now.
func (a *App) panelIfaceRows(c *ui.Context) {
	t := c.Theme()
	rates := a.ifaceRates()
	for _, i := range a.panelIfaces() {
		ref := refOf(i)
		ui.Row(c.Key("if-"+i.Name)).Gap(10).Padding(6, 10).Children(func() {
			dot(c, a.colorOf(i.Name), 8, !i.Up)
			name := ui.Text(c, a.ifaceLabel(ref)).SingleLine().Grow(1)
			if !i.Up {
				name.TextColor(t.TextMuted)
			}
			switch r, ok := rates[i.Name]; {
			case !i.Up:
				ui.Text(c, tr("Not connected", "未连接")).FontSize(12).TextColor(t.TextMuted)
			case ok && a.isRunning():
				ui.Text(c, "↓ "+rate(r[0])).FontSize(12).Font("monospace").SingleLine()
				ui.Text(c, "↑ "+rate(r[1])).FontSize(12).Font("monospace").TextColor(t.TextMuted).SingleLine()
			default:
				ui.Text(c, firstOf(i.IPv4)).FontSize(12).Font("monospace").TextColor(t.TextMuted).SingleLine()
			}
		})
	}
}

// panelFooter shows the totals, and opens the window or quits.
func (a *App) panelFooter(c *ui.Context) {
	t := c.Theme()
	ui.Row(c).Gap(6).Padding(4, 10).Children(func() {
		text := tr("Not routing", "没有在分流")
		if a.isRunning() {
			text = trf("%d open · ↓ %s · ↑ %s", "%d 个连接 · ↓ %s · ↑ %s", a.status.Open, byteSize(a.status.Received), byteSize(a.status.Sent))
		}
		ui.Text(c, text).FontSize(12).TextColor(t.TextMuted).SingleLine()
	})
	ui.Row(c).Gap(8).Padding(4, 4, 0).Children(func() {
		if ui.Button(c, tr("Open Pith", "打开橘核")).Grow(1).Clicked() && a.OpenWindow != nil {
			a.OpenWindow()
		}
		if ui.Button(c, tr("Quit", "退出")).Clicked() {
			a.quit()
		}
	})
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

func panelTitle(c *ui.Context, s string) {
	ui.Text(c, s).FontSize(11).FontWeight(600).TextColor(c.Theme().TextMuted).Padding(6, 10, 2)
}

func panelDivider(c *ui.Context) {
	ui.Box(c).Height(1).Margin(4, 8).Background(c.Theme().Border)
}

// panelRow is a row of the panel that highlights under the pointer.
func panelRow(c *ui.Context, key string) ui.Element {
	t := c.Theme()
	row := ui.Row(c.Key(key)).Gap(10).Padding(7, 10).Radius(8).Cursor(ui.CursorPointer)
	if row.Hovered() {
		row.Background(subtle(t, 0.07))
	}
	return row
}

func firstOf(s []string) string {
	if len(s) == 0 {
		return ""
	}
	return s[0]
}
