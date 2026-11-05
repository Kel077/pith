package ui

import (
	"context"
	"fmt"
	"strconv"

	"github.com/egoist/mygo/ui"
	"github.com/Kel077/pith/internal/api"
)

// checker is the state of the dialog that checks where a connection goes.
type checker struct {
	open     bool
	sceneID  string
	app      api.AppRef
	target   string
	address  string
	protocol int // 0 TCP, 1 UDP
	picker   appPicker
	result   *api.CheckResult
	checked  string // the input result answers
	busy     bool
}

func (a *App) openChecker() {
	a.checker = &checker{open: true, sceneID: a.sceneID}
}

func (k *checker) key() string {
	return k.sceneID + "|" + k.app.Path + "|" + k.target + "|" + k.address + "|" + strconv.Itoa(k.protocol)
}

func (a *App) runCheck() {
	k := a.checker
	if k.target == "" || k.busy {
		return
	}
	in := api.CheckInput{SceneID: k.sceneID, App: k.app, Target: k.target, Address: k.address, Protocol: []api.Protocol{api.TCP, api.UDP}[k.protocol]}
	key := k.key()
	k.busy = true
	var res api.CheckResult
	a.async(func(ctx context.Context) (err error) {
		res, err = a.Service.Check(ctx, in)
		return err
	}, func() {
		k.busy, k.result, k.checked = false, &res, key
	}, func(error) { k.busy = false })
}

func (a *App) checkerView(c *ui.Context) {
	k := a.checker
	t := c.Theme()
	ui.Modal(c, &k.open, func() {
		ui.Column(c).Width(560).Gap(0).Children(func() {
			ui.Text(c, tr("Check a Connection", "检查连接")).FontSize(17).Bold()
			ui.Text(c, tr("See which rule a connection would match, and where it would go now.", "看看一个连接会命中哪条规则、现在会从哪张网卡出去。")).
				TextColor(t.TextMuted).Margin(4, 0, 12)

			editorRow(c, tr("Scene", "场景"), func() {
				names := make([]string, len(a.cfg.Scenes))
				chosen := ""
				seen := map[string]int{}
				for i := range a.cfg.Scenes {
					names[i] = sceneName(&a.cfg.Scenes[i])
					if seen[names[i]]++; seen[names[i]] > 1 {
						names[i] += fmt.Sprintf(" (%d)", seen[names[i]]) // two scenes of one name
					}
					if a.cfg.Scenes[i].ID == k.sceneID {
						chosen = names[i]
					}
				}
				ui.Row(c).Children(func() {
					if ui.Select(c, &chosen, names).Label(tr("Scene", "场景")).Changed() {
						for i := range a.cfg.Scenes {
							if names[i] == chosen {
								k.sceneID = a.cfg.Scenes[i].ID
							}
						}
					}
				})
			})
			editorRow(c, tr("App", "应用"), func() {
				ui.Row(c).Gap(8).Children(func() {
					if k.app.Path != "" {
						a.removableChip(c, func() {
							a.appIconView(c, k.app, 16)
							ui.Text(c, appName(k.app)).FontSize(13).SingleLine()
						}, func() { k.app = api.AppRef{} })
					} else {
						ui.Text(c, tr("Any app", "任意应用")).TextColor(t.TextMuted).Padding(4, 2)
					}
					pick := textIconButton(c, icPlus, tr("Choose…", "选择…"))
					if pick.Clicked() {
						k.picker.open = !k.picker.open
					}
					a.appPickerPopover(c, pick, &k.picker, func(app api.AppRef) { k.app = app })
				})
			})
			editorRow(c, tr("Destination", "目标"), func() {
				if ui.TextInput(c.Key("check-target"), &k.target).Placeholder(tr("A domain, an IP address or a URL", "域名、IP 地址或网址")).AutoFocus().Submitted() {
					a.runCheck()
				}
				ui.TextInput(c.Key("check-address"), &k.address).Placeholder(tr("Actual IP (optional; else looked up)", "实际 IP（可选，不填则自动解析）"))
			})
			editorRow(c, tr("Protocol", "协议"), func() {
				ui.Row(c).Children(func() { ui.Segmented(c, &k.protocol, "TCP", "UDP").Label(tr("Protocol", "协议")) })
			})
			ui.Row(c).Gap(8).Margin(8, 0, 0).Children(func() {
				ui.Spacer(c)
				if ui.Button(c, tr("Done", "完成")).Clicked() {
					k.open = false
				}
				if ui.PrimaryButton(c, tr("Check", "检查")).Disabled(k.target == "" || k.busy).Clicked() {
					a.runCheck()
				}
			})
			if k.result != nil {
				a.checkResult(c, k)
			}
		})
	})
}

