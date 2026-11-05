package ui

import (
	"context"
	"encoding/json"

	"github.com/egoist/mygo/ui"
	"github.com/Kel077/pith/internal/api"
)

// settingsPage is the state of the settings page.
type settingsPage struct{}

const repoURL = "https://github.com/Kel077/pith"

func (a *App) settingsView(c *ui.Context) {
	t := c.Theme()
	pageHeader(c, tr("Settings", "设置"), nil)
	s := a.cfg.Settings
	set := func(fn func(s *api.Settings)) {
		a.change(func(cfg *api.Config) { fn(&cfg.Settings) })
	}
	ui.Scroll(c).Grow(1).Padding(0, 24, 28).Gap(18).Children(func() {
		group(c, tr("General", "通用"), func() {
			field(c, tr("Open at login", "登录时打开"), tr("Pith waits in the menu bar.", "橘核在菜单栏待命。"), func() {
				on := s.OpenAtLogin
				if ui.Switch(c, &on).Label(tr("Open at login", "登录时打开")).Changed() {
					set(func(s *api.Settings) { s.OpenAtLogin = on })
					a.async(func(context.Context) error { return a.Desktop.SetOpenAtLogin(on) }, nil, nil)
				}
			})
			ui.Divider(c)
			field(c, tr("Keep routing after quitting", "退出后继续分流"), tr("Off: quitting Pith stops routing.", "关闭时，退出橘核会停止分流。"), func() {
				on := s.KeepRouting
				if ui.Switch(c, &on).Label(tr("Keep routing after quitting", "退出后继续分流")).Changed() {
					set(func(s *api.Settings) { s.KeepRouting = on })
				}
			})
			ui.Divider(c)
			field(c, tr("Resume routing on launch", "启动时恢复分流"), tr("After a restart, route again if it was on.", "重启电脑后，如果之前在分流就自动恢复。"), func() {
				on := s.ResumeOnLaunch
				if ui.Switch(c, &on).Label(tr("Resume routing on launch", "启动时恢复分流")).Changed() {
					set(func(s *api.Settings) { s.ResumeOnLaunch = on })
				}
			})
			ui.Divider(c)
			field(c, tr("Speed in the menu bar", "在菜单栏显示网速"), "", func() {
				on := s.MenuBarSpeed
				if ui.Switch(c, &on).Label(tr("Speed in the menu bar", "在菜单栏显示网速")).Changed() {
					set(func(s *api.Settings) { s.MenuBarSpeed = on })
					a.trayState = ""
				}
			})
			ui.Divider(c)
			field(c, tr("Language", "语言"), "", func() {
				options := []string{tr("System", "跟随系统"), "English", "简体中文"}
				values := []string{"", "en", "zh-Hans"}
				chosen := options[0]
				for k, v := range values {
					if v == s.Language {
						chosen = options[k]
					}
				}
				if ui.Select(c, &chosen, options).Label(tr("Language", "语言")).Width(150).Changed() {
					for k, o := range options {
						if o == chosen {
							v := values[k]
							set(func(s *api.Settings) { s.Language = v })
							lang = resolveLanguage(v, a.Desktop.PreferredLanguages())
						}
					}
				}
			})
		})

		group(c, tr("Routing", "分流"), func() {
			field(c, tr("Recognize domains", "识别域名"), tr("Read the host name apps connect to (TLS SNI, HTTP Host), so domain rules match. Contents stay unread.", "读取应用连接的主机名（TLS SNI、HTTP Host）来匹配域名规则，不读取传输内容。"), func() {
				on := s.SniffDomains
				if ui.Switch(c, &on).Label(tr("Recognize domains", "识别域名")).Changed() {
					set(func(s *api.Settings) { s.SniffDomains = on })
				}
			})
			ui.Divider(c)
			field(c, "IPv6", tr("Route IPv6 connections too.", "同时对 IPv6 连接分流。"), func() {
				on := s.IPv6
				if ui.Switch(c, &on).Label("IPv6").Changed() {
					set(func(s *api.Settings) { s.IPv6 = on })
				}
			})
			ui.Divider(c)
			field(c, tr("Safe start", "安全启动"), tr("If the Mac loses the internet right after routing starts, undo it.", "开始分流后如果电脑断网，自动撤销。"), func() {
				on := s.SafeStart
				if ui.Switch(c, &on).Label(tr("Safe start", "安全启动")).Changed() {
					set(func(s *api.Settings) { s.SafeStart = on })
				}
			})
			ui.Divider(c)
			field(c, tr("Keep the connection log", "连接日志保留"), "", func() {
				options := []string{tr("Until quitting", "仅本次运行"), tr("1 day", "1 天"), tr("7 days", "7 天"), tr("30 days", "30 天")}
				values := []int{0, 1, 7, 30}
				chosen := options[2]
				for k, v := range values {
					if v == s.LogDays {
						chosen = options[k]
					}
				}
				if ui.Select(c, &chosen, options).Label(tr("Keep the connection log", "连接日志保留")).Width(150).Changed() {
					for k, o := range options {
						if o == chosen {
							v := values[k]
							set(func(s *api.Settings) { s.LogDays = v })
						}
					}
				}
			})
		})

		group(c, tr("Helper", "后台助手"), func() {
			h := a.status.Helper
			field(c, tr("Routing helper", "分流助手"), a.helperText(h), func() {
				switch {
				case a.busy == "install" || a.busy == "uninstall":
					ui.Spinner(c)
				case !h.Installed:
					if ui.PrimaryButton(c, tr("Install…", "安装…")).Clicked() {
						a.installHelper("")
					}
				case h.Outdated:
					if ui.PrimaryButton(c, tr("Update…", "更新…")).Clicked() {
						a.installHelper("")
					}
				default:
					if ui.Button(c, tr("Uninstall…", "卸载…")).Clicked() {
						a.confirm = &confirmation{
							title:  tr("Uninstall the helper?", "卸载后台助手？"),
							body:   tr("Routing stops. Your rules stay; installing again needs your password.", "分流会停止，规则会保留；重新安装需要再次输入密码。"),
							action: tr("Uninstall", "卸载"),
							danger: true,
							do:     a.uninstallHelper,
						}
					}
				}
			})
		})

		group(c, tr("Data", "数据"), func() {
			field(c, tr("Back up everything", "备份全部配置"), tr("Scenes, rules, interface names and settings, in one file.", "场景、规则、网卡名称和设置，保存为一个文件。"), func() {
				ui.Row(c).Gap(8).Children(func() {
					if textIconButton(c, icUpload, tr("Export…", "导出…")).Clicked() {
						a.exportBackup()
					}
					if textIconButton(c, icDownload, tr("Import…", "导入…")).Clicked() {
						a.importBackup()
					}
				})
			})
		})

		group(c, tr("About", "关于"), func() {
			ui.Row(c).Gap(12).PaddingY(8).Children(func() {
				ui.Box(c).Size(40, 40).Radius(11).Background(t.Accent).Center().Children(func() {
					ui.Icon(c, pithMark).FontSize(24).TextColor(t.AccentText)
				})
				ui.Column(c).Grow(1).Gap(2).Children(func() {
					ui.Text(c, tr("Pith", "橘核")+"  "+a.Version).FontWeight(600)
					muted(c, tr("Open source under the MIT license.", "开源软件，MIT 协议。")).FontSize(12)
				})
				if textIconButton(c, icLink, "GitHub").Clicked() {
					a.Desktop.OpenURL(repoURL)
				}
			})
		})
	})
}

