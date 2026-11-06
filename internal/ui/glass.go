package ui

import (
	"github.com/egoist/mygo/plugins/glass"
	"github.com/egoist/mygo/ui"
	"math"
)

// Pith's look: a calm background with soft glows of tangerine, rose and
// periwinkle, and panes of light glass over it, which show the glows
// through. Navigation and actions are capsules; lists are rows divided by
// hairlines inside one pane, as in System Settings, not boxes.

// glow is a soft light in the background: a color fading out from a point.
type glow struct {
	x, y   float32 // the center, in fractions of the window
	radius float32 // in DIPs
	light  ui.Color
	dark   ui.Color
}

var glows = []glow{
	{0.05, 0.0, 720, ui.RGBA(255, 158, 84, 0.34), ui.RGBA(255, 136, 56, 0.20)},
	{0.98, 0.12, 620, ui.RGBA(255, 150, 178, 0.20), ui.RGBA(214, 104, 156, 0.13)},
	{0.62, 1.08, 820, ui.RGBA(132, 160, 255, 0.24), ui.RGBA(86, 108, 255, 0.18)},
}

// ambient makes e the window's background: the base and its glows,
// painted under e's children. It returns e.
func ambient(c *ui.Context, e ui.Element) ui.Element {
	dark := c.Theme().Dark
	base := ui.Hex("#f6f4f1")
	if dark {
		base = ui.Hex("#121214")
	}
	return e.Background(base).Draw(func(p *ui.Painter, r ui.Rect) {
		for _, g := range glows {
			col := g.light
			if dark {
				col = g.dark
			}
			p.FillRadialGradient(r, ui.RadialGradient{
				Center: ui.Point{X: g.x, Y: g.y},
				Radius: g.radius,
				Oklab:  true,
				Stops:  []ui.GradientStop{{Color: col, At: 0}, {Color: col.Alpha(0), At: 1}},
			}, 0)
		}
	})
}

// hairline is the color of the lines that divide rows inside a pane.
func hairline(t *ui.Theme) ui.Color {
	if t.Dark {
		return ui.RGBA(255, 255, 255, 0.07)
	}
	return ui.RGBA(0, 0, 0, 0.06)
}

// rim is the thin border of a pane: light catching its edge.
func rim(t *ui.Theme) ui.Color {
	if t.Dark {
		return ui.RGBA(255, 255, 255, 0.10)
	}
	return ui.RGBA(255, 255, 255, 0.75)
}

// glassy gives any element the look of a pane of light glass: the
// background's glows show through it, softly, under a thin rim and a soft
// shadow. Rows, columns and lists take it alike.
func glassy(c *ui.Context, e ui.Element) ui.Element {
	t := c.Theme()
	shadow := ui.RGBA(70, 45, 20, 0.07)
	if t.Dark {
		shadow = ui.RGBA(0, 0, 0, 0.32)
	}
	return e.Radius(18).Material(glass.Glass{Clarity: 0.55}).Border(1, rim(t)).Shadow(0, 10, 32, -6, shadow)
}

// pane is a column of light glass.
func pane(c *ui.Context) ui.Element { return glassy(c, ui.Column(c)) }

// paneRow is a row of light glass, as a banner.
func paneRow(c *ui.Context) ui.Element { return glassy(c, ui.Row(c)).AlignItems(ui.Center) }

// paneRows is a pane whose children are rows divided by hairlines.
func paneRows(c *ui.Context) ui.Element {
	return pane(c).Dividers(1, hairline(c.Theme()))
}

// capsule is a small pane of glass shaped as a capsule: navigation, the
// routing switch, quiet buttons. Hovered, its glass takes a little of the
// text's color, as a control does under the pointer.
func capsule(c *ui.Context) ui.Element {
	t := c.Theme()
	e := ui.Row(c).Radius(999).Border(1, rim(t))
	g := glass.Glass{}
	if e.Hovered() {
		g.Tint = subtle(t, 0.06)
	}
	return e.Material(g)
}

// accentGradient is the fill of the primary action: tangerine, lit from
// above.
func accentGradient(t *ui.Theme) ui.LinearGradient {
	from, to := ui.Hex("#ffa456"), ui.Hex("#ee761b")
	if t.Dark {
		from, to = ui.Hex("#ffae63"), ui.Hex("#f2852c")
	}
	return ui.LinearGradient{From: from, To: to, Angle: 180, Oklab: true}
}

