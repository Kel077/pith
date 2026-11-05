package ui

import (
	"context"
	"encoding/json"
	"slices"
	"strings"

	"github.com/egoist/mygo/ui"
	"github.com/Kel077/pith/internal/api"
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
	pageHeader(c, tr("Rules", "规则"), func() {
		if textIconButton(c, icInspect, tr("Check", "检查")).Tooltip(tr("See where a connection would go", "看看一个连接会走哪张网卡")).Clicked() {
			a.openChecker()
		}
		if primaryIconButton(c, icPlus, tr("Add Rule", "添加规则")).Clicked() && sc != nil {
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
		ui.Column(c).Margin(0, 24, 10).Gap(6).Shrink(0).Children(func() {
			for _, n := range ns {
				noticeBar(c, n, false)
			}
		})
	}
	if a.isRunning() && a.status.SceneID != sc.ID {
		ui.Row(c).Margin(0, 24, 10).Padding(10, 14).Gap(10).Radius(10).Background(subtle(t, 0.05)).Children(func() {
			ui.Icon(c, icInfo).TextColor(t.TextMuted)
			muted(c, trf("Routing runs by “%s”. These rules apply once you switch.", "正在按「%s」分流，这些规则要切换后才生效。", sceneName(a.scene(a.status.SceneID)))).Grow(1)
			if ui.Button(c, trf("Switch to “%s”", "切换到「%s」", sceneName(sc))).Clicked() {
				a.start(sc.ID)
			}
		})
	}
	id := sc.ID
	if len(sc.Rules) == 0 {
		emptyState(c, icRules, tr("No rules yet", "还没有规则"),
			tr("Rules send apps or destinations out through the network you pick. Everything else goes as usual.",
				"规则把指定的应用或网址送到你选的网卡上，其余连接照常走系统网络。"),
			func() {
				if primaryIconButton(c, icPlus, tr("Add Rule", "添加规则")).Clicked() {
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
	ui.List(c, &a.rules.list, len(sc.Rules), func(i int) {
		if s := a.scene(id); s != nil && i < len(s.Rules) {
			a.ruleRow(c, id, i, &s.Rules[i])
		}
	}).Grow(1).Gap(8).Padding(2, 24, 12).Label(tr("Rules", "规则"))
	ui.Row(c).Padding(10, 28, 14).Gap(6).Shrink(0).BorderWidth(1, 0, 0, 0).BorderColor(t.Border).Children(func() {
		ui.Icon(c, icInfo).FontSize(12).TextColor(t.TextMuted)
		ui.Text(c, tr("Block rules always win; otherwise the first matching rule from the top decides. Drag to reorder.",
			"拦截规则总是优先；其余从上往下，第一条匹配的规则生效。拖动可以调整顺序。")).FontSize(12).TextColor(t.TextMuted)
	})
}

// loadErrorView explains that the configuration could not be read, and
// offers to read it again or to start over.
func (a *App) loadErrorView(c *ui.Context) {
	emptyState(c, icAlert, tr("Your rules can't be read", "无法读取规则"),
		errorText(a.loadErr)+" "+tr("Pith saves nothing over the file until you decide.", "在你决定之前，橘核不会覆盖这个文件。"),
		func() {
			if ui.Button(c, tr("Try Again", "重试")).Clicked() {
				a.reload()
			}
			if ui.Button(c, tr("Start Over…", "重新开始…")).Clicked() {
				a.confirm = &confirmation{
					title:  tr("Start over with no rules?", "清空规则重新开始？"),
					body:   tr("Pith saves an empty configuration in place of the one it can't read.", "橘核会用一份空白配置替换无法读取的那份。"),
					action: tr("Start Over", "重新开始"),
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
	ui.Row(c).Padding(0, 24, 12, 24).Gap(6).Shrink(0).Children(func() {
		for _, s := range a.cfg.Scenes {
			id, name := s.ID, sceneName(&s)
			shown := id == a.sceneID
			pill := ui.Row(c.Key("scene-"+id)).Gap(6).Padding(5, 12).Radius(999).Cursor(ui.CursorPointer).Label(name)
			switch {
			case shown:
				pill.Background(t.Accent.Alpha(0.14))
			case pill.Hovered():
				pill.Background(subtle(t, 0.06))
			}
			pill.Children(func() {
				if a.isRunning() && a.status.SceneID == id {
					dot(c, running(t), 7, false).Tooltip(tr("Routing", "正在分流"))
				}
				text := ui.Text(c, name).SingleLine()
				if shown {
					text.FontWeight(600).TextColor(t.Accent)
				} else {
					text.TextColor(t.TextMuted)
				}
			})
			if pill.Clicked() {
				a.showScene(id)
			}
			pill.ContextMenu(func(m *ui.Menu) { a.sceneMenu(m, id) })
		}
		add := ui.Row(c.Key("scene-add")).Padding(6, 9).Radius(999).Cursor(ui.CursorPointer).Label(tr("New Scene", "新建场景")).Tooltip(tr("New Scene", "新建场景"))
		if add.Hovered() {
			add.Background(subtle(t, 0.06))
		}
		add.Children(func() { ui.Icon(c, icPlus).FontSize(15).TextColor(t.TextMuted) })
		if add.Clicked() {
			a.openRename("")
		}
		ui.Spacer(c)
		shownID := a.sceneID
		ui.Button(c, "").Label(tr("Scene", "场景")).Tooltip(tr("Scene", "场景")).Children(func() {
			ui.Icon(c, icMore)
		}).Menu(func(m *ui.Menu) { a.sceneMenu(m, shownID) })
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
	if m.Item(tr("Rename…", "重命名…")).Chosen() {
		a.openRename(id)
	}
	if m.Item(tr("Duplicate", "复制场景")).Chosen() {
		a.duplicateScene(id)
	}
	m.Separator()
	if m.Item(tr("Export Rules…", "导出规则…")).Disabled(len(sc.Rules) == 0).Chosen() {
		a.exportRules(id)
	}
	if m.Item(tr("Import Rules…", "导入规则…")).Chosen() {
		a.importRules(id)
	}
	m.Separator()
	if m.Item(tr("Delete Scene…", "删除场景…")).Disabled(len(a.cfg.Scenes) < 2).Chosen() {
		name := sceneName(sc)
		a.confirm = &confirmation{
			title:  trf("Delete “%s”?", "删除「%s」？", name),
			body:   trf("Its %d rules go with it.", "其中的 %d 条规则会一起删除。", len(sc.Rules)),
			action: tr("Delete", "删除"),
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
	cp := api.Scene{ID: api.NewID(), Name: trf("%s copy", "%s 副本", sceneName(src))}
	for _, r := range src.Rules {
		r = cloneRule(r)
		r.ID = api.NewID()
		cp.Rules = append(cp.Rules, r)
	}
	a.change(func(cfg *api.Config) { cfg.Scenes = append(cfg.Scenes, cp) })
	a.showScene(cp.ID)
}

func (a *App) deleteScene(id string) {
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
	row := ui.Row(c).Gap(12).Padding(12, 14, 12, 8).Radius(12).Border(1, t.Border).Background(t.Background)
	hovered := row.Hovered()
	if hovered {
		row.Background(subtle(t, 0.03)).Border(1, subtle(t, 0.16))
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
							ui.Text(c, trf("Never applies: rule %d matches all of it first", "不会生效：第 %d 条规则已经覆盖了它", by)).FontSize(12).TextColor(t.Warning).SingleLine()
						})
						return
					}
				}
				ui.Text(c, a.ruleDetail(r)).FontSize(12).TextColor(t.TextMuted).SingleLine()
			})
			a.ruleRoute(c, r)
		})
		on := r.Enabled
		if ui.Switch(c, &on).Label(tr("Enabled", "启用")).Changed() {
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
		if m.Item(tr("Edit…", "编辑…")).Chosen() {
			a.openEditor(sceneID, i)
		}
		if m.Item(tr("Duplicate", "复制")).Chosen() {
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
		if m.Item(tr("Move to Top", "移到最上面")).Disabled(i == 0).Chosen() {
			a.change(func(cfg *api.Config) {
				if s := a.scene(sceneID); s != nil {
					if k := ruleIndex(s, ruleID); k >= 0 {
						s.Rules = move(s.Rules, []int{k}, 0)
					}
				}
			})
		}
		m.Separator()
		if m.Item(tr("Delete", "删除")).Chosen() {
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
			tagChip(c, icBlock, tr("Block", "拦截"), t.Danger)
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
			tagChip(c, nil, tr("System", "系统"), t.TextMuted).Tooltip(tr("When none of these connect, the connection goes as macOS routes it", "以上网卡都连不上时，按系统默认网络连接"))
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
			return trf("%s and %d more", "%s 等 %d 个应用", strings.Join(names, tr(", ", "、")), len(r.Apps))
		}
		return list(names)
	case r.Note != "":
		return r.Note
	case len(r.Targets) > 0:
		return targetsSummary(r.Targets)
	}
	return tr("Every connection", "所有连接")
}

// ruleDetail is the second line of a rule: the rest of what it matches.
func (a *App) ruleDetail(r *api.Rule) string {
	var parts []string
	switch {
	case len(r.Apps) > 0 && len(r.Targets) > 0:
		parts = append(parts, tr("To ", "访问 ")+targetsSummary(r.Targets))
	case len(r.Apps) > 0:
		parts = append(parts, tr("Every destination", "所有目标"))
	case r.Note != "" && len(r.Targets) > 0:
		parts = append(parts, tr("Every app, to ", "所有应用，访问 ")+targetsSummary(r.Targets))
	default:
		parts = append(parts, tr("Every app", "所有应用"))
	}
	switch r.Protocol {
	case api.TCP:
		parts = append(parts, tr("TCP only", "仅 TCP"))
	case api.UDP:
		parts = append(parts, tr("UDP only", "仅 UDP"))
	}
	if !r.Enabled {
		parts = append(parts, tr("Off", "已停用"))
	}
	return strings.Join(parts, " · ")
}

// targetsSummary names a rule's first destinations.
func targetsSummary(targets []string) string {
	shown := make([]string, 0, 2)
	for _, s := range targets[:min(len(targets), 2)] {
		shown = append(shown, targetLabel(s))
	}
	text := strings.Join(shown, tr(", ", "、"))
	if len(targets) > 2 {
		text += trf(" and %d more", " 等 %d 个", len(targets))
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
	a.undo = &undoAction{msg: tr("Rule deleted", "已删除规则"), do: func() {
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
		msg := trf("Imported %d rules", "已导入 %d 条规则", added)
		if skipped > 0 {
			msg += trf(", skipped %d invalid", "，跳过 %d 条无效规则", skipped)
		}
		a.toast(msg)
	}, nil)
}