// group is a titled box of settings.
func group(c *ui.Context, title string, body func()) {
	t := c.Theme()
	ui.Column(c).Gap(8).MaxWidth(720).Children(func() {
		sectionTitle(c, title).Padding(0, 4)
		ui.Column(c).Padding(4, 16).Radius(12).Border(1, t.Border).Children(body)
	})
}

func (a *App) helperText(h api.HelperStatus) string {
	switch {
	case !h.Installed:
		return tr("Not installed. Routing needs it; macOS asks for your password once.", "未安装。分流需要它，安装时 macOS 会要求输入一次密码。")
	case h.Outdated:
		return trf("Version %s is outdated.", "版本 %s 需要更新。", h.Version)
	case !h.Running:
		return tr("Installed, not answering.", "已安装，但没有响应。")
	}
	return trf("Installed, version %s.", "已安装，版本 %s。", h.Version)
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
			title:  tr("Replace your settings?", "替换当前配置？"),
			body:   trf("The backup has %d scenes. Your scenes, rules and settings are replaced.", "备份里有 %d 个场景。当前的场景、规则和设置都会被替换。", len(cfg.Scenes)),
			action: tr("Replace", "替换"),
			danger: true,
			do: func() {
				if a.isRunning() {
					a.stop()
				}
				a.change(func(c *api.Config) { *c = cfg })
				a.setConfig(cfg)
				a.sceneID = cfg.Selected
			},
		}
	}, nil)
}
