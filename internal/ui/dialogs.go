package ui

import (
	"strings"

	"github.com/egoist/mygo/plugins/glass"
	"github.com/egoist/mygo/ui"

	"github.com/Kel077/pith/internal/api"
	"github.com/Kel077/pith/locales"
)

// installPrompt asks to install the helper before routing starts.
type installPrompt struct {
	open   bool
	then   string // the scene to start once installed
	update bool   // an outdated helper is updated
}

// confirmation asks before something that can't be undone.
type confirmation struct {
	open   bool
	title  string
	body   string
	action string
	danger bool
	do     func()
}

// renameDialog names a new scene, or renames one.
type renameDialog struct {
	open bool
	id   string // empty for a new scene
	name string
}

// undoAction is a toast that offers to undo what was just done.
type undoAction struct {
	msg string
	do  func()
}

// dialogs shows the dialogs that are open, and the toasts.
func (a *App) dialogs(c *ui.Context) {
	if a.undo != nil {
		u := a.undo
		a.undo = nil
		c.ToastAction(u.msg, locales.Undo(), u.do)
	}
	if a.editor != nil {
		a.editorView(c)
		if !a.editor.open {
			a.editor = nil
		}
	}
	if a.checker != nil {
		a.checkerView(c)
		if !a.checker.open {
			a.checker = nil
		}
	}
	if a.install.open {
		a.installView(c)
	}
	if a.rename.open {
		a.renameView(c)
	}
	if a.hotspot.unlock.open {
		a.unlockView(c)
	}
	if a.confirm != nil {
		a.confirmView(c)
	}
	glassToasts(c)
}

// ---- the look of dialogs ----------------------------------------------------

// sheetGlass is the material of sheets, of the popovers over them and of
// toasts: glass frosted toward paper until it is nearly opaque, so that
// what is on it reads as on paper, one even pane whatever the window
// shows under it, dark pictures and bright glows alike.
func sheetGlass(t *ui.Theme) ui.Material {
	paper := ui.RGBA(255, 254, 252, 1)
	if t.Dark {
		paper = ui.RGBA(36, 36, 40, 1)
	}
	return frosted{glass.Glass{Tint: paper.Alpha(0.66)}, paper.Alpha(0.92)}
}

// frosted is glass under a face of a color: a material as opaque as the
// face makes it, beyond the eighth of what is under it that even an
// opaque tint lets glass show. What shows of it is the glass's, blurred
// and toned.
type frosted struct {
	glass glass.Glass
	face  ui.Color
}

// PaintMaterial paints the glass, and the face over it, under the
// element's rim and children.
func (f frosted) PaintMaterial(p *ui.Painter, box ui.Rect, radii [4]float32) {
	f.glass.PaintMaterial(p, box, radii)
	p.Fill(box, f.face, radii[0])
}

// sheet shows a dialog of Pith's look while *open is true: a sheet of
// frosted glass, its rim lit, lifted off the window by a deep, soft
// shadow, over the window dimmed and blurred a little. A click outside it
// or Escape closes it, which Dismissed reports. fn builds its content, top
// to bottom.
func sheet(c *ui.Context, open *bool, width float32, fn func()) ui.Element {
	t := c.Theme()
	w, h := c.Size()
	return ui.DialogBase(c, open, func(backdrop, panel ui.Element) {
		dim, shadow := ui.RGBA(52, 34, 18, 0.14), ui.RGBA(84, 50, 18, 0.20)
		if t.Dark {
			dim, shadow = ui.RGBA(0, 0, 0, 0.38), ui.RGBA(0, 0, 0, 0.55)
		}
		backdrop.Material(glass.Blur{Radius: 2}).Draw(func(p *ui.Painter, r ui.Rect) { p.Fill(r, dim, 0) })
		panel.Width(width).MaxWidth(w-40).MaxHeight(h-40).Padding(24, 26, 22).Radius(22).
			Material(sheetGlass(t)).Border(1, rim(t)).
			Shadow(0, 28, 72, -14, shadow).Shadow(0, 2, 8, -2, shadow.Alpha(0.08))
		fn()
	})
}

