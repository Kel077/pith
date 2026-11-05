package ui

import (
	"strings"

	"github.com/egoist/mygo/plugins/glass"
	"github.com/egoist/mygo/ui"

	"github.com/Kel077/pith/internal/api"
	"github.com/Kel077/pith/locales"
)

// dot is a small round mark: filled, or a ring for something off.
func dot(c *ui.Context, col ui.Color, size float32, hollow bool) ui.Element {
	d := ui.Box(c).Size(size, size).Radius(size / 2).Shrink(0)
	if hollow {
		return d.Border(1.5, col)
	}
	return d.Background(col)
}

// card is a panel on the window's background.
func card(c *ui.Context) ui.Element {
	t := c.Theme()
	return ui.Column(c).Padding(16).Gap(10).Radius(12).Background(t.Background).Border(1, t.Border)
}

// sectionTitle is the small title above a group of things.
func sectionTitle(c *ui.Context, s string) ui.Element {
	return ui.Text(c, s).FontSize(12).FontWeight(600).TextColor(c.Theme().TextMuted)
}

// iconButton is a quiet button showing an icon, named by label.
func iconButton(c *ui.Context, ic *ui.SVG, label string) ui.Element {
	return ui.Button(c, "").Label(label).Tooltip(label).Children(func() { ui.Icon(c, ic) })
}

// textIconButton is a button with an icon before its label.
func textIconButton(c *ui.Context, ic *ui.SVG, label string) ui.Element {
	return ui.Button(c, "").Label(label).Children(func() {
		ui.Icon(c, ic)
		ui.Text(c, label).SingleLine()
	})
}

// primaryIconButton is the accent button with an icon before its label.
func primaryIconButton(c *ui.Context, ic *ui.SVG, label string) ui.Element {
	return ui.PrimaryButton(c, "").Label(label).Children(func() {
		ui.Icon(c, ic)
		ui.Text(c, label).SingleLine()
	})
}

// muted is secondary text.
func muted(c *ui.Context, s string) ui.Element {
	return ui.Text(c, s).TextColor(c.Theme().TextMuted)
}

// emptyState fills a page that has nothing to show yet.
func emptyState(c *ui.Context, ic *ui.SVG, title, body string, actions func()) {
	t := c.Theme()
	ui.Column(c).Grow(1).Center().Gap(10).Padding(40).Children(func() {
		ui.Box(c).Size(64, 64).Radius(999).Material(glass.Glass{Tint: t.Accent.Alpha(0.14)}).Border(1, rim(t)).Center().Children(func() {
			ui.Icon(c, ic).FontSize(28).TextColor(t.Accent)
		})
		ui.Text(c, title).FontSize(18).FontWeight(600).MarginY(4)
		if body != "" {
			ui.Text(c, body).TextColor(t.TextMuted).MaxWidth(340).TextAlign(ui.Center)
		}
		if actions != nil {
			ui.Row(c).Gap(8).MarginY(8).Children(actions)
		}
	})
}

// kindIcon is the icon of a kind of interface.
func kindIcon(k api.InterfaceKind) *ui.SVG {
	switch k {
	case api.KindWiFi:
		return icWiFi
	case api.KindEthernet:
		return icEthernet
	case api.KindCellular:
		return icCellular
	case api.KindThunderbolt:
		return icThunderbolt
	case api.KindVPN:
		return icVPN
	}
	return icOther
}

// ifaceChip shows an interface a rule uses: its color, and its name, faded
// with a ring when it is not connected.
func (a *App) ifaceChip(c *ui.Context, ref api.InterfaceRef) ui.Element {
	t := c.Theme()
	i := a.iface(ref)
	up := i != nil && i.Up
	col := a.colorFor(ref)
	fill := col.Alpha(0.12)
	if t.Dark {
		fill = col.Alpha(0.2)
	}
	chip := ui.Row(c).Gap(6).Padding(3, 9, 3, 8).Radius(999).Background(fill).Shrink(0)
	chip.Children(func() {
		dot(c, col, 7, !up)
		text := ui.Text(c, a.ifaceLabel(ref)).FontSize(12).SingleLine()
		if !up {
			text.TextColor(t.TextMuted)
		}
	})
	if !up {
		chip.Tooltip(locales.NotConnected())
	}
	return chip
}

