package ui

import (
	"context"
	"fmt"
	"strconv"

	"github.com/egoist/mygo/ui"

	"github.com/Kel077/pith/internal/api"
	"github.com/Kel077/pith/locales"
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
	sheet(c, &k.open, 580, func() {
		sheetTitle(c, locales.CheckAConnection()).Margin(0, 0, 12)

		sheetScroll(c, func() {
			editorRow(c, locales.Scene(), "", func() {
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
					sel := ui.Select(c, &chosen, names).Label(locales.Scene())
					checkSelectLook(c, sel)
					if sel.Changed() {
						for i := range a.cfg.Scenes {
							if names[i] == chosen {
								k.sceneID = a.cfg.Scenes[i].ID
							}
						}
					}
				})
			})
			editorRow(c, locales.App(), "", func() {
				ui.Row(c).Gap(8).AlignItems(ui.Center).Children(func() {
					if k.app.Path != "" {
						a.removableChip(c, func() {
							a.appIconView(c, k.app, 18)
							ui.Text(c, appName(k.app)).SingleLine().AlignSelf(ui.Baseline) // the label lines up with it
						}, func() { k.app = api.AppRef{} })
					} else {
						ui.Text(c, locales.AnyApp()).TextColor(t.TextMuted).Padding(0, 4, 0, 2)
					}
					pick := editorAddButton(c, "choose-app", locales.Choose())
					if pick.Clicked() {
						k.picker.open = !k.picker.open
					}
					a.appPickerPopover(c, pick, &k.picker, func(app api.AppRef) { k.app = app })
				})
			})
			editorRow(c, locales.Destination(), "", func() {
				if sheetInput(c, ui.TextInput(c.Key("check-target"), &k.target)).Placeholder(locales.ADomainAnIpAddressOr()).AutoFocus().Submitted() {
					a.runCheck()
				}
				sheetInput(c, ui.TextInput(c.Key("check-address"), &k.address)).Placeholder(locales.ActualIpOptionalElseLookedUp())
			})
			editorRow(c, locales.Protocol(), "", func() {
				ui.Row(c).Children(func() {
					editorSegmented(c, "protocol", locales.Protocol(), &k.protocol, []string{"TCP", "UDP"}, nil, nil)
				})
			})
			sheetFooter(c, nil, func() {
				if sheetButton(c, "done", locales.Done()).Clicked() {
					k.open = false
				}
				if sheetPrimary(c, "check", locales.Check(), k.target == "" || k.busy).Clicked() {
					a.runCheck()
				}
			})
			if k.result != nil {
				a.checkResult(c, k)
			}
		})
	})
}

// checkSelectLook gives the checker's select the look of a capsule.
func checkSelectLook(c *ui.Context, sel ui.Element) {
	t := c.Theme()
	fill, line := ui.RGBA(255, 255, 255, 0.72), ui.RGBA(60, 40, 20, 0.10)
	if t.Dark {
		fill, line = ui.RGBA(255, 255, 255, 0.07), ui.RGBA(255, 255, 255, 0.10)
	}
	if sel.Hovered() {
		fill = fill.Mix(t.Text, 0.04)
	}
	sel.Padding(7, 14, 7, 16).Radius(999).Background(fill).Border(1, line).MinWidth(170)
}

// checkResult shows what the check found in a pane tinted by it: the
// outcome first, then how each rule compared, in rows.
func (a *App) checkResult(c *ui.Context, k *checker) {
	t := c.Theme()
	res := k.result
	sc := a.scene(k.sceneID)
	stale := k.checked != k.key()
	col, ic := running(t), icArrow
	switch {
	case res.Outcome == api.Blocked:
		col, ic = t.Danger, icBlock
	case res.Interface == "":
		col, ic = t.Warning, icAlert
	}
	fill := col.Alpha(0.08)
	if t.Dark {
		fill = col.Alpha(0.11)
	}
	box := ui.Column(c).Margin(20, 0, 0).Radius(16).Background(fill).Border(1, col.Alpha(0.16)).Dividers(1, hairline(t)).Clip()
	if stale {
		box.Opacity(0.5)
	}
	box.Children(func() {
		ui.Row(c).Gap(12).Padding(14, 16).Children(func() {
			disc := ui.Box(c).Size(34, 34).Radius(17).Background(col.Alpha(0.16)).Center().Shrink(0)
			disc.Children(func() { ui.Icon(c, ic).FontSize(17).TextColor(col) })
			ui.Column(c).Grow(1).Gap(4).MinWidth(0).Children(func() {
				ui.Row(c).Gap(8).Wrap().AlignItems(ui.Center).Children(func() {
					idx := -1
					if sc != nil {
						idx = ruleIndex(sc, res.RuleID)
					}
					switch {
					case res.Outcome == api.Blocked:
						ui.Text(c, locales.CheckBlocked()).FontWeight(600).TextColor(col)
					case res.Interface != "":
						ui.Text(c, locales.GoesOutThrough()).FontWeight(600)
						a.ifaceNameChip(c, res.Interface)
					default:
						ui.Text(c, locales.CanTConnectNoInterfaceOf()).FontWeight(600).TextColor(col)
					}
					if idx >= 0 {
						muted(c, locales.ByRuleNumber(idx+1))
					} else {
						muted(c, locales.NoRuleMatchesTheSystemS())
					}
				})
				if len(res.Addresses) > 0 {
					ui.Text(c, locales.ResolvedTo(joinMax(res.Addresses, 3))).FontSize(12).TextColor(t.TextMuted)
				}
			})
		})
		if sc == nil || len(res.Rules) == 0 {
			return
		}
		for i, rc := range res.Rules {
			k := ruleIndex(sc, rc.RuleID)
			if k < 0 {
				continue
			}
			r := &sc.Rules[k]
			ui.Row(c).Gap(10).Padding(9, 16).Children(func() {
				ui.Text(c, strconv.Itoa(i+1)).FontSize(12).FontWeight(600).TextColor(t.TextMuted).Width(16)
				title := ui.Text(c, a.ruleTitle(r)).SingleLine().Grow(1).Shrink(1).MinWidth(0)
				tone, text := t.TextMuted, ""
				switch rc.Result {
				case api.CheckMatched:
					tone, text = running(t), locales.MatchesDecides()
					title.FontWeight(600)
				case api.CheckShadowed:
					tone, text = t.Warning, locales.MatchesButAnotherRuleDecides()
				case api.CheckDisabled:
					text = locales.RuleOff()
				case api.CheckApp:
					text = locales.OtherApps()
				case api.CheckTarget:
					text = locales.OtherDestinations()
				case api.CheckProtocol:
					text = locales.OtherProtocol()
				}
				ui.Text(c, text).FontSize(12).TextColor(tone).SingleLine()
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