// sheetScroll holds the body of a sheet, between its title and its
// buttons, which scrolls when the window is too short for it. It reaches
// a little into the sheet's padding, so as not to clip the rings and
// shadows of what is at its edges.
func sheetScroll(c *ui.Context, body func()) {
	ui.Scroll(c).Shrink(1).MinHeight(0).Padding(4, 6).Margin(-4, -6).Children(body)
}

// sheetTitle is the title of a sheet.
func sheetTitle(c *ui.Context, s string) ui.Element {
	return ui.Text(c, s).FontSize(19).FontWeight(700).LetterSpacing(-0.2)
}

// sheetFooter holds a sheet's buttons, on the right: lead builds what
// goes on the left, if anything.
func sheetFooter(c *ui.Context, lead func(), buttons func()) {
	ui.Row(c).Gap(10).Margin(22, 0, 0).AlignItems(ui.Center).Children(func() {
		if lead != nil {
			lead()
		}
		ui.Spacer(c)
		buttons()
	})
}

// sheetChoices holds the buttons of a sheet that asks one question, as an
// alert does: side by side, as wide as each other, across the sheet. Give
// them Grow(1).Basis(0).
func sheetChoices(c *ui.Context, buttons func()) {
	ui.Row(c).Gap(10).Margin(22, 0, 0).Children(buttons)
}

// sheetButton is a quiet button of a sheet: a capsule of glass that takes
// the keyboard focus, as the system's buttons do, set apart from the
// sheet (onSheet).
func sheetButton(c *ui.Context, key, label string) ui.Element {
	return onSheet(c, glassCapsuleButton(c, key, nil, label).Padding(8, 18).Center().Focusable())
}

// onSheet sets a capsule of glass apart from the sheet it is on, and
// returns it. On a light sheet, glass is as pale as the paper under it
// and its rim of light vanishes, leaving a button that reads as text:
// there its glass takes a shade of the text, deeper under the pointer,
// and its rim a hairline. On a dark sheet, glass stands out already.
func onSheet(c *ui.Context, b ui.Element) ui.Element {
	t := c.Theme()
	if t.Dark {
		return b
	}
	shade := float32(0.07)
	if b.Hovered() {
		shade = 0.11
	}
	return b.Material(glass.Glass{Tint: subtle(t, shade)}).Border(1, subtle(t, 0.1))
}

// sheetPrimary is the main action of a sheet: the tangerine capsule,
// faded while it can't be taken.
func sheetPrimary(c *ui.Context, key, label string, disabled bool) ui.Element {
	b := primaryCapsule(c, key, nil, label).Padding(8, 20).Center().Focusable().Disabled(disabled)
	if disabled {
		b.Opacity(0.5).Cursor(ui.CursorDefault)
	}
	return b
}

// sheetDangerGradient is the fill of an action that destroys something:
// red, lit from above as the tangerine of the primary action is.
func sheetDangerGradient(t *ui.Theme) ui.LinearGradient {
	from, to := ui.Hex("#ff6d64"), ui.Hex("#e0433b")
	if t.Dark {
		from, to = ui.Hex("#ff7a70"), ui.Hex("#e8524a")
	}
	return ui.LinearGradient{From: from, To: to, Angle: 180, Oklab: true}
}

// sheetDanger is the action of a sheet that destroys something: a red
// capsule.
func sheetDanger(c *ui.Context, key, label string) ui.Element {
	t := c.Theme()
	b := ui.Row(c.Key(key)).Padding(8, 20).Radius(999).Center().Cursor(ui.CursorPointer).Label(label).Focusable().
		LinearGradient(sheetDangerGradient(t)).Shadow(0, 4, 14, -4, t.Danger.Alpha(0.45))
	if b.Pressed() {
		b.Opacity(0.85)
	}
	b.Children(func() { ui.Text(c, label).FontWeight(600).TextColor(ui.Hex("#ffffff")).SingleLine() })
	return b
}

