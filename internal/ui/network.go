package ui

import (
	"context"
	"strings"

	"github.com/egoist/mygo/plugins/glass"
	"github.com/egoist/mygo/ui"

	"github.com/Kel077/pith/internal/api"
	"github.com/Kel077/pith/locales"
)

// networkPage is the state of the network page.
type networkPage struct {
	renaming string // the key of the interface being renamed
	alias    string
	target   string // "host:port" to test through every interface
	results  map[string]api.Probe
	tested   []string // the interfaces the test runs through
	testing  int      // probes of the test still running
	testGen  int      // the test results belong to
}

// networkInset is how far the rows of the page's groups, and the titles
// above them, keep from the groups' edges.
const networkInset = 16

// networkView shows the interfaces, what each is connected to, and tests
// of the internet through each: a pane of glass for each connected
// interface, two by two; the others as rows of one group; and a server
// reached through all of them.
func (a *App) networkView(c *ui.Context) {
	t := c.Theme()
	a.watchHotspots()
	pageHeader(c, locales.Network(), "", func() {
		all := glassCapsuleButton(c, "test-all", icRefresh, locales.TestAll()).Focusable().Shrink(0).
			Tooltip(locales.TestsConnectThroughOneInterfaceAt()).Disabled(len(a.probing) > 0)
		if all.Clicked() {
			for _, i := range a.ifaces {
				if i.Up {
					a.probe(i.Name)
				}
			}
		}
	})
	ui.Scroll(c).Grow(1).Padding(0, 32, 28).Gap(24).Children(func() {
		var up, down []*api.Interface
		for _, i := range a.visibleIfaces() {
			if i.Up {
				up = append(up, i)
			} else {
				down = append(down, i)
			}
		}
		if len(up) > 0 {
			ui.Grid(c).Columns(2).Gap(16).Children(func() {
				for _, i := range up {
					a.ifaceCard(c, i)
				}
			})
		} else {
			pane(c).Padding(28).Gap(10).AlignItems(ui.Center).Children(func() {
				ui.Icon(c, icNetwork).FontSize(22).TextColor(t.TextMuted)
				muted(c, locales.NoNetworkIsConnected())
			})
		}
		if len(down) > 0 {
			networkGroup(c, locales.NotConnected(), func() {
				for _, i := range down {
					a.downRow(c, i)
				}
			})
		}
		a.reachability(c)
	})
}

// networkGroup is a group of rows under a small title.
func networkGroup(c *ui.Context, title string, rows func()) {
	ui.Column(c).Gap(8).Children(func() {
		sectionTitle(c, title).Padding(0, networkInset)
		paneRows(c).Children(rows)
	})
}

// networkKindIcon shows the icon of an interface's kind in a circle of
// glass tinted with the interface's color, or gray for one that is not
// connected.
func networkKindIcon(c *ui.Context, kind api.InterfaceKind, col ui.Color, up bool, size float32) {
	t := c.Theme()
	tint, fg := col.Alpha(0.2), col
	if t.Dark {
		tint, fg = col.Alpha(0.28), col.Mix(ui.Hex("#ffffff"), 0.3)
	}
	if !up {
		tint, fg = subtle(t, 0.05), t.TextMuted
	}
	ui.Box(c).Size(size, size).Radius(size/2).Material(glass.Glass{Tint: tint}).Border(1, rim(t)).Center().Shrink(0).Children(func() {
		ui.Icon(c, kindIcon(kind)).FontSize(size * 0.48).TextColor(fg)
	})
}

// networkField gives a text input of the page its look: a capsule, a
// shade deeper than the glass it is on.
func networkField(c *ui.Context, in ui.Element) ui.Element {
	t := c.Theme()
	return in.Radius(999).Padding(8, 16).Background(subtle(t, 0.045)).Border(1, hairline(t))
}

