package ui

import (
	"context"
	"strings"

	"github.com/egoist/mygo/ui"
	"github.com/Kel077/pith/internal/api"
)

// networkPage is the state of the network page.
type networkPage struct {
	renaming string // the key of the interface being renamed
	alias    string
	target   string // "host:port" to test through every interface
	results  map[string]api.Probe
	testing  int // tests running
}

// networkView shows the interfaces, what each is connected to, and tests
// of the internet through each.
func (a *App) networkView(c *ui.Context) {
	t := c.Theme()
	pageHeader(c, tr("Network", "网络"), func() {
		if textIconButton(c, icRefresh, tr("Test All", "全部测试")).Disabled(len(a.probing) > 0).Clicked() {
			for _, i := range a.ifaces {
				if i.Up {
					a.probe(i.Name)
				}
			}
		}
	})
	ui.Scroll(c).Grow(1).Padding(0, 24, 24).Gap(16).Children(func() {
		var up, down []*api.Interface
		for _, i := range a.visibleIfaces() {
			if i.Up {
				up = append(up, i)
			} else {
				down = append(down, i)
			}
		}
		ui.Grid(c).Columns(2).Gap(12).Children(func() {
			for _, i := range up {
				a.ifaceCard(c, i)
			}
		})
		if len(up) == 0 {
			muted(c, tr("No network is connected.", "没有已连接的网络。")).Padding(24)
		}
		if len(down) > 0 {
			ui.Column(c).Gap(8).Children(func() {
				sectionTitle(c, tr("Not connected", "未连接")).Padding(0, 4)
				ui.Column(c).Radius(12).Border(1, t.Border).Dividers(1, t.Border).Children(func() {
					for _, i := range down {
						a.downRow(c, i)
					}
				})
			})
		}
		a.reachability(c)
		ui.Row(c).Gap(6).Children(func() {
			ui.Icon(c, icInfo).FontSize(12).TextColor(t.TextMuted)
			ui.Text(c, tr("Tests connect through one interface at a time, whatever the rules say.", "测试会直接从指定网卡连接，不受规则影响。")).FontSize(12).TextColor(t.TextMuted)
		})
	})
}

// ifaceCard shows one interface.
func (a *App) ifaceCard(c *ui.Context, i *api.Interface) {
	t := c.Theme()
	ref := refOf(i)
	col := a.colorOf(i.Name)
	n := &a.network
	box := ui.Column(c.Key("iface-"+i.Name)).Padding(16).Gap(12).Radius(14).Border(1, t.Border)
	if !i.Up {
		box.Background(subtle(t, 0.025))
	}
	box.Children(func() {
		ui.Row(c).Gap(12).Children(func() {
			badge := ui.Box(c).Size(38, 38).Radius(11).Center().Shrink(0)
			if i.Up {
				badge.Background(col.Alpha(0.14))
			} else {
				badge.Background(subtle(t, 0.06))
			}
			badge.Children(func() {
				ic := ui.Icon(c, kindIcon(i.Kind)).FontSize(20)
				if i.Up {
					ic.TextColor(col)
				} else {
					ic.TextColor(t.TextMuted)
				}
			})
			ui.Column(c).Grow(1).Gap(2).MinWidth(0).Children(func() {
				if n.renaming == ref.Key()+"|"+i.Name {
					in := ui.TextInput(c.Key("alias"), &n.alias).AutoFocus().Placeholder(i.Label)
					if in.Submitted() {
						a.setAlias(ref.Key(), n.alias)
						n.renaming = ""
					}
				} else {
					name := ui.Text(c, a.ifaceLabel(ref)).FontSize(15).FontWeight(600).SingleLine().Cursor(ui.CursorText).Tooltip(tr("Double-click to rename", "双击重命名"))
					if name.DoubleClicked() {
						n.renaming, n.alias = ref.Key()+"|"+i.Name, a.cfg.Aliases[ref.Key()]
					}
				}
				status := tr("Not connected", "未连接")
				if i.Up {
					status = tr("Connected", "已连接")
				}
				ui.Row(c).Gap(6).Children(func() {
					dot(c, map[bool]ui.Color{true: running(t), false: t.TextMuted}[i.Up], 7, !i.Up)
					ui.Text(c, i.Name+" · "+status).FontSize(12).TextColor(t.TextMuted).SingleLine()
				})
			})
			if i.Primary {
				ui.Text(c, tr("Default", "系统默认")).FontSize(11).FontWeight(600).TextColor(t.Accent).Padding(2, 8).Radius(999).Background(t.Accent.Alpha(0.12)).Tooltip(tr("macOS sends connections no rule matches here", "未命中规则的连接默认从这里出去"))
			}
		})
		if !i.Up {
			return
		}
		ui.Column(c).Gap(5).Children(func() {
			kv(c, "IPv4", strings.Join(i.IPv4, ", "))
			if len(i.IPv6) > 0 {
				kv(c, "IPv6", i.IPv6[0])
			}
			kv(c, tr("Router", "路由器"), i.Router)
			kv(c, "DNS", strings.Join(i.DNS, ", "))
			if i.HardwareAddr != "" {
				kv(c, tr("Hardware", "硬件地址"), i.HardwareAddr)
			}
		})
		ui.Divider(c)
		ui.Row(c).Gap(10).Children(func() {
			p, tested := a.probes[i.Name]
			switch {
			case a.probing[i.Name]:
				ui.Spinner(c)
				muted(c, tr("Testing…", "正在测试…"))
			case tested && p.OK:
				ui.Text(c, millis(p.Latency)).FontWeight(600).Font("monospace")
				ip := p.PublicIPv4
				if ip == "" {
					ip = p.PublicIPv6
				}
				if ip != "" {
					muted(c, tr("Public IP ", "公网 IP ")+ip).SingleLine().Shrink(1)
				}
			case tested:
				ui.Icon(c, icAlert).TextColor(t.Danger)
				ui.Text(c, tr("No internet", "无法上网")).TextColor(t.Danger)
				if p.Problem != "" {
					ui.Text(c, p.Problem).FontSize(12).TextColor(t.TextMuted).SingleLine().Shrink(1)
				}
			default:
				muted(c, tr("Not tested", "尚未测试"))
			}
			ui.Spacer(c)
			if ui.Button(c, tr("Test", "测试")).Disabled(a.probing[i.Name]).Clicked() {
				a.probe(i.Name)
			}
		})
	})
}