// sheetInput gives a text input of a sheet its look: a soft well, whose
// rim lights in the accent while it has the focus. It returns in.
func sheetInput(c *ui.Context, in ui.Element) ui.Element {
	t := c.Theme()
	fill, line := ui.RGBA(255, 255, 255, 0.72), ui.RGBA(60, 40, 20, 0.10)
	if t.Dark {
		fill, line = ui.RGBA(255, 255, 255, 0.06), ui.RGBA(255, 255, 255, 0.10)
	}
	return in.Padding(8, 12).Radius(10).Background(fill).Border(1, line)
}

// sheetBadge is the mark at the top of a sheet that asks something: an
// icon on a rounded tile of glass tinted with col.
func sheetBadge(c *ui.Context, ic *ui.SVG, col ui.Color) ui.Element {
	t := c.Theme()
	b := ui.Box(c).Size(52, 52).Radius(16).Material(glass.Glass{Tint: col.Alpha(0.16)}).Border(1, rim(t)).Center().Shrink(0)
	b.Children(func() { ui.Icon(c, ic).FontSize(26).TextColor(col) })
	return b
}

// glassToasts builds the window's toasts in Pith's look, in place of the
// theme's: capsules of glass at the foot of the window, each with its
// message and its action. Call it once in a view.
func glassToasts(c *ui.Context) {
	t := c.Theme()
	w, _ := c.Size()
	vibrant := c.Vibrancy()
	ui.ToastViewportBase(c, func(viewport ui.Element, toasts []ui.Toast) {
		viewport.Padding(0, 16, 24).AlignItems(ui.Center).Gap(8)
		for _, ts := range toasts {
			p := ui.ToastBase(c, ts)
			box := p.Root.Row().AlignItems(ui.Center).Gap(12).Padding(10, 20).Radius(20).MaxWidth(min(w-32, 560)).
				Border(1, rim(t)).Shadow(0, 12, 32, -8, ui.RGBA(40, 24, 8, 0.24))
			if ts.Action != "" {
				box.Padding(6, 6, 6, 18)
			}
			// Over the system's material, as in the menu bar's panel, glass
			// would show the panel's emptiness: a translucent face instead.
			switch {
			case vibrant && t.Dark:
				box.Background(ui.RGBA(52, 52, 56, 0.92))
			case vibrant:
				box.Background(ui.RGBA(255, 255, 255, 0.92))
			default:
				box.Material(sheetGlass(t))
			}
			box.Transition(ui.ElementTransition{Enter: &ui.Motion{Y: 10}, Exit: &ui.Motion{Y: 6}})
			box.Children(func() {
				ui.Column(c).Shrink(1).MinWidth(0).Gap(2).Children(func() {
					title := ui.Text(c, ts.Title)
					if ts.Description != "" {
						title.FontWeight(600)
						ui.Text(c, ts.Description).FontSize(12).TextColor(t.TextMuted)
					}
				})
				if ts.Action == "" {
					return
				}
				b := p.ActionButton().Padding(6, 14).Radius(999).Background(t.Accent.Alpha(0.14)).Cursor(ui.CursorPointer)
				if b.Hovered() {
					b.Background(t.Accent.Alpha(0.22))
				}
				b.Children(func() { ui.Text(c, ts.Action).FontWeight(600).TextColor(t.Accent).SingleLine() })
			})
		}
	})
}

// ---- the dialogs ------------------------------------------------------------