// ifaceCard shows one interface: a pane with its icon, name and state, its
// addresses, and a test of the internet through it.
func (a *App) ifaceCard(c *ui.Context, i *api.Interface) {
	t := c.Theme()
	ref := refOf(i)
	col := a.colorOf(i.Name)
	n := &a.network
	box := pane(c.Key("iface-" + i.Name)).Padding(18).Gap(14)
	box.Children(func() {
		ui.Row(c).Gap(12).Children(func() {
			networkKindIcon(c, i.Kind, col, i.Up, 40)
			ui.Column(c).Grow(1).Gap(3).MinWidth(0).Children(func() {
				if n.renaming == ref.Key()+"|"+i.Name {
					in := networkField(c, ui.TextInput(c.Key("alias"), &n.alias)).Padding(1, 10).FontSize(15).FontWeight(600).AutoFocus().Placeholder(i.Label)
					if in.Submitted() {
						a.setAlias(ref.Key(), n.alias)
						n.renaming = ""
					}
				} else {
					name := ui.Text(c, a.ifaceLabel(ref)).FontSize(15).FontWeight(600).SingleLine().Cursor(ui.CursorText).Tooltip(locales.DoubleClickToRename())
					if name.DoubleClicked() {
						n.renaming, n.alias = ref.Key()+"|"+i.Name, a.cfg.Aliases[ref.Key()]
					}
				}
				status, state := locales.NotConnected(), t.TextMuted
				if i.Up {
					status, state = locales.Connected(), running(t)
				}
				ui.Row(c).Gap(6).Children(func() {
					dot(c, state, 7, !i.Up)
					ui.Text(c, i.Name+" · "+status).FontSize(12).TextColor(t.TextMuted).SingleLine()
				})
			})
			if i.Primary {
				tagChip(c, nil, locales.SystemDefault(), t.Accent).Tooltip(locales.MacosSendsConnectionsNoRuleMatches())
			}
		})
		if !i.Up {
			return
		}
		h, isHotspot := a.hotspot.info[i.Name]
		if isHotspot {
			a.hotspotStrip(c, h, col)
		}
		// The details, as rows of their own inside the pane.
		ui.Column(c).Radius(12).Background(subtle(t, 0.03)).Dividers(1, hairline(t)).Children(func() {
			kv(c, "IPv4", strings.Join(i.IPv4, ", "))
			if len(i.IPv6) > 0 {
				kv(c, "IPv6", i.IPv6[0])
			}
			kv(c, locales.Router(), i.Router)
			kv(c, "DNS", strings.Join(i.DNS, ", "))
			if i.HardwareAddr != "" {
				kv(c, locales.Hardware(), i.HardwareAddr)
			}
		})
		// The test, at the foot of the pane however tall its row makes it.
		ui.Row(c).Gap(10).Margin(ui.Auto, 0, 0, 0).Children(func() {
			p, tested := a.probes[i.Name]
			renewal := ""
			if r, ok := a.hotspot.renewals[i.Name]; ok {
				renewal = renewalText(r)
			}
			switch {
			case a.hotspot.renewing[i.Name]:
				ui.Spinner(c)
				muted(c, locales.ChangingHotspotIp())
			case renewal != "":
				ui.Text(c, renewal).FontSize(13).FontWeight(600).FontFeatures("tnum").SingleLine().Shrink(1).Selectable()
			case a.probing[i.Name]:
				ui.Spinner(c)
				muted(c, locales.Testing())
			case tested && p.OK:
				ui.Text(c, millis(p.Latency)).FontSize(15).FontWeight(600).FontFeatures("tnum").SingleLine().Shrink(0)
				ip := p.PublicIPv4
				if ip == "" {
					ip = p.PublicIPv6
				}
				if ip != "" {
					muted(c, locales.PublicIp(ip)).FontSize(12).SingleLine().Shrink(1)
				}
			case tested:
				ui.Icon(c, icAlert).FontSize(14).TextColor(t.Danger).Shrink(0)
				ui.Text(c, locales.NoInternet()).TextColor(t.Danger).SingleLine().Shrink(0)
				if p.Problem != "" {
					ui.Text(c, p.Problem).FontSize(12).TextColor(t.TextMuted).SingleLine().Shrink(1)
				}
			default:
				muted(c, locales.NotTested())
			}
			ui.Spacer(c)
			if isHotspot {
				busy := a.hotspot.renewing[i.Name] || a.probing[i.Name]
				renew := glassCapsuleButton(c, "renew", icSwap, locales.ChangeHotspotIp()).Shrink(0).Disabled(busy || h.Locked || !h.Connected)
				if h.Locked {
					renew.Opacity(0.5).Tooltip(locales.HotspotPassword())
				}
				if renew.Clicked() && !busy && !h.Locked && h.Connected {
					a.renewHotspot(i.Name)
				}
			}
			if glassCapsuleButton(c, "test", nil, locales.Test()).Focusable().Shrink(0).Disabled(a.probing[i.Name] || a.hotspot.renewing[i.Name]).Clicked() {
				a.probe(i.Name)
			}
		})
	})
}