// checkResult shows what the check found: the outcome first, then how each
// rule compared.
func (a *App) checkResult(c *ui.Context, k *checker) {
	t := c.Theme()
	res := k.result
	sc := a.scene(k.sceneID)
	stale := k.checked != k.key()
	box := ui.Column(c).Margin(16, 0, 0).Padding(14).Gap(10).Radius(12).Background(subtle(t, 0.04))
	if stale {
		box.Opacity(0.5)
	}
	box.Children(func() {
		ui.Row(c).Gap(8).Wrap().Children(func() {
			idx := -1
			if sc != nil {
				idx = ruleIndex(sc, res.RuleID)
			}
			switch {
			case res.Outcome == api.Blocked:
				tagChip(c, icBlock, tr("Blocked", "会被拦截"), t.Danger)
			case res.Interface != "":
				ui.Text(c, tr("Goes out through", "会从这里出去：")).FontWeight(600)
				a.ifaceNameChip(c, res.Interface)
			default:
				tagChip(c, icAlert, tr("Can't connect: no interface of its rule is up", "无法连接：规则里的网卡都没有连接"), t.Warning)
			}
			if idx >= 0 {
				muted(c, trf("by rule %d", "依据第 %d 条规则", idx+1))
			} else {
				muted(c, tr("no rule matches: the system's routing", "没有规则命中，按系统默认网络"))
			}
		})
		if len(res.Addresses) > 0 {
			ui.Text(c, tr("Resolved to ", "解析为 ")+joinMax(res.Addresses, 3)).FontSize(12).TextColor(t.TextMuted)
		}
		if sc == nil || len(res.Rules) == 0 {
			return
		}
		ui.Divider(c)
		for i, rc := range res.Rules {
			k := ruleIndex(sc, rc.RuleID)
			if k < 0 {
				continue
			}
			r := &sc.Rules[k]
			ui.Row(c).Gap(10).Children(func() {
				ui.Text(c, strconv.Itoa(i+1)).FontSize(12).TextColor(t.TextMuted).Width(14)
				ui.Text(c, a.ruleTitle(r)).SingleLine().Grow(1).Shrink(1)
				col, text := t.TextMuted, ""
				switch rc.Result {
				case api.CheckMatched:
					col, text = running(t), tr("Matches — decides", "命中，生效")
				case api.CheckShadowed:
					col, text = t.Warning, tr("Matches, but another rule decides", "也匹配，但被优先的规则覆盖")
				case api.CheckDisabled:
					text = tr("Off", "已停用")
				case api.CheckApp:
					text = tr("Other apps", "应用不同")
				case api.CheckTarget:
					text = tr("Other destinations", "目标不同")
				case api.CheckProtocol:
					text = tr("Other protocol", "协议不同")
				}
				ui.Text(c, text).FontSize(12).TextColor(col).SingleLine()
			})
		}
	})
}

func joinMax(items []string, n int) string {
	s := ""
	for i, it := range items[:min(len(items), n)] {
		if i > 0 {
			s += ", "
		}
		s += it
	}
	if len(items) > n {
		s += " …"
	}
	return s
}
