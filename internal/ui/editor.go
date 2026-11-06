package ui

import (
	"context"
	"fmt"
	"math"
	"slices"
	"strconv"
	"strings"

	"github.com/egoist/mygo/ui"

	"github.com/Kel077/pith/internal/api"
	"github.com/Kel077/pith/locales"
)

// editor is the state of the dialog that edits a rule.
type editor struct {
	open       bool
	sceneID    string
	ruleID     string // empty for a new rule
	rule       api.Rule
	targets    []string // in ParseTarget's canonical form ("*.example.com"), or as typed when invalid
	subdomains bool     // domains added from now on include their subdomains
	draft      string   // the target being typed
	draftErr   bool
	protocol   int // 0 any, 1 TCP, 2 UDP
	action     int // 0 route, 1 block
	picker     appPicker
	tried      bool // Save was pressed: show problems
}

var protocols = []api.Protocol{api.AnyProtocol, api.TCP, api.UDP}

// openEditor edits the rule at index i of a scene, or a new one for -1.
func (a *App) openEditor(sceneID string, i int) {
	e := &editor{open: true, sceneID: sceneID, subdomains: true}
	sc := a.scene(sceneID)
	if sc != nil && i >= 0 && i < len(sc.Rules) {
		e.rule = cloneRule(sc.Rules[i])
		e.ruleID = e.rule.ID
	} else {
		e.rule = api.Rule{Enabled: true, Protocol: api.AnyProtocol, Action: api.Route, Fallback: true}
		// A new rule starts with the primary interface, the likeliest pick.
		for k := range a.ifaces {
			if a.ifaces[k].Primary {
				e.rule.Via = []api.InterfaceRef{refOf(&a.ifaces[k])}
			}
		}
	}
	// Each target keeps whether it includes subdomains: the check box only
	// says it for the domains added next.
	for _, s := range e.rule.Targets {
		if t, err := api.ParseTarget(s); err == nil {
			s = t.String()
		}
		e.targets = append(e.targets, s)
	}
	e.protocol = max(0, slices.Index(protocols, e.rule.Protocol))
	if e.rule.Action == api.Block {
		e.action = 1
	}
	a.editor = e
}

// prepared is the rule as the dialog would save it.
func (e *editor) prepared() api.Rule {
	r := cloneRule(e.rule)
	r.Protocol = protocols[e.protocol]
	r.Action = api.Route
	if e.action == 1 {
		r.Action = api.Block
	}
	r.Targets = slices.Clone(e.targets)
	if len(r.Targets) == 0 {
		r.Targets = nil
	}
	r.Note = strings.TrimSpace(r.Note)
	return r
}

// addDraft adds what is typed as targets: several at once, separated by
// spaces, commas or lines, as a pasted list. What does not parse stays.
func (e *editor) addDraft() {
	var bad []string
	for _, s := range strings.FieldsFunc(e.draft, func(r rune) bool {
		return r == ',' || r == '\uff0c' || r == ';' || r == '\n' || r == ' ' || r == '\t' || r == '\u3001'
	}) {
		t, err := api.ParseTarget(s)
		if err != nil {
			bad = append(bad, s)
			continue
		}
		if t.IsDomain() && !t.Subdomains {
			// "*.x" and ".x" say it themselves; a bare domain follows the box.
			t.Subdomains = e.subdomains
		}
		if v := t.String(); !slices.ContainsFunc(e.targets, func(o string) bool { return sameTarget(o, v) }) {
			e.targets = append(e.targets, v)
		}
	}
	e.draft = strings.Join(bad, " ")
	e.draftErr = len(bad) > 0
}