// downRow is a row for an interface that is not connected.
func (a *App) downRow(c *ui.Context, i *api.Interface) {
	t := c.Theme()
	ref := refOf(i)
	n := &a.network
	ui.Row(c.Key("down-"+i.Name)).Gap(12).Padding(10, networkInset).Children(func() {
		networkKindIcon(c, i.Kind, t.TextMuted, false, 30)
		if n.renaming == ref.Key()+"|"+i.Name {
			if networkField(c, ui.TextInput(c.Key("alias"), &n.alias)).Padding(3, 10).FontWeight(500).AutoFocus().Placeholder(i.Label).Width(220).Submitted() {
				a.setAlias(ref.Key(), n.alias)
				n.renaming = ""
			}
		} else {
			name := ui.Text(c, a.ifaceLabel(ref)).FontWeight(500).SingleLine().Cursor(ui.CursorText).Tooltip(locales.DoubleClickToRename())
			if name.DoubleClicked() {
				n.renaming, n.alias = ref.Key()+"|"+i.Name, a.cfg.Aliases[ref.Key()]
			}
		}
		ui.Text(c, i.Name).FontSize(12).TextColor(t.TextMuted)
		ui.Spacer(c)
		dot(c, t.TextMuted, 7, true)
		ui.Text(c, locales.NotConnected()).FontSize(12).TextColor(t.TextMuted).SingleLine().Shrink(0)
	})
}

// kv is a row of an interface's details: what it is on the left, quietly,
// and its value on the right.
func kv(c *ui.Context, k, v string) {
	if v == "" {
		return
	}
	t := c.Theme()
	ui.Row(c).Gap(16).Padding(7, 12).Children(func() {
		ui.Text(c, k).FontSize(12).TextColor(t.TextMuted).SingleLine().Shrink(0)
		ui.Text(c, v).FontSize(12).FontFeatures("tnum").SingleLine().Grow(1).MinWidth(0).TextAlign(ui.End).Selectable()
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
	// Read the interfaces again, for a backend that shows aliases in them.
	var ifs []api.Interface
	a.async(func(ctx context.Context) (err error) {
		ifs, err = a.Service.Interfaces(ctx)
		return err
	}, func() { a.setIfaces(ifs) }, nil)
}

// probe tests the internet through an interface.
func (a *App) probe(name string) {
	if a.probing[name] {
		return
	}
	delete(a.hotspot.renewals, name)
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

// reachability tests a host and port through every connected interface: a
// group whose first row takes the server, and whose next rows tell how
// each interface reached it.
func (a *App) reachability(c *ui.Context) {
	t := c.Theme()
	n := &a.network
	networkGroup(c, locales.ReachAServer(), func() {
		ui.Row(c).Gap(10).Padding(12).Children(func() {
			submit := networkField(c, ui.TextInput(c.Key("reach"), &n.target)).Placeholder(locales.HostPortEGGithubCom()).Grow(1).Submitted()
			ready := strings.TrimSpace(n.target) != "" && n.testing == 0
			test := primaryCapsule(c, "reach-test", nil, locales.Test()).Focusable().Shrink(0).Tooltip(locales.ConnectToAHostAndPort()).Disabled(!ready)
			if (test.Clicked() || submit) && ready {
				a.reach(strings.TrimSpace(n.target))
			}
		})
		if len(n.results) == 0 && n.testing == 0 {
			return
		}
		for _, name := range n.tested {
			i := a.ifaceByName(name)
			if i == nil {
				continue
			}
			p, ok := n.results[i.Name]
			ui.Row(c.Key("reach-"+i.Name)).Gap(10).Padding(10, networkInset).Children(func() {
				ui.Row(c).Width(200).Shrink(0).Children(func() { a.ifaceChip(c, refOf(i)) })
				switch {
				case !ok:
					ui.Spinner(c)
				case p.OK:
					ui.Icon(c, icCheck).FontSize(14).TextColor(running(t)).Shrink(0)
					ui.Text(c, millis(p.Latency)).FontWeight(600).FontFeatures("tnum").SingleLine().Shrink(0)
					muted(c, p.Address).FontSize(12).SingleLine().Shrink(1)
				default:
					ui.Icon(c, icX).FontSize(14).TextColor(t.Danger).Shrink(0)
					ui.Text(c, p.Problem).TextColor(t.Danger).SingleLine().Shrink(1)
				}
			})
		}
	})
}

func (a *App) reach(target string) {
	n := &a.network
	n.testGen++
	gen := n.testGen
	n.results, n.tested, n.testing = map[string]api.Probe{}, nil, 0
	for _, i := range a.ifaces {
		if !i.Up {
			continue
		}
		name := i.Name
		n.tested = append(n.tested, name)
		n.testing++
		var p api.Probe
		a.async(func(ctx context.Context) (err error) {
			p, err = a.Service.Probe(ctx, name, target)
			if err != nil {
				p = api.Probe{Interface: name, Problem: err.Error()}
			}
			return nil
		}, func() {
			if gen != n.testGen {
				return // an older test's
			}
			n.testing--
			n.results[name] = p
		}, nil)
	}
}
