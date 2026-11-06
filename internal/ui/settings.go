package ui

import (
	"context"
	"encoding/json"

	"github.com/egoist/mygo/i18n"
	"github.com/egoist/mygo/plugins/glass"
	"github.com/egoist/mygo/ui"

	"github.com/Kel077/pith/internal/api"
	"github.com/Kel077/pith/locales"
)

// settingsPage is the state of the settings page.
type settingsPage struct{}

const repoURL = "https://github.com/Kel077/pith"

const (
	// settingsWidth is as wide as the settings get: wider, a control
	// would be too far from its label.
	settingsWidth = 760
	// settingsInset is how far the rows of a group, and its title, keep
	// from the group's edges.
	settingsInset = 16
)

// settingsView shows the settings as System Settings does: groups of rows
// under small titles, each row a label on the left and its control on the
// right.
func (a *App) settingsView(c *ui.Context) {
	t := c.Theme()
	pageHeader(c, locales.Settings(), "", nil)
	s := a.cfg.Settings
	set := func(fn func(s *api.Settings)) {
		a.change(func(cfg *api.Config) { fn(&cfg.Settings) })
	}
	ui.Scroll(c).Grow(1).Padding(0, 32, 32).Children(func() {
		ui.Column(c).MaxWidth(settingsWidth).Gap(24).Children(func() {
			group(c, locales.General(), func() {
				settingsRow(c, locales.OpenAtLogin(), locales.PithWaitsInTheMenuBar(), false, func() {
					on := s.OpenAtLogin
					if ui.Switch(c, &on).Label(locales.OpenAtLogin()).Changed() {
						set(func(s *api.Settings) { s.OpenAtLogin = on })
						a.async(func(context.Context) error { return a.Desktop.SetOpenAtLogin(on) }, nil, nil)
					}
				})
				settingsRow(c, locales.KeepRoutingAfterQuitting(), locales.OffQuittingPithStopsRouting(), false, func() {
					on := s.KeepRouting
					if ui.Switch(c, &on).Label(locales.KeepRoutingAfterQuitting()).Changed() {
						set(func(s *api.Settings) { s.KeepRouting = on })
					}
				})
				settingsRow(c, locales.ResumeRoutingOnLaunch(), locales.AfterARestartRouteAgainIf(), false, func() {
					on := s.ResumeOnLaunch
					if ui.Switch(c, &on).Label(locales.ResumeRoutingOnLaunch()).Changed() {
						set(func(s *api.Settings) { s.ResumeOnLaunch = on })
					}
				})
				settingsRow(c, locales.SpeedInTheMenuBar(), "", false, func() {
					on := s.MenuBarSpeed
					if ui.Switch(c, &on).Label(locales.SpeedInTheMenuBar()).Changed() {
						set(func(s *api.Settings) { s.MenuBarSpeed = on })
						a.trayState = ""
					}
				})
				settingsRow(c, locales.Language(), "", false, func() {
					options := []string{locales.LanguageSystem(), i18n.Autonym("en"), i18n.Autonym("zh-Hans")}
					values := []string{"", "en", "zh-Hans"}
					chosen := options[0]
					for k, v := range values {
						if v == s.Language {
							chosen = options[k]
						}
					}
					if settingsSelect(c, &chosen, options).Label(locales.Language()).Changed() {
						for k, o := range options {
							if o == chosen {
								v := values[k]
								set(func(s *api.Settings) { s.Language = v })
								a.langSet = v
								a.Desktop.SetLanguage(v)
							}
						}
					}
				})
			})

			group(c, locales.RoutingSwitch(), func() {
				settingsRow(c, locales.RecognizeDomains(), locales.ReadTheHostNameAppsConnect(), true, func() {
					on := s.SniffDomains
					if ui.Switch(c, &on).Label(locales.RecognizeDomains()).Changed() {
						set(func(s *api.Settings) { s.SniffDomains = on })
					}
				})
				settingsRow(c, "IPv6", locales.RouteIpv6ConnectionsToo(), true, func() {
					on := s.IPv6
					if ui.Switch(c, &on).Label("IPv6").Changed() {
						set(func(s *api.Settings) { s.IPv6 = on })
					}
				})
				settingsRow(c, locales.SafeStart(), locales.IfTheMacLosesTheInternet(), true, func() {
					on := s.SafeStart
					if ui.Switch(c, &on).Label(locales.SafeStart()).Changed() {
						set(func(s *api.Settings) { s.SafeStart = on })
					}
				})
				settingsRow(c, locales.KeepTheConnectionLog(), "", false, func() {
					options := []string{locales.UntilQuitting(), locales.LogKeep1Day(), locales.LogKeep7Days(), locales.LogKeep30Days()}
					values := []int{0, 1, 7, 30}
					chosen := options[2]
					for k, v := range values {
						if v == s.LogDays {
							chosen = options[k]
						}
					}
					if settingsSelect(c, &chosen, options).Label(locales.KeepTheConnectionLog()).Changed() {
						for k, o := range options {
							if o == chosen {
								v := values[k]
								set(func(s *api.Settings) { s.LogDays = v })
							}
						}
					}
				})
			})

			group(c, locales.Helper(), func() {
				h := a.status.Helper
				settingsRow(c, locales.RoutingHelper(), "", false, func() {
					ui.Row(c).Gap(14).Children(func() {
						a.helperState(c, h)
						switch {
						case a.busy == "install" || a.busy == "uninstall":
							ui.Spinner(c)
						case !h.Installed:
							if primaryCapsule(c, "install", icDownload, locales.Install()).Focusable().Clicked() {
								a.installHelper("")
							}
						case helperUntrusted(a.status):
							ui.Row(c).Gap(8).Children(func() {
								if primaryCapsule(c, "reinstall", icRefresh, locales.Reinstall()).Focusable().Clicked() {
									a.installHelper("")
								}
								a.uninstallButton(c)
							})
						case helperNotResponding(a.status):
							ui.Row(c).Gap(8).Children(func() {
								if primaryCapsule(c, "repair", icRefresh, locales.Repair()).Focusable().Clicked() {
									a.installHelper("")
								}
								a.uninstallButton(c)
							})
						case h.Outdated:
							if primaryCapsule(c, "update", icRefresh, locales.Update()).Focusable().Clicked() {
								a.installHelper("")
							}
						default:
							a.uninstallButton(c)
						}
					})
				})
			})

			group(c, locales.Data(), func() {
				settingsRow(c, locales.BackUpEverything(), locales.ScenesRulesInterfaceNamesAndSettings(), false, func() {
					ui.Row(c).Gap(8).Children(func() {
						if glassCapsuleButton(c, "export", icUpload, locales.Export()).Focusable().Clicked() {
							a.exportBackup()
						}
						if glassCapsuleButton(c, "import", icDownload, locales.Import()).Focusable().Clicked() {
							a.importBackup()
						}
					})
				})
			})

			group(c, locales.About(), func() {
				ui.Row(c).Gap(14).Padding(14, settingsInset).Children(func() {
					// The mark of the app, as its icon shows it: on tangerine.
					ui.Box(c).Size(44, 44).Radius(12).LinearGradient(accentGradient(t)).Border(1, ui.RGBA(255, 255, 255, 0.28)).
						Shadow(0, 6, 16, -6, t.Accent.Alpha(0.55)).Center().Shrink(0).Children(func() {
						ui.Icon(c, pithMark).FontSize(26).TextColor(ui.Hex("#ffffff"))
					})
					ui.Column(c).Grow(1).Gap(3).MinWidth(0).Children(func() {
						ui.Row(c).Gap(8).AlignItems(ui.Baseline).Children(func() {
							ui.Text(c, locales.AppName()).FontSize(15).FontWeight(600).SingleLine()
							ui.Text(c, a.Version).FontSize(12).TextColor(t.TextMuted).FontFeatures("tnum").SingleLine()
						})
						ui.Text(c, locales.MitLicense()).FontSize(12).TextColor(t.TextMuted).SingleLine()
					})
					if glassCapsuleButton(c, "github", icLink, "GitHub").Focusable().Clicked() {
						a.Desktop.OpenURL(repoURL)
					}
				})
			})
		})
	})
}

