package ui

import (
	"slices"

	"github.com/egoist/mygo/ui"

	"github.com/Kel077/pith/internal/api"
	"github.com/Kel077/pith/locales"
)

// pages are the window's pages, in the order of the navigation.
var pages = []string{"rules", "activity", "network", "settings"}

// WindowView is the main window: the background's glows, a capsule of
// navigation and the routing switch at the top, and the page shown.
func (a *App) WindowView(c *ui.Context) {
	t := theme(c)
	c.SetTheme(t)
	a.flushToasts(c)
	c.Root().Background(ui.Transparent)
	ambient(c, ui.Column(c).Fill()).Children(func() {
		a.topBar(c)
		ui.Column(c).Grow(1).MinHeight(0).Children(func() {
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
	for i, p := range pages {
		if c.Shortcut(ui.Cmd, ui.Key1+ui.Key(i)) {
			a.page = p
		}
	}
}

// topBar holds the window's controls: the traffic lights on the left, the
// pages in a capsule in the middle, and the routing switch on the right.
// It drags the window, as a title bar does. The capsule sits at the
// window's center: both sides are equally wide, each at least as wide as
// the traffic lights need.
func (a *App) topBar(c *ui.Context) {
	bar := c.TitleBar()
	// As far from the window's edges as the pages' content: the routing
	// capsule stands over the pages' actions.
	const edge = 32
	side := max(max(bar.Left, 76)+12, bar.Right+edge) - edge
	ui.Row(c).Height(64).Padding(0, edge, 0, edge).Gap(12).DragWindow().Shrink(0).Children(func() {
		ui.Row(c).Grow(1).Basis(0).MinWidth(side).Children(func() {})
		chosen := slices.Index(pages, a.page)
		labels := []string{locales.Rules(), locales.Activity(), locales.Network(), locales.Settings()}
		if segmentedCapsule(c, "nav", locales.Pages(), &chosen, labels, []*ui.SVG{icRules, icActivity, icNetwork, icSettings}) {
			a.page = pages[chosen]
		}
		ui.Row(c).Grow(1).Basis(0).MinWidth(side).Justify(ui.End).Children(func() { a.routingCapsule(c) })
	})
}

// routingCapsule is the switch that starts and stops routing, with what
// runs: a capsule at the top right of the window.
func (a *App) routingCapsule(c *ui.Context) {
	t := c.Theme()
	on := a.isRunning() || a.status.State == api.Starting
	capsule(c.Key("routing")).Gap(8).Padding(5, 6, 5, 12).Tooltip(a.stateDetail()).Children(func() {
		col := t.TextMuted
		switch {
		case helperNotResponding(a.status):
			col = t.Warning // unknown, not failed
		case a.status.State == api.Failed:
			col = t.Danger
		case on:
			col = running(t)
		}
		dot(c, col, 8, !on)
		text := a.stateText()
		if a.isRunning() {
			text = sceneName(a.scene(a.status.SceneID))
		}
		ui.Text(c, text).FontWeight(500).SingleLine().MaxWidth(160)
		sw := on
		if ui.Switch(c, &sw).Label(locales.RoutingSwitch()).Disabled(a.busy != "" || !a.loaded).Changed() {
			a.toggle()
		}
	})
}

// pageHeader is the top of a page: its title, a line under it, and its
// actions on the right.
func pageHeader(c *ui.Context, title, subtitle string, actions func()) {
	ui.Row(c).Padding(8, 32, 18).Gap(10).AlignItems(ui.Center).Shrink(0).Children(func() {
		ui.Column(c).Grow(1).Gap(4).MinWidth(0).Children(func() {
			ui.Text(c, title).FontSize(28).FontWeight(700).LetterSpacing(-0.4).SingleLine()
			if subtitle != "" {
				ui.Text(c, subtitle).TextColor(c.Theme().TextMuted).SingleLine()
			}
		})
		// The actions stand in a box as high as the highest of any page's,
		// centered on it, and so does the title: every page's title stands
		// at one height, whatever its actions, or none.
		box := ui.Row(c).Height(headerActionsHeight).Gap(10).AlignItems(ui.Center).Shrink(0)
		if actions != nil {
			box.Children(actions)
		}
	})
}

// headerActionsHeight is the height of the box of a page's actions: room
// for the highest of them, Activity's capsule of views.
const headerActionsHeight = 44

// stateText says in a word whether routing runs.
func (a *App) stateText() string {
	switch {
	case a.busy == "install":
		return locales.Installing()
	case a.busy == "start" || a.status.State == api.Starting:
		return locales.Starting()
	case a.busy == "stop" || a.status.State == api.Stopping:
		return locales.Stopping()
	case a.status.State == api.Running:
		return locales.RoutingNow()
	case helperUntrusted(a.status):
		// Not "stopped": Pith can't tell what an unverified helper does.
		return locales.CantVerifyHelper()
	case helperNotResponding(a.status):
		return locales.HelperNotAnsweringState()
	case a.status.State == api.Failed:
		return locales.StoppedByAnError()
	}
	return locales.RoutingOff()
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
		return locales.TurnOnToInstallTheHelper()
	}
	return locales.TurnOnToRoute(sceneName(a.scene(a.cfg.Selected)))
}