// downRow is a compact row for an interface that is not connected.
func (a *App) downRow(c *ui.Context, i *api.Interface) {
	t := c.Theme()
	ref := refOf(i)
	n := &a.network
	ui.Row(c.Key("down-"+i.Name)).Gap(12).Padding(9, 14).Children(func() {
		ui.Icon(c, kindIcon(i.Kind)).FontSize(16).TextColor(t.TextMuted)
		if n.renaming == ref.Key()+"|"+i.Name {
			if ui.TextInput(c.Key("alias"), &n.alias).AutoFocus().Placeholder(i.Label).Width(220).Submitted() {
				a.setAlias(ref.Key(), n.alias)
				n.renaming = ""
			}
		} else {
			name := ui.Text(c, a.ifaceLabel(ref)).SingleLine().Cursor(ui.CursorText).Tooltip(tr("Double-click to rename", "双击重命名"))
			if name.DoubleClicked() {
				n.renaming, n.alias = ref.Key()+"|"+i.Name, a.cfg.Aliases[ref.Key()]
			}
		}
		ui.Text(c, i.Name).FontSize(12).Font("monospace").TextColor(t.TextMuted)
		ui.Spacer(c)
		ui.Text(c, tr("Not connected", "未连接")).FontSize(12).TextColor(t.TextMuted)
	})
}

func kv(c *ui.Context, k, v string) {
	if v == "" {
		return
	}
	t := c.Theme()
	ui.Row(c).Gap(10).Children(func() {
		ui.Text(c, k).FontSize(12).TextColor(t.TextMuted).Width(64).Shrink(0)
		ui.Text(c, v).FontSize(12).Font("monospace").SingleLine().Shrink(1).Selectable()
	})
}

func (a *App) setAlias(key, alias string) {
	alias = strings.TrimSpace(alias)
	a.change(func(cfg *api.Config) {
		if cfg.Aliases == nil {
			cfg.Aliases = map[string]string{}
		}
		if alias == "" {
			delete(cfg.Aliases, key)
		} else {
			cfg.Aliases[key] = alias
		}
	})
}

// probe tests the internet through an interface.
func (a *App) probe(name string) {
	if a.probing[name] {
		return
	}
	a.probing[name] = true
	var p api.Probe
	a.async(func(ctx context.Context) (err error) {
		p, err = a.Service.Probe(ctx, name, "")
		return err
	}, func() {
		delete(a.probing, name)
		a.probes[name] = p
	}, func(error) { delete(a.probing, name) })
}

// reachability tests a host and port through every connected interface.
func (a *App) reachability(c *ui.Context) {
	t := c.Theme()
	n := &a.network
	box := card(c)
	box.Children(func() {
		ui.Text(c, tr("Reach a server", "连通性测试")).FontSize(15).FontWeight(600)
		muted(c, tr("Connect to a host and port through each interface, to compare them.", "分别从每张网卡连接同一个服务器，比较哪张能通、快多少。"))
		ui.Row(c).Gap(8).Children(func() {
			submit := ui.TextInput(c.Key("reach"), &n.target).Placeholder(tr("host:port, e.g. github.com:443", "主机:端口，例如 github.com:443")).Grow(1).Submitted()
			if (ui.PrimaryButton(c, tr("Test", "测试")).Disabled(strings.TrimSpace(n.target) == "" || n.testing > 0).Clicked() || submit) && strings.TrimSpace(n.target) != "" {
				a.reach(strings.TrimSpace(n.target))
			}
		})
		if len(n.results) == 0 && n.testing == 0 {
			return
		}
		for k := range a.ifaces {
			i := &a.ifaces[k]
			if !i.Up {
				continue
			}
			p, ok := n.results[i.Name]
			ui.Row(c.Key("reach-" + i.Name)).Gap(10).PaddingY(4).Children(func() {
				ui.Row(c).Width(200).Shrink(0).Children(func() { a.ifaceChip(c, refOf(i)) })
				switch {
				case !ok:
					ui.Spinner(c)
				case p.OK:
					ui.Icon(c, icCheck).TextColor(running(t))
					ui.Text(c, millis(p.Latency)).Font("monospace").FontWeight(600)
					muted(c, p.Address).SingleLine()
				default:
					ui.Icon(c, icX).TextColor(t.Danger)
					ui.Text(c, p.Problem).TextColor(t.Danger).SingleLine().Shrink(1)
				}
			})
		}
	})
}

func (a *App) reach(target string) {
	n := &a.network
	n.results = map[string]api.Probe{}
	for _, i := range a.ifaces {
		if !i.Up {
			continue
		}
		name := i.Name
		n.testing++
		var p api.Probe
		a.async(func(ctx context.Context) (err error) {
			p, err = a.Service.Probe(ctx, name, target)
			if err != nil {
				p = api.Probe{Interface: name, Problem: err.Error()}
			}
			return nil
		}, func() {
			n.testing--
			n.results[name] = p
		}, nil)
	}
}