func (a *App) editorView(c *ui.Context) {
	e := a.editor
	t := c.Theme()
	sheet(c, &e.open, 620, func() {
		title := locales.NewRule()
		if e.ruleID != "" {
			title = locales.EditRule()
		}
		sheetTitle(c, title).Margin(0, 0, 12)
		rule := e.prepared()
		problems := rule.Problems()
		show := func(field string) []api.Problem {
			var ps []api.Problem
			for _, p := range problems {
				if p.Field == field && (e.tried || p.Code != api.ProblemNoVia) {
					ps = append(ps, p)
				}
			}
			return ps
		}

		// Every label the form may show, so that its controls don't move as
		// rows come and go.
		lw := editorLabelWidth(c, locales.Apps(), locales.Destinations(), locales.Protocol(), locales.Action(), locales.Interfaces(), locales.Note())
		sheetScroll(c, func() {
			editorRow(c, lw, locales.Apps(), locales.LeaveEmptyForEveryAppAn(), func() {
				ui.Row(c).Gap(6).Wrap().AlignItems(ui.Center).Children(func() {
					for k, app := range e.rule.Apps {
						a.appChip(c, app, func() { e.rule.Apps = slices.Delete(e.rule.Apps, k, k+1) })
					}
					if len(e.rule.Apps) == 0 {
						ui.Text(c, locales.EveryApp()).TextColor(t.TextMuted).Padding(0, 4, 0, 2)
					}
					add := editorAddButton(c, "choose-app", locales.Choose())
					if add.Clicked() {
						e.picker.open = !e.picker.open
						e.picker.query = ""
					}
					a.appPickerPopover(c, add, &e.picker, func(app api.AppRef) {
						if !slices.ContainsFunc(e.rule.Apps, func(x api.AppRef) bool { return x.Path == app.Path }) {
							e.rule.Apps = append(e.rule.Apps, app)
						}
					})
				})
			})

			editorRow(c, lw, locales.Destinations(), locales.TargetPlaceholder(), func() {
				if len(e.targets) > 0 {
					ui.Row(c).Gap(6).Wrap().AlignItems(ui.Center).Children(func() {
						for k, s := range e.targets {
							tg, err := api.ParseTarget(s)
							a.removableChip(c, func() {
								switch {
								case err != nil:
									ui.Text(c, s).SingleLine().TextColor(t.Danger)
								case tg.IsDomain():
									ui.Text(c, tg.Domain).SingleLine()
									if subdomainToggle(c, k, tg.Subdomains) {
										tg.Subdomains = !tg.Subdomains
										e.targets[k] = tg.String()
									}
								default:
									ui.Text(c, tg.String()).SingleLine()
								}
							}, func() { e.targets = slices.Delete(e.targets, k, k+1) })
						}
					})
				}
				placeholder := locales.TargetPlaceholderShort()
				if len(e.targets) == 0 {
					placeholder = locales.EveryDestination() + " · " + placeholder
				}
				in := sheetInput(c, ui.TextInput(c.Key("target-draft"), &e.draft)).Placeholder(placeholder)
				if e.draftErr {
					in.Border(1, t.Danger)
				}
				if in.Changed() {
					e.draftErr = false
					if strings.ContainsAny(e.draft, ",\uff0c\n\u3001") {
						e.addDraft()
					}
				}
				if in.Submitted() {
					e.addDraft()
				}
				if e.draftErr {
					ui.Text(c, locales.NotADomainIpAddressOr()).FontSize(12).TextColor(t.Danger).Margin(-4, 0, 0, 4)
				}
				ui.Row(c).Children(func() {
					ui.Checkbox(c, &e.subdomains, locales.NewDomainsIncludeTheirSubdomains())
				})
			})

			editorRow(c, lw, locales.Protocol(), "", func() {
				ui.Row(c).Children(func() {
					editorSegmented(c, "protocol", locales.Protocol(), &e.protocol, []string{locales.TcpAndUdp(), "TCP", "UDP"}, nil, nil)
				})
			})

			editorRow(c, lw, locales.Action(), "", func() {
				ui.Row(c).Children(func() {
					editorSegmented(c, "action", locales.Action(), &e.action, []string{locales.RouteThrough(), locales.Block()},
						[]*ui.SVG{icArrow, icBlock}, []string{"", locales.NewMatchingConnectionsAreRefusedConnections()})
				})
			})

			if e.action == 0 {
				editorRow(c, lw, locales.Interfaces(), locales.TriedFromTheTopTheNext(), func() {
					a.viaEditor(c, e, len(show("via")) > 0)
					ui.Checkbox(c, &e.rule.Fallback, locales.WhenNoneOfThemConnectsUse())
				})
			}

			editorRow(c, lw, locales.Note(), "", func() {
				sheetInput(c, ui.TextInput(c.Key("note"), &e.rule.Note)).Placeholder(locales.OptionalEGNas())
			})

			if e.tried && len(problems) > 0 {
				ui.Row(c).Gap(6).Margin(6, 0, 0, lw+editorLabelGap).Children(func() {
					ui.Icon(c, icAlert).FontSize(14).TextColor(t.Danger)
					ui.Text(c, problemText(problems[0])).TextColor(t.Danger).FontSize(12)
				})
			}
		})

		sheetFooter(c, func() {
			if e.ruleID != "" && sheetQuietDanger(c, "delete", icTrash, locales.Delete()).Clicked() {
				e.open = false
				a.deleteRule(e.sceneID, e.ruleID)
			}
		}, func() {
			if sheetButton(c, "cancel", locales.Cancel()).Clicked() {
				e.open = false
			}
			save := sheetPrimary(c, "save", locales.Save(), false).Clicked() || c.Shortcut(ui.Cmd, ui.KeyS)
			if save {
				if e.draft != "" {
					e.addDraft()
					rule = e.prepared()
					problems = rule.Problems()
				}
				e.tried = true
				if len(problems) == 0 && !e.draftErr {
					a.saveRule(e.sceneID, e.ruleID, rule)
					e.open = false
				}
			}
		})
	})
}

