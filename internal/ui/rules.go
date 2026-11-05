package ui

import (
	"context"

	"encoding/json"
	"slices"
	"strings"

	"github.com/egoist/mygo/plugins/glass"

	"github.com/egoist/mygo/ui"

	"github.com/Kel077/pith/internal/api"
	"github.com/Kel077/pith/locales"
)

// rulesPage is the state of the rules page.
type rulesPage struct {
	list ui.ListState
}

// rulesView shows the rules of a scene, each as a sentence: these apps, to
// these places, go out through these interfaces.
func (a *App) rulesView(c *ui.Context) {
	t := c.Theme()
	sc := a.scene(a.sceneID)
	if sc == nil && len(a.cfg.Scenes) > 0 {
		a.sceneID = a.cfg.Scenes[0].ID
		sc = &a.cfg.Scenes[0]
	}
	pageHeader(c, locales.Rules(), "", func() {
		if glassCapsuleButton(c, "check", icInspect, locales.Check()).Tooltip(locales.SeeWhereAConnectionWouldGo()).Clicked() {
			a.openChecker()
		}
		if primaryCapsule(c, "add-rule", icPlus, locales.AddRule()).Clicked() && sc != nil {
			a.openEditor(sc.ID, -1)
		}
	})
	if a.loadErr != nil {
		a.loadErrorView(c)
		return
	}
	if sc == nil {
		return
	}
	a.sceneBar(c)
	if ns := a.notices(); len(ns) > 0 {
		ui.Column(c).Margin(0, 32, 12).Gap(6).Shrink(0).Children(func() {
			for _, n := range ns {
				noticeBar(c, n, false)
			}
		})
	}
	if a.isRunning() && a.status.SceneID != sc.ID {
		paneRow(c).Margin(0, 32, 12).Padding(10, 10, 10, 16).Gap(10).Children(func() {
			ui.Icon(c, icInfo).TextColor(t.TextMuted)
			muted(c, locales.OtherSceneRunning(sceneName(a.scene(a.status.SceneID)))).Grow(1)
			if ui.Button(c, locales.SwitchToScene(sceneName(sc))).Clicked() {
				a.start(sc.ID)
			}
		})
	}
	id := sc.ID
	if len(sc.Rules) == 0 {
		emptyState(c, icRules, locales.NoRulesYet(), "",
			func() {
				if primaryIconButton(c, icPlus, locales.AddRule()).Clicked() {
					a.openEditor(id, -1)
				}
			})
		return
	}
	a.rules.list.Key = func(i int) any {
		if s := a.scene(id); s != nil && i < len(s.Rules) {
			return s.Rules[i].ID
		}
		return i
	}
	a.rules.list.Reorder = func(rows []int, to int) {
		a.change(func(cfg *api.Config) {
			if s := a.scene(id); s != nil {
				s.Rules = move(s.Rules, rows, to)
			}
		})
	}
	// The list takes the room it needs, and scrolls in its pane when the
	// window is too short; the hint stays under it.
	ui.Column(c).Grow(1).MinHeight(0).Padding(0, 32, 20).Children(func() {
		list := ui.List(c, &a.rules.list, len(sc.Rules), func(i int) {
			if s := a.scene(id); s != nil && i < len(s.Rules) {
				a.ruleRow(c, id, i, &s.Rules[i])
			}
		}).Shrink(1).MinHeight(0).Label(locales.Rules())
		glassy(c, list).Dividers(1, hairline(t)).Clip()
	})
}

// loadErrorView explains that the configuration could not be read, and
// offers to read it again or to start over.
func (a *App) loadErrorView(c *ui.Context) {
	emptyState(c, icAlert, locales.YourRulesCanTBeRead(),
		errorText(a.loadErr)+" "+locales.PithSavesNothingOverTheFile(),
		func() {
			if ui.Button(c, locales.TryAgain()).Clicked() {
				a.reload()
			}
			if ui.Button(c, locales.StartOver()).Clicked() {
				a.confirm = &confirmation{
					title:  locales.StartOverWithNoRules(),
					action: locales.StartOverConfirm(),
					danger: true,
					do: func() {
						a.setConfig(api.DefaultConfig())
						a.change(func(*api.Config) {})
					},
				}
			}
		})
}