// systemChip shows the interface macOS routed a connection through, quietly.
func (a *App) systemChip(c *ui.Context, name string) ui.Element {
	t := c.Theme()
	label := name
	if i := a.ifaceByName(name); i != nil {
		label = a.ifaceLabel(refOf(i))
	}
	return ui.Row(c).Gap(6).Padding(3, 9, 3, 8).Radius(999).Border(1, t.Border).Shrink(0).Children(func() {
		dot(c, a.colorOf(name), 7, false).Opacity(0.6)
		ui.Text(c, label).FontSize(12).TextColor(t.TextMuted).SingleLine()
	})
}

// quietButton is a small icon button without a face until hovered.
func quietButton(c *ui.Context, key string, ic *ui.SVG, label string, disabled bool) bool {
	t := c.Theme()
	b := ui.Box(c.Key(key)).Size(26, 26).Radius(7).Center().Label(label).Tooltip(label).Shrink(0)
	if disabled {
		b.Opacity(0.3)
	} else {
		b.Cursor(ui.CursorPointer)
		if b.Hovered() {
			b.Background(subtle(t, 0.08))
		}
	}
	b.Children(func() { ui.Icon(c, ic).FontSize(15).TextColor(t.TextMuted) })
	return !disabled && b.Clicked()
}

// ifaceNameChip is ifaceChip for an interface known by its BSD name.
func (a *App) ifaceNameChip(c *ui.Context, name string) ui.Element {
	if i := a.ifaceByName(name); i != nil {
		return a.ifaceChip(c, refOf(i))
	}
	return a.ifaceChip(c, api.InterfaceRef{Name: name})
}

// tagChip is a small label in a pill: "Blocked", "System".
func tagChip(c *ui.Context, ic *ui.SVG, s string, col ui.Color) ui.Element {
	t := c.Theme()
	fill := col.Alpha(0.12)
	if t.Dark {
		fill = col.Alpha(0.2)
	}
	return ui.Row(c).Gap(5).Padding(3, 9, 3, 8).Radius(999).Background(fill).Shrink(0).Children(func() {
		if ic != nil {
			ui.Icon(c, ic).FontSize(12).TextColor(col)
		}
		ui.Text(c, s).FontSize(12).TextColor(col).SingleLine()
	})
}

// appIconView shows an app's icon, or a stand-in while it loads.
func (a *App) appIconView(c *ui.Context, app api.AppRef, size float32) ui.Element {
	if bmp := a.icon(app.Path); bmp != nil {
		return ui.Image(c, bmp).Size(size, size).Shrink(0)
	}
	ic := icApp
	if !strings.HasSuffix(app.Path, ".app") {
		ic = icTerminal
	}
	return ui.Box(c).Size(size, size).Center().Shrink(0).Children(func() {
		ui.Icon(c, ic).FontSize(size * 0.78).TextColor(c.Theme().TextMuted)
	})
}

// appName is the name to show for an app.
func appName(app api.AppRef) string {
	if app.Name != "" {
		return app.Name
	}
	if app.Path == "" {
		return locales.UnknownApp()
	}
	base := app.Path[strings.LastIndex(app.Path, "/")+1:]
	return strings.TrimSuffix(base, ".app")
}

// appStack shows up to three apps' icons, overlapping a little.
func (a *App) appStack(c *ui.Context, apps []api.AppRef, size float32) {
	ui.Row(c).Shrink(0).Children(func() {
		for i, app := range apps[:min(len(apps), 3)] {
			e := a.appIconView(c, app, size)
			if i > 0 {
				e.Margin(0, 0, 0, -size*0.3)
			}
		}
	})
}

// field is a labeled row of a settings form: the label on the left, the
// control on the right, and an explanation under the label.
func field(c *ui.Context, label, help string, control func()) {
	t := c.Theme()
	ui.Row(c).Gap(16).PaddingY(10).AlignItems(ui.Center).Children(func() {
		ui.Column(c).Grow(1).Gap(3).MinWidth(0).Children(func() {
			ui.Text(c, label)
			if help != "" {
				ui.Text(c, help).FontSize(12).TextColor(t.TextMuted)
			}
		})
		control()
	})
}