// subdomainToggle is the chip's switch of whether a domain includes its
// subdomains: a small pill, filled while it does and outlined while it
// doesn't, in the quiet tones of a tag, the accent being for actions. It
// keeps its size either way, and reports a click.
func subdomainToggle(c *ui.Context, k int, on bool) bool {
	t := c.Theme()
	label := locales.Subdomains()
	pill := ui.Row(c.Key(fmt.Sprint("sub-", k))).Padding(2, 8).Radius(999).Cursor(ui.CursorPointer).Label(label)
	if on {
		fill := t.TextMuted.Alpha(0.12)
		if t.Dark {
			fill = t.TextMuted.Alpha(0.2)
		}
		pill.Background(fill).Border(1, ui.Transparent).Tooltip(locales.IncludesItsSubdomainsClickForThis())
	} else {
		pill.Border(1, subtle(t, 0.14)).Tooltip(locales.ThisDomainAloneClickToInclude())
	}
	pill.Children(func() {
		ui.Text(c, label).FontSize(11).FontWeight(500).TextColor(t.TextMuted).SingleLine()
	})
	return pill.Clicked()
}

// sameTarget reports whether two targets are the same, written alike or not.
func sameTarget(x, y string) bool {
	tx, ex := api.ParseTarget(x)
	ty, ey := api.ParseTarget(y)
	if ex != nil || ey != nil {
		return x == y
	}
	return tx.String() == ty.String()
}

// saveRule saves a rule the editor made: new rules go first, as they are
// most often exceptions to the rules below them.
func (a *App) saveRule(sceneID, ruleID string, r api.Rule) {
	a.change(func(cfg *api.Config) {
		s := a.scene(sceneID)
		if s == nil {
			return
		}
		if k := ruleIndex(s, ruleID); ruleID != "" && k >= 0 {
			r.ID = ruleID
			s.Rules[k] = r
			return
		}
		r.ID = api.NewID()
		s.Rules = slices.Insert(s.Rules, 0, r)
	})
}

