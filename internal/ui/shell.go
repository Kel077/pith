package ui

import (
	"fmt"

	"github.com/egoist/mygo/ui"
	"github.com/Kel077/pith/internal/api"
)

// WindowView is the main window: a sidebar of four pages and the routing
// switch, beside the page shown.
func (a *App) WindowView(c *ui.Context) {
	t := theme(c)
	c.SetTheme(t)
	a.flushToasts(c)
	if c.Vibrancy() {
		c.Root().Background(ui.Transparent)
	}
	ui.Row(c).Fill().AlignItems(ui.Stretch).Children(func() {
		a.sidebar(c)
		ui.Column(c).Grow(1).MinWidth(0).Background(t.Background).Children(func() {
			switch a.page {
			case "activity":
				a.activityView(c)
			case "network":
				a.networkView(c)
			case "settings":
				a.settingsView(c)
			default:
				a.rulesView(c)
			}
		})
	})
	a.dialogs(c)
	if c.Shortcut(ui.Cmd, ui.KeyN) && a.editor == nil && a.scene(a.sceneID) != nil {
		a.page = "rules"
		a.openEditor(a.sceneID, -1)
	}
	for i, p := range []string{"rules", "activity", "network", "settings"} {
		if c.Shortcut(ui.Cmd, ui.Key1+ui.Key(i)) {
			a.page = p
		}
	}
}

// pageHeader is the top of a page: its title, and its actions on the right.
// It drags the window, as a title bar does.
func pageHeader(c *ui.Context, title string, actions func()) {
	bar := c.TitleBar()
	ui.Row(c).Height(max(bar.Height, 56)).Padding(12, 24, 0, 28).Gap(8).DragWindow().Shrink(0).Children(func() {
		ui.Text(c, title).FontSize(20).Bold().SingleLine()
		ui.Spacer(c)
		if actions != nil {
			actions()
		}
	})
}

func (a *App) sidebar(c *ui.Context) {
	t := c.Theme()
	bar := c.TitleBar()
	side := ui.Column(c).Width(212).Shrink(0).Padding(max(bar.Height, 28)+14, 12, 12).Gap(14)
	if !c.Vibrancy() {
		side.Background(t.Surface)
	}
	side.BorderWidth(0, 1, 0, 0).BorderColor(t.Border).DragWindow()
	side.Children(func() {
		ui.Row(c).Gap(8).Padding(0, 8).Children(func() {
			ui.Icon(c, pithMark).FontSize(20).TextColor(t.Accent)
			ui.Text(c, tr("Pith", "橘核")).FontSize(16).Bold()
		})
		page := a.page
		if ui.Sidebar(c, &page, func() {
			ui.SidebarItem(c, "rules", icRules, tr("Rules", "规则"))
			act := ui.SidebarItem(c, "activity", icActivity, tr("Activity", "活动"))
			if a.isRunning() && a.status.Open > 0 {
				act.Children(func() { ui.Badge(c, fmt.Sprint(a.status.Open)) })
			}
			ui.SidebarItem(c, "network", icNetwork, tr("Network", "网络"))
			ui.SidebarItem(c, "settings", icSettings, tr("Settings", "设置"))
		}).Grow(1).Label(tr("Pages", "页面")).Changed() {
			a.page = page
		}
		a.routingCard(c)
	})
}

// routingCard is the switch that starts and stops routing, with what runs.
func (a *App) routingCard(c *ui.Context) {
	t := c.Theme()
	on := a.isRunning() || a.status.State == api.Starting
	box := ui.Column(c).Padding(12).Gap(6).Radius(12).Background(t.Background).Border(1, t.Border)
	box.Children(func() {
		ui.Row(c).Gap(8).Children(func() {
			col := t.TextMuted
			switch {
			case a.status.State == api.Failed:
				col = t.Danger
			case on:
				col = running(t)
			}
			dot(c, col, 8, !on)
			ui.Text(c, a.stateText()).FontWeight(600).Grow(1).SingleLine()
			sw := on
			if ui.Switch(c, &sw).Label(tr("Routing", "分流")).Disabled(a.busy != "" || !a.loaded).Changed() {
				a.toggle()
			}
		})
		ui.Text(c, a.stateDetail()).FontSize(12).TextColor(t.TextMuted).SingleLine()
	})
}

// stateText says in a word whether routing runs.
func (a *App) stateText() string {
	switch {
	case a.busy == "install":
		return tr("Installing…", "正在安装…")
	case a.busy == "start" || a.status.State == api.Starting:
		return tr("Starting…", "正在启动…")
	case a.busy == "stop" || a.status.State == api.Stopping:
		return tr("Stopping…", "正在停止…")
	case a.status.State == api.Running:
		return tr("Routing", "正在分流")
	case a.status.State == api.Failed:
		return tr("Stopped by an error", "因错误停止")
	}
	return tr("Off", "未分流")
}

// stateDetail says which scene runs and for how long, or which would.
func (a *App) stateDetail() string {
	if a.isRunning() {
		name := sceneName(a.scene(a.status.SceneID))
		return name + " · " + duration(a.Now().Sub(a.status.Since))
	}
	if a.status.State == api.Failed && a.status.Error != nil {
		return errorText(a.status.Error)
	}
	if !a.status.Helper.Installed {
		return tr("Turn on to install the helper", "打开时会安装后台助手")
	}
	return trf("Turn on to route by “%s”", "打开后按「%s」分流", sceneName(a.scene(a.cfg.Selected)))
}
