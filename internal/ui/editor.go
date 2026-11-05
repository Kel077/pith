package ui

import (
	"context"
	"slices"
	"strconv"
	"strings"

	"github.com/egoist/mygo/ui"
	"github.com/Kel077/pith/internal/api"
)

// editor is the state of the dialog that edits a rule.
type editor struct {
	open       bool
	sceneID    string
	ruleID     string // empty for a new rule
	rule       api.Rule
	targets    []string // as typed, domains without "*."
	subdomains bool     // domains match their subdomains too
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
	hasDomain, allSub := false, true
	for _, s := range e.rule.Targets {
		t, err := api.ParseTarget(s)
		if err == nil && t.IsDomain() {
			hasDomain = true
			allSub = allSub && t.Subdomains
			e.targets = append(e.targets, t.Domain)
		} else if err == nil {
			e.targets = append(e.targets, t.String())
		} else {
			e.targets = append(e.targets, s)
		}
	}
	if hasDomain {
		e.subdomains = allSub
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
	r.Targets = r.Targets[:0]
	for _, s := range e.targets {
		t, err := api.ParseTarget(s)
		if err != nil {
			r.Targets = append(r.Targets, s)
			continue
		}
		t.Subdomains = t.IsDomain() && e.subdomains
		r.Targets = append(r.Targets, t.String())
	}
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
		return r == ',' || r == '，' || r == ';' || r == '\n' || r == ' ' || r == '\t' || r == '、'
	}) {
		t, err := api.ParseTarget(s)
		if err != nil {
			bad = append(bad, s)
			continue
		}
		v := t.String()
		if t.IsDomain() {
			v = t.Domain
			if t.Subdomains {
				e.subdomains = true
			}
		}
		if !slices.Contains(e.targets, v) {
			e.targets = append(e.targets, v)
		}
	}
	e.draft = strings.Join(bad, " ")
	e.draftErr = len(bad) > 0
}

