package ui

import (
	"strings"

	"github.com/egoist/mygo/ui"
	"github.com/Kel077/pith/internal/api"
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

// dialogs shows the dialogs that are open.
func (a *App) dialogs(c *ui.Context) {
	if a.undo != nil {
		u := a.undo
		a.undo = nil
		c.ToastAction(u.msg, tr("Undo", "撤销"), u.do)
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
	if a.confirm != nil {
		a.confirmView(c)
	}
}

func (a *App) installView(c *ui.Context) {
	t := c.Theme()
	p := &a.install
	ui.Modal(c, &p.open, func() {
		ui.Column(c).Width(440).Gap(12).Children(func() {
			ui.Box(c).Size(44, 44).Radius(12).Background(t.Accent.Alpha(0.14)).Center().Children(func() {
				ui.Icon(c, icShield).FontSize(24).TextColor(t.Accent)
			})
			title := tr("Install the helper", "安装后台助手")
			if p.update {
				title = tr("Update the helper", "更新后台助手")
			}
			ui.Text(c, title).FontSize(17).Bold()
			ui.Text(c, tr("Pith routes connections with a small helper that runs with administrator rights. macOS asks for your password once to install it.",
				"橘核需要一个以管理员权限运行的小助手来接管路由。安装时 macOS 会要求输入一次管理员密码。"))
			ui.Column(c).Gap(6).Children(func() {
				for _, s := range []string{
					tr("It only routes while you turn routing on.", "它只在你打开分流时工作。"),
					tr("It never reads what apps send: no decryption, nothing uploaded.", "不读取传输内容，不解密，也不上传任何数据。"),
					tr("You can remove it in Settings at any time.", "随时可以在设置里卸载。"),
				} {
					ui.Row(c).Gap(8).AlignItems(ui.Start).Children(func() {
						ui.Icon(c, icCheck).TextColor(running(t)).Margin(2, 0, 0)
						ui.Text(c, s).TextColor(t.TextMuted)
					})
				}
			})
			ui.Row(c).Gap(8).Margin(8, 0, 0).Children(func() {
				ui.Spacer(c)
				if ui.Button(c, tr("Not Now", "以后再说")).Clicked() {
					p.open = false
				}
				label := tr("Install…", "安装…")
				if p.update {
					label = tr("Update…", "更新…")
				}
				if ui.PrimaryButton(c, label).Disabled(a.busy != "").Clicked() {
					p.open = false
					a.installHelper(p.then)
				}
			})
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
	ui.Modal(c, &d.open, func() {
		ui.Column(c).Width(360).Gap(12).Children(func() {
			title := tr("New Scene", "新建场景")
			if d.id != "" {
				title = tr("Rename Scene", "重命名场景")
			}
			ui.Text(c, title).FontSize(17).Bold()
			submit := ui.TextInput(c.Key("scene-name"), &d.name).Placeholder(tr("e.g. Office, Home", "例如：公司、家里")).AutoFocus().Submitted()
			ui.Row(c).Gap(8).Children(func() {
				ui.Spacer(c)
				if ui.Button(c, tr("Cancel", "取消")).Clicked() {
					d.open = false
				}
				ok := ui.PrimaryButton(c, tr("Done", "完成")).Disabled(strings.TrimSpace(d.name) == "").Clicked()
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
	})
}

func (a *App) confirmView(c *ui.Context) {
	k := a.confirm
	k.open = true
	ui.Modal(c, &k.open, func() {
		ui.Column(c).Width(380).Gap(10).Children(func() {
			ui.Text(c, k.title).FontSize(17).Bold()
			if k.body != "" {
				muted(c, k.body)
			}
			ui.Row(c).Gap(8).Margin(8, 0, 0).Children(func() {
				ui.Spacer(c)
				if ui.Button(c, tr("Cancel", "取消")).Clicked() {
					k.open = false
				}
				b := ui.PrimaryButton(c, k.action)
				if k.danger {
					b.Background(c.Theme().Danger)
				}
				if b.Clicked() {
					k.open = false
					k.do()
				}
			})
		})
	})
	if !k.open {
		a.confirm = nil
	}
}