// viaEditor edits the interfaces of a route rule, in order of preference:
// a pane of glass whose rows are the interfaces, and at its foot the
// button that adds one.
func (a *App) viaEditor(c *ui.Context, e *editor, missing bool) {
	t := c.Theme()
	box := paneRows(c)
	if missing {
		box.Border(1, t.Danger.Alpha(0.7))
	}
	// Each row: the ordinal, in a column of its own, then the chip.
	const inset, ordinal, gap = 14, 12, 10
	box.Children(func() {
		for k, v := range e.rule.Via {
			i := a.iface(v)
			ui.Row(c.Key("via-"+strconv.Itoa(k)+"-"+v.Name)).Gap(gap).Padding(7, 8, 7, inset).Children(func() {
				ui.Text(c, strconv.Itoa(k+1)).FontSize(12).FontWeight(600).TextColor(t.TextMuted).Width(ordinal)
				a.ifaceChip(c, v)
				state := locales.NotConnected()
				if i != nil && i.Up {
					state = i.Name + " · " + locales.Connected()
				} else if i != nil {
					state = i.Name + " · " + state
				}
				ui.Text(c, state).FontSize(12).TextColor(t.TextMuted).Grow(1).SingleLine()
				if quietButton(c, "up", icChevronUp, locales.MoveUp(), k == 0) {
					e.rule.Via = move(e.rule.Via, []int{k}, k-1)
				}
				if quietButton(c, "down", icChevronDown, locales.MoveDown(), k == len(e.rule.Via)-1) {
					e.rule.Via = move(e.rule.Via, []int{k}, k+2)
				}
				if quietButton(c, "remove", icX, locales.Remove(), false) {
					e.rule.Via = slices.Delete(e.rule.Via, k, k+1)
				}
			})
		}
		// The button stands in the chips' column, under them.
		ui.Row(c).Padding(7, 8, 7, inset+ordinal+gap).Gap(gap).Children(func() {
			editorAddButton(c, "add-interface", locales.AddInterface()).Menu(func(m *ui.Menu) {
				for _, i := range a.visibleIfaces() {
					ref := refOf(i)
					if slices.ContainsFunc(e.rule.Via, func(v api.InterfaceRef) bool { return v.Resolve([]api.Interface{*i}) != nil }) {
						continue
					}
					label := a.ifaceLabel(ref) + "  ·  " + i.Name
					if !i.Up {
						label += "  ·  " + locales.NotConnectedLower()
					}
					if m.Item(label).Chosen() {
						e.rule.Via = append(e.rule.Via, ref)
					}
				}
			})
			if missing {
				ui.Text(c, locales.PickAtLeastOneInterface()).FontSize(12).TextColor(t.Danger)
			}
		})
	})
}

// editorLabelGap is the room between the labels of a sheet's form and
// their controls.
const editorLabelGap = 16

// editorLabelWidth is the width of the column of the labels of a sheet's
// form: that of the widest of labels, as the language shown writes them,
// so that the controls stand as near their labels in every language.
func editorLabelWidth(c *ui.Context, labels ...string) float32 {
	var w float32
	for _, l := range labels {
		lw, _ := c.MeasureText(0, ui.Span{Text: l})
		w = max(w, lw)
	}
	return float32(math.Ceil(float64(w)))
}

// editorRow is a row of a sheet's form: its label on the left, in a
// column labelWidth wide (editorLabelWidth), level with the first line of
// its control, which tip, if any, explains on hover.
func editorRow(c *ui.Context, labelWidth float32, label, tip string, body func()) {
	t := c.Theme()
	ui.Row(c).Gap(editorLabelGap).AlignItems(ui.Baseline).PaddingY(7).Children(func() {
		l := ui.Text(c, label).MinWidth(labelWidth).TextColor(t.TextMuted).Shrink(0)
		if tip != "" {
			l.Tooltip(tip)
		}
		ui.Column(c).Grow(1).Gap(10).MinWidth(0).Children(body)
	})
}

// editorAddButton is a small capsule of glass that adds something, as
// glassCapsuleButton is, level with the chips beside it: a plus and its
// label, by whose line the label of its row lines up. It takes the
// keyboard focus.
func editorAddButton(c *ui.Context, key, label string) ui.Element {
	t := c.Theme()
	b := capsule(c.Key(key)).Gap(5).Padding(5, 13, 5, 10).Cursor(ui.CursorPointer).Label(label).Focusable()
	if b.Hovered() {
		b.Background(subtle(t, 0.04))
	}
	b.Children(func() {
		ui.Icon(c, icPlus).FontSize(14).TextColor(t.Text)
		ui.Text(c, label).FontWeight(500).SingleLine().AlignSelf(ui.Baseline)
	})
	return b
}