// group is a group of settings: a small title, and its rows in a pane,
// divided by hairlines.
func group(c *ui.Context, title string, rows func()) {
	ui.Column(c).Gap(8).Children(func() {
		sectionTitle(c, title).Padding(0, settingsInset)
		paneRows(c).Children(rows)
	})
}

// settingsRow is a row of a group: its label on the left, its control on
// the right. help, when given, explains the setting in a tooltip on the
// label, and on an info icon beside it when info, for settings whose label
// alone does not tell what they do.
func settingsRow(c *ui.Context, label, help string, info bool, control func()) {
	t := c.Theme()
	ui.Row(c).Gap(16).Padding(9, settingsInset).MinHeight(52).Children(func() {
		ui.Row(c).Grow(1).Gap(6).MinWidth(0).Children(func() {
			text := ui.Text(c, label).FontWeight(500).Shrink(1)
			if help == "" {
				return
			}
			text.Tooltip(help)
			if info {
				ui.Icon(c, icInfo).FontSize(13).TextColor(t.TextMuted).Tooltip(help).Shrink(0)
			}
		})
		ui.Row(c).Shrink(0).Children(control)
	})
}

// settingsSelect is a drop-down of a row, shaped as a capsule of glass as
// the page's buttons are.
func settingsSelect(c *ui.Context, chosen *string, options []string) ui.Element {
	return ui.Select(c, chosen, options).Width(150).Padding(8, 14, 8, 16).Radius(999).
		Material(glass.Glass{}).Border(1, rim(c.Theme()))
}