// reload reads the configuration again.
func (a *App) reload() {
	var cfg api.Config
	a.async(func(ctx context.Context) (err error) {
		cfg, err = a.Service.LoadConfig(ctx)
		return err
	}, func() { a.setConfig(cfg) }, func(err error) { a.loadErr = err })
}

// sceneBar chooses the scene the page shows, which the switch starts.
func (a *App) sceneBar(c *ui.Context) {
	t := c.Theme()
	ui.Row(c).Padding(0, 32, 14).Gap(8).Shrink(0).Children(func() {
		// The scenes, as a segmented capsule: the arrow keys move among them,
		// and each has the scene's menu on a right click.
		chosen := slices.IndexFunc(a.cfg.Scenes, func(s api.Scene) bool { return s.ID == a.sceneID })
		before := chosen
		seg := ui.SegmentedBase(c.Key("scenes"), &chosen, len(a.cfg.Scenes))
		seg.Track.Padding(4).Gap(2).Radius(999).Material(glass.Glass{}).Border(1, rim(t)).Label(locales.Scene())
		seg.Track.Children(func() {
			for i, s := range a.cfg.Scenes {
				id, name := s.ID, sceneName(&s)
				shown := i == chosen
				e := seg.Segment(i).Gap(6).Padding(6, 14).Radius(999).Cursor(ui.CursorPointer).Label(name)
				switch {
				case shown && t.Dark:
					e.Background(ui.RGBA(255, 255, 255, 0.16)).Shadow(0, 1, 3, 0, ui.RGBA(0, 0, 0, 0.3))
				case shown:
					e.Background(ui.RGBA(255, 255, 255, 0.92)).Shadow(0, 1, 4, 0, ui.RGBA(60, 40, 20, 0.12))
				case e.Hovered():
					e.Background(subtle(t, 0.05))
				}
				e.Children(func() {
					if a.isRunning() && a.status.SceneID == id {
						dot(c, running(t), 7, false).Tooltip(locales.RoutingNow())
					}
					text := ui.Text(c, name).SingleLine()
					if shown {
						text.FontWeight(600)
					} else {
						text.TextColor(t.TextMuted)
					}
				})
				e.ContextMenu(func(m *ui.Menu) { a.sceneMenu(m, id) })
			}
		})
		if chosen != before && chosen >= 0 && chosen < len(a.cfg.Scenes) {
			a.showScene(a.cfg.Scenes[chosen].ID)
		}
		if glassCapsuleButton(c, "scene-add", icPlus, "").Label(locales.NewScene()).Tooltip(locales.NewScene()).Clicked() {
			a.openRename("")
		}
		// How rules decide, for whoever wonders: a tip, not a line of text.
		ui.Box(c.Key("rules-order")).Size(28, 28).Center().Tooltip(locales.BlockRulesAlwaysWinOtherwiseThe()).Children(func() {
			ui.Icon(c, icInfo).FontSize(14).TextColor(t.TextMuted)
		})
		ui.Spacer(c)
		shownID := a.sceneID
		glassCapsuleButton(c, "scene-menu", icMore, "").Label(locales.Scene()).Tooltip(locales.Scene()).
			Menu(func(m *ui.Menu) { a.sceneMenu(m, shownID) })
	})
}

// showScene shows a scene on the page, and makes it the one the switch
// starts.
func (a *App) showScene(id string) {
	a.sceneID = id
	if a.cfg.Selected != id {
		a.change(func(cfg *api.Config) { cfg.Selected = id })
	}
}