// editorSegmented chooses one of a few options as segmentedCapsule does, a
// capsule whose chosen segment is a brighter pill, built on the toolkit's
// segmented control so that it keeps the keyboard (Tab, the arrows) and
// reads as a radio group named label. On a sheet, its capsule is a faint
// shade rather than glass, which on the sheet's white would leave the
// chosen pill without contrast. icons and tips may be nil, or have an
// icon and an explanation for each segment. It reports a new choice.
func editorSegmented(c *ui.Context, key, label string, chosen *int, labels []string, icons []*ui.SVG, tips []string) bool {
	t := c.Theme()
	seg := ui.SegmentedBase(c.Key(key), chosen, len(labels))
	track := seg.Track.Padding(3).Gap(2).Radius(999).Background(subtle(t, 0.05)).Border(1, subtle(t, 0.06)).Label(label)
	track.Children(func() {
		for i, l := range labels {
			s := seg.Segment(i).Gap(6).Padding(5, 14).Radius(999).Cursor(ui.CursorPointer).Label(l)
			on := i == *chosen
			switch {
			case on && t.Dark:
				s.Background(ui.RGBA(255, 255, 255, 0.16)).Shadow(0, 1, 3, 0, ui.RGBA(0, 0, 0, 0.3))
			case on:
				s.Background(ui.RGBA(255, 255, 255, 0.95)).Shadow(0, 1, 4, 0, ui.RGBA(60, 40, 20, 0.14))
			case s.Hovered():
				s.Background(subtle(t, 0.05))
			}
			if i < len(tips) && tips[i] != "" {
				s.Tooltip(tips[i])
			}
			s.Children(func() {
				if i < len(icons) && icons[i] != nil {
					ic := ui.Icon(c, icons[i]).FontSize(14)
					if on {
						ic.TextColor(t.Accent)
					} else {
						ic.TextColor(t.TextMuted)
					}
				}
				text := ui.Text(c, l).SingleLine().AlignSelf(ui.Baseline) // the label lines up with it, not the icon
				if on {
					text.FontWeight(600)
				} else {
					text.TextColor(t.TextMuted)
				}
			})
		}
	})
	return track.Changed()
}

// sheetQuietDanger is a quiet action of a sheet that destroys something:
// a capsule of glass, its icon and label red.
func sheetQuietDanger(c *ui.Context, key string, ic *ui.SVG, label string) ui.Element {
	t := c.Theme()
	b := capsule(c.Key(key)).Gap(6).Padding(8, 16, 8, 13).Cursor(ui.CursorPointer).Label(label).Focusable()
	if b.Hovered() {
		b.Background(t.Danger.Alpha(0.1))
	}
	b.Children(func() {
		ui.Icon(c, ic).FontSize(14).TextColor(t.Danger)
		ui.Text(c, label).FontWeight(500).TextColor(t.Danger).SingleLine()
	})
	return b
}

// appChip shows an app as a chip that removes it: its icon, and its name
// centered on it. The name's line is as high as the icon, and aligned by
// its baseline, which makes that baseline the chip's: the label of the
// form's row, and the chips and buttons beside it, line up with it.
func (a *App) appChip(c *ui.Context, app api.AppRef, remove func()) {
	const icon = 18
	a.removableChip(c, func() {
		a.appIconView(c, app, icon)
		ui.Text(c, appName(app)).SingleLine().FixedLineHeight(icon).AlignSelf(ui.Baseline)
	}, remove)
}

// removableChip is a soft pill with a button that removes it.
func (a *App) removableChip(c *ui.Context, content func(), remove func()) {
	t := c.Theme()
	fill, line := ui.RGBA(255, 255, 255, 0.82), ui.RGBA(60, 40, 20, 0.09)
	if t.Dark {
		fill, line = ui.RGBA(255, 255, 255, 0.08), ui.RGBA(255, 255, 255, 0.08)
	}
	chip := ui.Row(c).Gap(6).Padding(3, 4, 3, 7).Radius(999).Background(fill).Border(1, line)
	if !t.Dark {
		chip.Shadow(0, 1, 2, 0, ui.RGBA(60, 40, 20, 0.06))
	}
	chip.Children(func() {
		ui.Row(c).Gap(6).Padding(0, 0, 0, 3).Children(content)
		x := ui.Box(c).Size(20, 20).Radius(10).Center().Cursor(ui.CursorPointer).Label(locales.Remove())
		if x.Hovered() {
			x.Background(subtle(t, 0.1))
		}
		x.Children(func() { ui.Icon(c, icX).FontSize(11).TextColor(t.TextMuted) })
		if x.Clicked() {
			remove()
		}
	})
}