// uninstallButton asks before removing the helper.
func (a *App) uninstallButton(c *ui.Context) {
	if glassCapsuleButton(c, "uninstall", icTrash, locales.Uninstall()).Focusable().Clicked() {
		a.confirm = &confirmation{
			title:  locales.UninstallTheHelper(),
			action: locales.Uninstall2(),
			danger: true,
			do:     a.uninstallHelper,
		}
	}
}

// helperState says in a word or two whether the helper is there and
// answers, with its version, and in full in a tooltip.
func (a *App) helperState(c *ui.Context, h api.HelperStatus) {
	t := c.Theme()
	text, col, hollow := locales.HelperInstalled(), running(t), false
	switch {
	case !h.Installed:
		text, col, hollow = locales.HelperNotInstalled(), t.TextMuted, true
	case helperUntrusted(a.status):
		text, col = locales.CantVerifyHelper(), t.Danger
	case h.Outdated:
		text, col = locales.HelperOutdated(), t.Warning
	case !h.Running:
		text, col = locales.HelperNotAnswering(), t.Warning
	}
	if h.Installed && h.Version != "" {
		text += " · " + h.Version
	}
	ui.Row(c).Gap(6).Children(func() {
		dot(c, col, 7, hollow)
		ui.Text(c, text).TextColor(t.TextMuted).SingleLine().Tooltip(a.helperText(h))
	})
}

func (a *App) helperText(h api.HelperStatus) string {
	switch {
	case !h.Installed:
		return locales.NotInstalledRoutingNeedsItMacos()
	case helperUntrusted(a.status):
		return locales.PithCanTVerifyItsHelper()
	case h.Outdated:
		return locales.ItDoesnTMatchThisVersion()
	case !h.Running:
		return locales.InstalledNotAnswering()
	}
	return locales.HelperInstalledVersion(h.Version)
}

func (a *App) exportBackup() {
	cfg := cloneConfig(a.cfg)
	f := api.File{Kind: api.FileBackup, Version: api.ConfigVersion, App: a.Version, Exported: a.Now(), Config: &cfg}
	data, _ := json.MarshalIndent(f, "", "  ")
	name := "Pith " + a.Now().Format("2006-01-02") + ".pith.json"
	a.async(func(context.Context) error { return a.Desktop.Save(name, data) }, nil, nil)
}

func (a *App) importBackup() {
	var cfg api.Config
	a.async(func(context.Context) error {
		data, err := a.Desktop.Open()
		if err != nil || data == nil {
			return err
		}
		f, err := api.ParseFile(data)
		if err != nil {
			return err
		}
		if f.Kind != api.FileBackup {
			return &api.Error{Code: api.ErrInvalidConfig, Detail: "this file holds rules: import it on the Rules page"}
		}
		f.Config.Version = api.ConfigVersion
		if err := f.Config.Validate(); err != nil {
			return err
		}
		cfg = *f.Config
		return nil
	}, func() {
		if len(cfg.Scenes) == 0 {
			return
		}
		a.confirm = &confirmation{
			title:  locales.ReplaceYourSettings(),
			body:   locales.ReplaceBackupBody(len(cfg.Scenes)),
			action: locales.Replace(),
			danger: true,
			do: func() {
				if a.isRunning() {
					a.stop()
				}
				// setConfig first: it also ends a configuration that could
				// not be read, which change would refuse to save over.
				a.setConfig(cfg)
				a.sceneID = cfg.Selected
				a.change(func(*api.Config) {})
			},
		}
	}, nil)
}