func (a *App) sceneMenu(m *ui.Menu, id string) {
	sc := a.scene(id)
	if sc == nil {
		return
	}
	if m.Item(locales.Rename()).Chosen() {
		a.openRename(id)
	}
	if m.Item(locales.DuplicateScene()).Chosen() {
		a.duplicateScene(id)
	}
	m.Separator()
	if m.Item(locales.ExportRules()).Disabled(len(sc.Rules) == 0).Chosen() {
		a.exportRules(id)
	}
	if m.Item(locales.ImportRules()).Chosen() {
		a.importRules(id)
	}
	m.Separator()
	if m.Item(locales.DeleteScene()).Disabled(len(a.cfg.Scenes) < 2).Chosen() {
		name := sceneName(sc)
		a.confirm = &confirmation{
			title:  locales.DeleteSceneTitle(name),
			body:   locales.DeleteSceneBody(len(sc.Rules)),
			action: locales.Delete(),
			danger: true,
			do:     func() { a.deleteScene(id) },
		}
	}
}

func (a *App) duplicateScene(id string) {
	src := a.scene(id)
	if src == nil {
		return
	}
	cp := api.Scene{ID: api.NewID(), Name: locales.SceneCopyName(sceneName(src))}
	for _, r := range src.Rules {
		r = cloneRule(r)
		r.ID = api.NewID()
		cp.Rules = append(cp.Rules, r)
	}
	a.change(func(cfg *api.Config) { cfg.Scenes = append(cfg.Scenes, cp) })
	a.showScene(cp.ID)
}

func (a *App) deleteScene(id string) {
	if len(a.cfg.Scenes) < 2 || a.scene(id) == nil {
		return // a configuration keeps one scene at least
	}
	if a.isRunning() && a.status.SceneID == id {
		a.stop()
	}
	a.change(func(cfg *api.Config) {
		cfg.Scenes = slices.DeleteFunc(cfg.Scenes, func(s api.Scene) bool { return s.ID == id })
		if cfg.Selected == id {
			cfg.Selected = cfg.Scenes[0].ID
		}
	})
	a.sceneID = a.cfg.Selected
}

// ruleRow shows one rule: what it matches on the left, where it sends it on
// the right, and its switch.
func (a *App) ruleRow(c *ui.Context, sceneID string, i int, r *api.Rule) {
	t := c.Theme()
	ruleID := r.ID
	row := ui.Row(c).Gap(12).Padding(13, 16, 13, 8)
	hovered := row.Hovered()
	if hovered {
		row.Background(subtle(t, 0.025))
	}
	row.Cursor(ui.CursorPointer).Label(a.ruleTitle(r))
	row.Children(func() {
		grip := ui.Icon(c, icGrip).FontSize(14).TextColor(t.TextMuted)
		if !hovered {
			grip.Opacity(0)
		}
		body := ui.Row(c).Grow(1).Gap(12).MinWidth(0)
		if !r.Enabled {
			body.Opacity(0.45)
		}
		body.Children(func() {
			if len(r.Apps) > 0 {
				a.appStack(c, r.Apps, 28)
			} else {
				ui.Box(c).Size(28, 28).Radius(8).Background(subtle(t, 0.06)).Center().Shrink(0).Children(func() {
					ic := icOther
					if len(r.Targets) == 0 {
						ic = icApp
					}
					ui.Icon(c, ic).FontSize(16).TextColor(t.TextMuted)
				})
			}
			ui.Column(c).Grow(1).Gap(3).MinWidth(0).Children(func() {
				ui.Row(c).Gap(8).Children(func() {
					ui.Text(c, a.ruleTitle(r)).FontSize(14).FontWeight(600).SingleLine().Shrink(1)
					if r.Note != "" && len(r.Apps) > 0 {
						ui.Text(c, r.Note).FontSize(11).TextColor(t.TextMuted).Padding(1, 6).Radius(4).Background(subtle(t, 0.06)).SingleLine()
					}
				})
				if s := a.scene(sceneID); s != nil {
					if by := shadowedBy(s.Rules, i); by > 0 {
						ui.Row(c).Gap(5).Children(func() {
							ui.Icon(c, icAlert).FontSize(12).TextColor(t.Warning)
							ui.Text(c, locales.RuleNeverApplies(by)).FontSize(12).TextColor(t.Warning).SingleLine()
						})
						return
					}
				}
				ui.Text(c, a.ruleDetail(r)).FontSize(12).TextColor(t.TextMuted).SingleLine()
			})
			a.ruleRoute(c, r)
		})
		on := r.Enabled
		if ui.Switch(c, &on).Label(locales.Enabled()).Changed() {
			a.change(func(cfg *api.Config) {
				if s := a.scene(sceneID); s != nil {
					if k := ruleIndex(s, ruleID); k >= 0 {
						s.Rules[k].Enabled = on
					}
				}
			})
		}
	})
	if row.Clicked() {
		a.openEditor(sceneID, i)
	}
	row.ContextMenu(func(m *ui.Menu) {
		if m.Item(locales.Edit()).Chosen() {
			a.openEditor(sceneID, i)
		}
		if m.Item(locales.DuplicateRule()).Chosen() {
			a.change(func(cfg *api.Config) {
				if s := a.scene(sceneID); s != nil {
					if k := ruleIndex(s, ruleID); k >= 0 {
						cp := cloneRule(s.Rules[k])
						cp.ID = api.NewID()
						s.Rules = slices.Insert(s.Rules, k+1, cp)
					}
				}
			})
		}
		if m.Item(locales.MoveToTop()).Disabled(i == 0).Chosen() {
			a.change(func(cfg *api.Config) {
				if s := a.scene(sceneID); s != nil {
					if k := ruleIndex(s, ruleID); k >= 0 {
						s.Rules = move(s.Rules, []int{k}, 0)
					}
				}
			})
		}
		m.Separator()
		if m.Item(locales.Delete()).Chosen() {
			a.deleteRule(sceneID, ruleID)
		}
	})
}