// problemText explains a problem of a rule.
func problemText(p api.Problem) string {
	switch p.Code {
	case api.ProblemBadTarget:
		return locales.ADestinationIsNotADomain()
	case api.ProblemDuplicate:
		return locales.SomethingIsListedTwice()
	case api.ProblemNoVia:
		return locales.PickAtLeastOneInterface()
	case api.ProblemBadApp:
		return locales.AnAppHasNoLocation()
	}
	return string(p.Code)
}

// ---- choosing apps ----------------------------------------------------------

// appPicker is the state of the popover that chooses an app.
type appPicker struct {
	open  bool
	query string
	list  ui.ListState
}

// appPickerPopover lists the installed apps under anchor, with a search,
// and a way to choose any program: a popover of the sheets' glass, its
// apps in rounded rows.
func (a *App) appPickerPopover(c *ui.Context, anchor ui.Element, p *appPicker, chosen func(api.AppRef)) {
	t := c.Theme()
	ui.PopoverBase(c, anchor, &p.open, func(panel ui.Element) {
		shadow := ui.RGBA(70, 40, 12, 0.22)
		if t.Dark {
			shadow = ui.RGBA(0, 0, 0, 0.5)
		}
		panel.Margin(6, 0, 0, 0).Padding(8).Radius(16).Material(sheetGlass(t)).Border(1, rim(t)).Shadow(0, 16, 44, -10, shadow)
		ui.Column(c).Width(300).Height(360).Gap(6).Children(func() {
			search := ui.SearchField(c.Key("app-search"), &p.query).Label(locales.SearchApps()).Placeholder(locales.SearchApps()).AutoFocus()
			sheetInput(c, search).Padding(7, 10).Margin(0, 0, 2)
			apps := a.installedApps()
			var shown []api.AppRef
			q := strings.ToLower(p.query)
			for _, app := range apps {
				if q == "" || strings.Contains(strings.ToLower(app.Name), q) || strings.Contains(strings.ToLower(app.BundleID), q) {
					shown = append(shown, app)
				}
			}
			ui.List(c, &p.list, len(shown), func(i int) {
				app := shown[i]
				row := ui.Row(c.Key(app.Path)).Gap(10).Padding(5, 8).Radius(9).Cursor(ui.CursorPointer)
				if row.Hovered() {
					row.Background(subtle(t, 0.07))
				}
				row.Children(func() {
					a.appIconView(c, app, 22)
					ui.Text(c, appName(app)).SingleLine().Grow(1)
				})
				if row.Clicked() {
					chosen(app)
					p.open = false
				}
			}).Grow(1).Children(func() {
				switch {
				case apps == nil:
					ui.Row(c).Padding(12).Gap(8).Children(func() {
						ui.Spinner(c)
						muted(c, locales.FindingApps())
					})
				case len(shown) == 0:
					muted(c, locales.NoAppMatches()).Padding(12)
				}
			})
			ui.Box(c).Height(1).Margin(0, 4).Background(hairline(t)).Shrink(0)
			other := ui.Row(c.Key("other-program")).Gap(10).Padding(6, 8).Radius(9).Cursor(ui.CursorPointer).
				Label(locales.OtherProgram()).Focusable()
			if other.Hovered() {
				other.Background(subtle(t, 0.07))
			}
			other.Children(func() {
				ui.Box(c).Size(22, 22).Center().Children(func() { ui.Icon(c, icTerminal).FontSize(17).TextColor(t.TextMuted) })
				ui.Text(c, locales.OtherProgram()).SingleLine()
			})
			if other.Clicked() {
				p.open = false
				var path string
				a.async(func(context.Context) (err error) {
					path, err = a.Desktop.ChooseProgram()
					return err
				}, func() {
					if path != "" {
						chosen(a.programRef(path))
					}
				}, nil)
			}
		})
	})
}

// programRef refers to a program the user chose by its path: an installed
// app with its bundle ID and name, as Pith lists it.
func (a *App) programRef(path string) api.AppRef {
	path = strings.TrimSuffix(path, "/")
	for _, app := range a.apps {
		if app.Path == path {
			return app
		}
	}
	ref := api.AppRef{Path: path}
	ref.Name = appName(ref)
	return ref
}