func (a *App) installView(c *ui.Context) {
	t := c.Theme()
	p := &a.install
	sheet(c, &p.open, 380, func() {
		ui.Column(c).Gap(8).AlignItems(ui.Center).Padding(6, 4, 0).Children(func() {
			// What the helper does and does not, for whoever wonders.
			sheetBadge(c, icShield, t.Accent).Margin(0, 0, 8).Tooltip(strings.Join([]string{
				locales.PithRoutesConnectionsWithASmall(),
				locales.ItOnlyRoutesWhileYouTurn(),
				locales.ItNeverReadsWhatAppsSend(),
				locales.YouCanRemoveItInSettings(),
			}, "\n"))
			title := locales.InstallTheHelper()
			if p.update {
				title = locales.UpdateTheHelper()
			}
			sheetTitle(c, title).TextAlign(ui.Center)
			ui.Text(c, locales.MacosAsksForYourPassword()).TextColor(t.TextMuted).TextAlign(ui.Center)
		})
		sheetChoices(c, func() {
			if sheetButton(c, "not-now", locales.NotNow()).Grow(1).Basis(0).Clicked() {
				p.open = false
			}
			label := locales.Install()
			if p.update {
				label = locales.Update()
			}
			if sheetPrimary(c, "install", label, a.busy != "").Grow(1).Basis(0).Clicked() {
				p.open = false
				a.installHelper(p.then)
			}
		})
	})
}

func (a *App) openRename(id string) {
	a.rename = renameDialog{open: true, id: id}
	if s := a.scene(id); s != nil {
		a.rename.name = sceneName(s)
	}
}

func (a *App) renameView(c *ui.Context) {
	d := &a.rename
	sheet(c, &d.open, 360, func() {
		title := locales.NewScene()
		if d.id != "" {
			title = locales.RenameScene()
		}
		sheetTitle(c, title)
		in := sheetInput(c, ui.TextInput(c.Key("scene-name"), &d.name)).Placeholder(locales.EGOfficeHome()).AutoFocus().Margin(14, 0, 0)
		submit := in.Submitted()
		sheetFooter(c, nil, func() {
			if sheetButton(c, "cancel", locales.Cancel()).Clicked() {
				d.open = false
			}
			ok := sheetPrimary(c, "done", locales.Done(), strings.TrimSpace(d.name) == "").Clicked()
			if (ok || submit) && strings.TrimSpace(d.name) != "" {
				name := strings.TrimSpace(d.name)
				d.open = false
				if d.id == "" {
					s := api.Scene{ID: api.NewID(), Name: name}
					a.change(func(cfg *api.Config) { cfg.Scenes = append(cfg.Scenes, s) })
					a.showScene(s.ID)
					return
				}
				id := d.id
				a.change(func(cfg *api.Config) {
					if s := a.scene(id); s != nil {
						s.Name = name
					}
				})
			}
		})
	})
}

// confirmView asks before something that can't be undone, as an alert:
// the question, what it would cost if anything, and the two answers.
func (a *App) confirmView(c *ui.Context) {
	t := c.Theme()
	k := a.confirm
	k.open = true
	sheet(c, &k.open, 340, func() {
		ui.Column(c).Gap(6).AlignItems(ui.Center).Padding(6, 4, 0).Children(func() {
			if k.danger {
				sheetBadge(c, icAlert, t.Danger).Margin(0, 0, 8)
			}
			sheetTitle(c, k.title).TextAlign(ui.Center)
			if k.body != "" {
				muted(c, k.body).TextAlign(ui.Center)
			}
		})
		sheetChoices(c, func() {
			if sheetButton(c, "cancel", locales.Cancel()).Grow(1).Basis(0).Clicked() {
				k.open = false
			}
			var b ui.Element
			if k.danger {
				b = sheetDanger(c, "action", k.action)
			} else {
				b = sheetPrimary(c, "action", k.action, false)
			}
			if b.Grow(1).Basis(0).Clicked() {
				k.open = false
				k.do()
			}
		})
	})
	if !k.open {
		a.confirm = nil
	}
}