// ruleRoute shows where a rule sends connections: its interfaces in order,
// and the system's routing when it falls back; or that it blocks them.
func (a *App) ruleRoute(c *ui.Context, r *api.Rule) {
	t := c.Theme()
	ui.Row(c).Gap(6).Shrink(0).Justify(ui.End).Children(func() {
		if r.Action == api.Block {
			tagChip(c, icBlock, locales.Block(), t.Danger)
			return
		}
		ui.Icon(c, icArrow).FontSize(14).TextColor(t.TextMuted)
		for k, v := range r.Via {
			if k > 0 {
				ui.Icon(c, icChevron).FontSize(12).TextColor(t.TextMuted)
			}
			a.ifaceChip(c, v)
		}
		if r.Fallback {
			ui.Icon(c, icChevron).FontSize(12).TextColor(t.TextMuted)
			tagChip(c, nil, locales.SystemRoute(), t.TextMuted).Tooltip(locales.WhenNoneOfTheseConnectThe())
		}
	})
}

// ruleTitle is the first line of a rule: its apps, else its destinations.
func (a *App) ruleTitle(r *api.Rule) string {
	switch {
	case len(r.Apps) > 0:
		names := make([]string, 0, 3)
		for _, app := range r.Apps[:min(len(r.Apps), 2)] {
			names = append(names, appName(app))
		}
		if len(r.Apps) > 2 {
			return locales.AppsAndMore(strings.Join(names, locales.ListComma()), len(r.Apps)-2)
		}
		return list(names)
	case r.Note != "":
		return r.Note
	case len(r.Targets) > 0:
		return targetsSummary(r.Targets)
	}
	return locales.EveryConnection()
}

// ruleDetail is the second line of a rule: the rest of what it matches.
func (a *App) ruleDetail(r *api.Rule) string {
	var parts []string
	switch {
	case len(r.Apps) > 0 && len(r.Targets) > 0:
		parts = append(parts, locales.RuleToTargets(targetsSummary(r.Targets)))
	case len(r.Apps) > 0:
		parts = append(parts, locales.EveryDestination())
	case r.Note != "" && len(r.Targets) > 0:
		parts = append(parts, locales.RuleEveryAppToTargets(targetsSummary(r.Targets)))
	default:
		parts = append(parts, locales.EveryApp())
	}
	switch r.Protocol {
	case api.TCP:
		parts = append(parts, locales.TcpOnly())
	case api.UDP:
		parts = append(parts, locales.UdpOnly())
	}
	if !r.Enabled {
		parts = append(parts, locales.RuleOff())
	}
	return strings.Join(parts, " · ")
}