// controlHeight is the height of the capsules, buttons, fields and
// drop-downs that stand side by side.
const controlHeight = 34

// primaryCapsule is the primary action of a page: a tangerine capsule. It
// takes the keyboard focus, and Space or Return presses it.
func primaryCapsule(c *ui.Context, key string, ic *ui.SVG, label string) ui.Element {
	t := c.Theme()
	b := ui.Row(c.Key(key)).Gap(6).Height(controlHeight).Padding(0, 16, 0, 14).AlignItems(ui.Center).Radius(999).Cursor(ui.CursorPointer).Label(label).
		Focusable().LinearGradient(accentGradient(t)).Shadow(0, 4, 14, -4, t.Accent.Alpha(0.45))
	switch {
	case b.Pressed():
		b.Opacity(0.85)
	case b.Hovered():
		b.Shadow(0, 6, 18, -4, t.Accent.Alpha(0.55))
	}
	b.Children(func() {
		if ic != nil {
			ui.Icon(c, ic).FontSize(14).TextColor(ui.Hex("#ffffff"))
		}
		ui.Text(c, label).FontWeight(600).TextColor(ui.Hex("#ffffff")).SingleLine()
	})
	return b
}

// glassCapsuleButton is a quiet action: a capsule of glass with an icon,
// and a label unless empty. It takes the keyboard focus.
func glassCapsuleButton(c *ui.Context, key string, ic *ui.SVG, label string) ui.Element {
	t := c.Theme()
	b := capsule(c.Key(key)).Gap(6).Cursor(ui.CursorPointer).Label(label).Focusable()
	if label == "" {
		b.Size(controlHeight, controlHeight).Center()
	} else {
		b.Height(controlHeight).Padding(0, 14, 0, 12).AlignItems(ui.Center)
	}
	b.Children(func() {
		if ic != nil {
			ui.Icon(c, ic).FontSize(14).TextColor(t.Text)
		}
		if label != "" {
			ui.Text(c, label).FontWeight(500).SingleLine()
		}
	})
	return b
}

// segmentedCapsule chooses one of a few options: a capsule of glass whose
// chosen segment is a brighter pill inside it. The arrow keys move the
// choice, and screen readers read it as radio buttons in a group named
// label. It reports a new choice.
func segmentedCapsule(c *ui.Context, key, label string, chosen *int, labels []string, icons []*ui.SVG) bool {
	t := c.Theme()
	before := *chosen
	seg := ui.SegmentedBase(c.Key(key), chosen, len(labels))
	g := glass.Glass{}
	seg.Track.Height(controlHeight).Padding(3).Gap(2).Radius(999).Material(g).Border(1, rim(t)).Label(label)
	seg.Track.Children(func() {
		for i, text := range labels {
			s := seg.Segment(i).Gap(6).Height(controlHeight-8).Padding(0, 14).AlignItems(ui.Center).Radius(999).Cursor(ui.CursorPointer).Label(text)
			on := i == *chosen
			switch {
			case on && t.Dark:
				s.Background(ui.RGBA(255, 255, 255, 0.16)).Shadow(0, 1, 3, 0, ui.RGBA(0, 0, 0, 0.3))
			case on:
				s.Background(ui.RGBA(255, 255, 255, 0.92)).Shadow(0, 1, 4, 0, ui.RGBA(60, 40, 20, 0.12))
			case s.Hovered():
				s.Background(subtle(t, 0.05))
			}
			s.Children(func() {
				if icons != nil && icons[i] != nil {
					ic := ui.Icon(c, icons[i]).FontSize(14)
					if on {
						ic.TextColor(t.Accent)
					} else {
						ic.TextColor(t.TextMuted)
					}
				}
				// As wide as it is bold, chosen or not: choosing doesn't move
				// the segments.
				bold, _ := c.MeasureText(0, ui.Span{Text: text, Weight: 600})
				label := ui.Text(c, text).SingleLine().MinWidth(float32(math.Ceil(float64(bold)))).TextAlign(ui.Center)
				if on {
					label.FontWeight(600)
				} else {
					label.TextColor(t.TextMuted)
				}
			})
		}
	})
	return *chosen != before
}