func (a *App) editorView(c *ui.Context) {
	e := a.editor
	t := c.Theme()
	ui.Modal(c, &e.open, func() {
		ui.Column(c).Width(600).Gap(0).Children(func() {
			title := tr("New Rule", "新规则")
			if e.ruleID != "" {
				title = tr("Edit Rule", "编辑规则")
			}
			ui.Text(c, title).FontSize(17).Bold().Margin(0, 0, 14)
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

			editorRow(c, tr("Apps", "应用"), func() {
				ui.Row(c).Gap(6).Wrap().Children(func() {
					for k, app := range e.rule.Apps {
						a.removableChip(c, func() {
							a.appIconView(c, app, 16)
							ui.Text(c, appName(app)).FontSize(13).SingleLine()
						}, func() { e.rule.Apps = slices.Delete(e.rule.Apps, k, k+1) })
					}
					if len(e.rule.Apps) == 0 {
						ui.Text(c, tr("Every app", "所有应用")).TextColor(t.TextMuted).Padding(4, 2)
					}
					add := textIconButton(c, icPlus, tr("Choose…", "选择…"))
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
				hint(c, tr("Leave empty for every app. An app's helpers and the system services that work for it match too.",
					"留空表示所有应用。应用的辅助进程、替它联网的系统服务也会一并匹配。"))
			})

			editorRow(c, tr("Destinations", "目标"), func() {
				ui.Row(c).Gap(6).Wrap().Children(func() {
					for k, s := range e.targets {
						_, err := api.ParseTarget(s)
						a.removableChip(c, func() {
							text := ui.Text(c, s).FontSize(13).SingleLine()
							if err != nil {
								text.TextColor(t.Danger)
							}
						}, func() { e.targets = slices.Delete(e.targets, k, k+1) })
					}
				})
				in := ui.TextInput(c.Key("target-draft"), &e.draft).Placeholder(tr("example.com, 192.168.50.10 or 10.0.0.0/8, then Return", "输入 example.com、192.168.50.10 或 10.0.0.0/8，按回车添加"))
				if e.draftErr {
					in.Border(1, t.Danger)
				}
				if in.Changed() {
					e.draftErr = false
					if strings.ContainsAny(e.draft, ",，\n、") {
						e.addDraft()
					}
				}
				if in.Submitted() {
					e.addDraft()
				}
				ui.Row(c).Gap(14).Children(func() {
					ui.Checkbox(c, &e.subdomains, tr("Domains include their subdomains", "域名包含子域名"))
					ui.Spacer(c)
					switch {
					case e.draftErr:
						ui.Text(c, tr("Not a domain, IP address or range", "不是有效的域名、IP 或网段")).FontSize(12).TextColor(t.Danger)
					case len(e.targets) == 0:
						ui.Text(c, tr("Empty: every destination", "留空表示所有目标")).FontSize(12).TextColor(t.TextMuted)
					}
				})
			})

			editorRow(c, tr("Protocol", "协议"), func() {
				ui.Row(c).Children(func() {
					ui.Segmented(c, &e.protocol, tr("TCP and UDP", "TCP 和 UDP"), "TCP", "UDP").Label(tr("Protocol", "协议"))
				})
			})

			editorRow(c, tr("Action", "动作"), func() {
				ui.Row(c).Children(func() {
					ui.Segmented(c, &e.action, tr("Route through", "走指定网卡"), tr("Block", "拦截")).Label(tr("Action", "动作"))
				})
			})

			if e.action == 0 {
				editorRow(c, tr("Interfaces", "网卡"), func() {
					a.viaEditor(c, e, len(show("via")) > 0)
					ui.Checkbox(c, &e.rule.Fallback, tr("When none of them connects, use the system's network", "以上网卡都连不上时，使用系统默认网络"))
				})
			} else {
				editorRow(c, "", func() {
					hint(c, tr("New matching connections are refused. Connections already open stay.", "匹配的新连接会被拒绝；已经建立的连接不受影响。"))
				})
			}

			editorRow(c, tr("Note", "备注"), func() {
				ui.TextInput(c.Key("note"), &e.rule.Note).Placeholder(tr("Optional, e.g. NAS", "可选，例如 NAS"))
			})

			if e.tried && len(problems) > 0 {
				ui.Row(c).Gap(6).Margin(6, 0, 0, 96).Children(func() {
					ui.Icon(c, icAlert).TextColor(t.Danger)
					ui.Text(c, problemText(problems[0])).TextColor(t.Danger).FontSize(12)
				})
			}

			ui.Row(c).Gap(8).Margin(18, 0, 0).Children(func() {
				if e.ruleID != "" {
					if ui.Button(c, tr("Delete", "删除")).TextColor(t.Danger).Clicked() {
						e.open = false
						a.deleteRule(e.sceneID, e.ruleID)
					}
				}
				ui.Spacer(c)
				if ui.Button(c, tr("Cancel", "取消")).Clicked() {
					e.open = false
				}
				if ui.PrimaryButton(c, tr("Save", "保存")).Clicked() {
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
	})
	if c.Shortcut(ui.Cmd, ui.KeyS) && e.open {
		e.tried = true
	}
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

// viaEditor edits the interfaces of a route rule, in order of preference.
func (a *App) viaEditor(c *ui.Context, e *editor, missing bool) {
	t := c.Theme()
	box := ui.Column(c).Gap(0).Radius(10).Border(1, t.Border)
	if missing {
		box.Border(1, t.Danger)
	}
	box.Children(func() {
		for k, v := range e.rule.Via {
			i := a.iface(v)
			ui.Row(c.Key("via-"+strconv.Itoa(k)+"-"+v.Name)).Gap(10).Padding(7, 8, 7, 12).Children(func() {
				ui.Text(c, strconv.Itoa(k+1)).FontSize(12).TextColor(t.TextMuted).Width(12)
				a.ifaceChip(c, v)
				state := tr("Not connected", "未连接")
				if i != nil && i.Up {
					state = i.Name + " · " + tr("Connected", "已连接")
				} else if i != nil {
					state = i.Name + " · " + state
				}
				ui.Text(c, state).FontSize(12).TextColor(t.TextMuted).Grow(1).SingleLine()
				if quietButton(c, "up", icChevronUp, tr("Move Up", "上移"), k == 0) {
					e.rule.Via = move(e.rule.Via, []int{k}, k-1)
				}
				if quietButton(c, "down", icChevronDown, tr("Move Down", "下移"), k == len(e.rule.Via)-1) {
					e.rule.Via = move(e.rule.Via, []int{k}, k+2)
				}
				if quietButton(c, "remove", icX, tr("Remove", "移除"), false) {
					e.rule.Via = slices.Delete(e.rule.Via, k, k+1)
				}
			})
			ui.Divider(c)
		}
		ui.Row(c).Padding(6, 8).Gap(8).Children(func() {
			ui.Button(c, "").Label(tr("Add Interface", "添加网卡")).Children(func() {
				ui.Icon(c, icPlus)
				ui.Text(c, tr("Add Interface", "添加网卡")).SingleLine()
			}).Menu(func(m *ui.Menu) {
				for _, i := range a.visibleIfaces() {
					ref := refOf(i)
					if slices.ContainsFunc(e.rule.Via, func(v api.InterfaceRef) bool { return v.Resolve([]api.Interface{*i}) != nil }) {
						continue
					}
					label := a.ifaceLabel(ref) + "  ·  " + i.Name
					if !i.Up {
						label += "  ·  " + tr("not connected", "未连接")
					}
					if m.Item(label).Chosen() {
						e.rule.Via = append(e.rule.Via, ref)
					}
				}
			})
			if len(e.rule.Via) > 1 {
				ui.Text(c, tr("Tried from the top: the next one when one can't connect.", "从上往下尝试：前一个连不上时用下一个。")).FontSize(12).TextColor(t.TextMuted)
			} else if missing {
				ui.Text(c, tr("Pick at least one interface.", "至少选择一张网卡。")).FontSize(12).TextColor(t.Danger)
			}
		})
	})
}

// editorRow is a row of the rule editor: its label on the left.
func editorRow(c *ui.Context, label string, body func()) {
	ui.Row(c).Gap(16).AlignItems(ui.Start).PaddingY(8).Children(func() {
		ui.Text(c, label).Width(80).TextColor(c.Theme().TextMuted).Padding(6, 0, 0).Shrink(0)
		ui.Column(c).Grow(1).Gap(8).MinWidth(0).Children(body)
	})
}

// hint is a line of help under a control.
func hint(c *ui.Context, s string) ui.Element {
	return ui.Text(c, s).FontSize(12).TextColor(c.Theme().TextMuted)
}

// removableChip is a chip with a remove button.
func (a *App) removableChip(c *ui.Context, content func(), remove func()) {
	t := c.Theme()
	ui.Row(c).Gap(6).Padding(3, 4, 3, 8).Radius(8).Background(subtle(t, 0.06)).Children(func() {
		content()
		x := ui.Box(c).Size(18, 18).Radius(9).Center().Cursor(ui.CursorPointer).Label(tr("Remove", "移除"))
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
		return tr("A destination is not a domain, an IP address or a range.", "有目标不是有效的域名、IP 或网段。")
	case api.ProblemDuplicate:
		return tr("Something is listed twice.", "有重复的项目。")
	case api.ProblemNoVia:
		return tr("Pick at least one interface.", "至少选择一张网卡。")
	case api.ProblemBadApp:
		return tr("An app has no location.", "有应用缺少位置信息。")
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
// and a way to choose any program.
func (a *App) appPickerPopover(c *ui.Context, anchor ui.Element, p *appPicker, chosen func(api.AppRef)) {
	t := c.Theme()
	ui.Popover(c, anchor, &p.open, func() {
		ui.Column(c).Width(300).Height(360).Gap(8).Padding(4).Children(func() {
			ui.SearchField(c.Key("app-search"), &p.query).Label(tr("Search apps", "搜索应用")).AutoFocus()
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
				row := ui.Row(c.Key(app.Path)).Gap(8).Padding(5, 8).Radius(6).Cursor(ui.CursorPointer)
				if row.Hovered() {
					row.Background(subtle(t, 0.07))
				}
				row.Children(func() {
					a.appIconView(c, app, 20)
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
						muted(c, tr("Finding apps…", "正在查找应用…"))
					})
				case len(shown) == 0:
					muted(c, tr("No app matches.", "没有匹配的应用。")).Padding(12)
				}
			})
			ui.Divider(c)
			if textIconButton(c, icTerminal, tr("Other Program…", "其他程序…")).Clicked() {
				p.open = false
				var path string
				a.async(func(context.Context) (err error) {
					path, err = a.Desktop.ChooseProgram()
					return err
				}, func() {
					if path != "" {
						chosen(programRef(path))
					}
				}, nil)
			}
		})
	})
}

// programRef refers to a program the user chose by its path.
func programRef(path string) api.AppRef {
	ref := api.AppRef{Path: strings.TrimSuffix(path, "/")}
	ref.Name = appName(ref)
	return ref
}