// targetsSummary names a rule's first destinations.
func targetsSummary(targets []string) string {
	shown := make([]string, 0, 2)
	for _, s := range targets[:min(len(targets), 2)] {
		shown = append(shown, targetLabel(s))
	}
	text := strings.Join(shown, locales.ListComma())
	if len(targets) > 2 {
		return locales.TargetsAndMore(text, len(targets)-2)
	}
	return text
}

// targetLabel writes a target as the interface shows it: a domain with its
// subdomains without its "*.".
func targetLabel(s string) string {
	t, err := api.ParseTarget(s)
	if err != nil {
		return s
	}
	if t.IsDomain() {
		return t.Domain
	}
	return t.String()
}

func ruleIndex(s *api.Scene, id string) int {
	return slices.IndexFunc(s.Rules, func(r api.Rule) bool { return r.ID == id })
}

func (a *App) deleteRule(sceneID, ruleID string) {
	s := a.scene(sceneID)
	if s == nil || ruleIndex(s, ruleID) < 0 {
		return
	}
	var removed api.Rule
	var at int
	a.change(func(cfg *api.Config) {
		if s := a.scene(sceneID); s != nil {
			if k := ruleIndex(s, ruleID); k >= 0 {
				removed, at = s.Rules[k], k
				s.Rules = slices.Delete(s.Rules, k, k+1)
			}
		}
	})
	a.undo = &undoAction{msg: locales.RuleDeleted(), do: func() {
		a.change(func(cfg *api.Config) {
			if s := a.scene(sceneID); s != nil {
				s.Rules = slices.Insert(s.Rules, min(at, len(s.Rules)), removed)
			}
		})
	}}
}

// move moves the items at rows before the item at to (len for the end).
func move[T any](items []T, rows []int, to int) []T {
	rows = slices.Clone(rows)
	slices.Sort(rows)
	moving := make([]T, 0, len(rows))
	set := map[int]bool{}
	for _, r := range rows {
		if r >= 0 && r < len(items) && !set[r] {
			set[r] = true
			moving = append(moving, items[r])
		}
	}
	out := make([]T, 0, len(items))
	for i := 0; i <= len(items); i++ {
		if i == to {
			out = append(out, moving...)
		}
		if i < len(items) && !set[i] {
			out = append(out, items[i])
		}
	}
	return out
}

// ---- import and export ------------------------------------------------------

func (a *App) exportRules(id string) {
	sc := a.scene(id)
	if sc == nil {
		return
	}
	f := api.File{Kind: api.FileRules, Version: api.ConfigVersion, App: a.Version, Exported: a.Now(), Rules: sc.Rules}
	data, _ := json.MarshalIndent(f, "", "  ")
	name := sceneName(sc) + ".pith.json"
	a.async(func(context.Context) error { return a.Desktop.Save(name, data) }, nil, nil)
}

func (a *App) importRules(id string) {
	var file api.File
	a.async(func(context.Context) error {
		data, err := a.Desktop.Open()
		if err != nil || data == nil {
			return err
		}
		f, err := api.ParseFile(data)
		if err != nil {
			return err
		}
		if f.Kind != api.FileRules {
			return &api.Error{Code: api.ErrInvalidConfig, Detail: "this is a backup: import it in Settings"}
		}
		file = f
		return nil
	}, func() {
		var added, skipped int
		a.change(func(cfg *api.Config) {
			s := a.scene(id)
			if s == nil {
				return
			}
			for _, r := range file.Rules {
				r.ID = api.NewID()
				if len(r.Problems()) > 0 {
					skipped++
					continue
				}
				s.Rules = append(s.Rules, r)
				added++
			}
		})
		if len(file.Rules) == 0 && skipped == 0 {
			return
		}
		msg := locales.ImportedRuleCount(added)
		if skipped > 0 {
			msg += locales.ImportSkipped(skipped)
		}
		a.toast(msg)
	}, nil)
}
