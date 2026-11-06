package ui

import (
	"fmt"
	"strconv"

	"github.com/egoist/mygo/ui"

	"github.com/Kel077/pith/internal/api"
	"github.com/Kel077/pith/locales"
)

// proxiesGroup is the settings' group of local SOCKS5 proxies: a row a port,
// each with the one interface it goes out through, and a last row that
// adds one.
func (a *App) proxiesGroup(c *ui.Context) {
	group(c, locales.LocalProxies(), func() {
		for k := range a.cfg.Proxies {
			a.proxyRow(c, k)
		}
		exits, _ := a.proxyExits(api.InterfaceRef{})
		ui.Row(c).Gap(16).Padding(9, settingsInset).MinHeight(52).Children(func() {
			ui.Spacer(c)
			full := len(a.cfg.Proxies) >= api.MaxProxies
			add := glassCapsuleButton(c, "add-proxy", icPlus, locales.AddProxy()).Focusable().Shrink(0).Disabled(len(exits) == 0 || full)
			if add.Clicked() && len(exits) > 0 && !full {
				a.addProxy(exits)
			}
		})
	})
}

// proxyRow is a proxy's row: its address, which a double-click changes the
// port of, and how it is doing; on the right, its interface, its switch,
// and a button that removes it.
func (a *App) proxyRow(c *ui.Context, k int) {
	p := a.cfg.Proxies[k]
	s := &a.settings
	ui.Row(c.Key(fmt.Sprint("proxy-", k))).Gap(16).Padding(9, settingsInset).MinHeight(52).Children(func() {
		ui.Column(c).Grow(1).MinWidth(0).Gap(2).Children(func() {
			if s.editingPort == p.Port {
				in := networkField(c, ui.TextInput(c.Key("proxy-port"), &s.port)).Padding(1, 10).FontWeight(500).Width(120).AutoFocus().Placeholder(strconv.Itoa(p.Port))
				if in.Submitted() {
					a.setProxyPort(k, s.port)
				}
			} else {
				addr := ui.Text(c, "127.0.0.1:"+strconv.Itoa(p.Port)).FontWeight(500).FontFeatures("tnum").SingleLine().Cursor(ui.CursorText).Tooltip(locales.DoubleClickToChangeThePort())
				if addr.DoubleClicked() {
					s.editingPort, s.port = p.Port, strconv.Itoa(p.Port)
				}
			}
			a.proxyState(c, p)
		})
		ui.Row(c).Shrink(0).Gap(12).AlignItems(ui.Center).Children(func() {
			refs, labels := a.proxyExits(p.Via)
			chosen := a.ifaceLabel(p.Via)
			for j, r := range refs {
				if r.Key() == p.Via.Key() {
					chosen = labels[j]
				}
			}
			if settingsSelect(c, &chosen, labels).Label(locales.Interfaces()).Changed() {
				for j, l := range labels {
					if l == chosen {
						via := refs[j]
						a.change(func(cfg *api.Config) { cfg.Proxies[k].Via = via })
					}
				}
			}
			on := p.Enabled
			if ui.Switch(c, &on).Label("127.0.0.1:" + strconv.Itoa(p.Port)).Changed() {
				a.change(func(cfg *api.Config) { cfg.Proxies[k].Enabled = on })
			}
			if quietButton(c, "remove-proxy", icX, locales.Remove(), false) {
				a.change(func(cfg *api.Config) { cfg.Proxies = append(cfg.Proxies[:k:k], cfg.Proxies[k+1:]...) })
			}
		})
	})
}

// proxyState says how a proxy is doing, under its address: off, listening
// with what goes through it, or what keeps it from working.
func (a *App) proxyState(c *ui.Context, p api.Proxy) {
	t := c.Theme()
	var st api.ProxyStatus
	for _, s := range a.status.Proxies {
		if s.Port == p.Port {
			st = s
		}
	}
	var code api.ErrorCode
	if st.Error != nil {
		code = st.Error.Code
	}
	ui.Row(c).Gap(6).AlignItems(ui.Center).Children(func() {
		line := func(col ui.Color, hollow bool, s string, text ui.Color) {
			dot(c, col, 6, hollow)
			ui.Text(c, s).FontSize(12).TextColor(text).SingleLine().Shrink(1)
		}
		switch {
		case !p.Enabled:
			line(t.TextMuted, true, locales.ProxyOff(), t.TextMuted)
		case code == api.ErrPortInUse:
			line(t.Danger, false, locales.PortInUse(), t.Danger)
		case code == api.ErrInterfaceUnavailable:
			line(t.Warning, true, locales.NotConnected(), t.Warning)
		case code == api.ErrDNSUnavailable:
			line(t.Warning, true, locales.NoDns(), t.Warning)
		case st.Error != nil:
			line(t.Danger, false, errorText(st.Error), t.Danger)
		case st.Listening:
			line(running(t), false, locales.ProxyListening()+" · "+locales.PanelTotals(st.Open, byteSize(st.Received), byteSize(st.Sent)), t.TextMuted)
		default:
			line(t.TextMuted, true, locales.ProxyStarting(), t.TextMuted)
		}
	})
}

// proxyExits is the interfaces a proxy can go out through, and their names
// as the menu shows them, with current, if it is gone, at the end.
func (a *App) proxyExits(current api.InterfaceRef) ([]api.InterfaceRef, []string) {
	var refs []api.InterfaceRef
	for i := range a.ifaces {
		if a.ifaces[i].CanProxy() {
			refs = append(refs, refOf(&a.ifaces[i]))
		}
	}
	if current.Name != "" && current.Resolve(a.ifaces) == nil {
		refs = append(refs, current)
	}
	labels := make([]string, len(refs))
	seen := map[string]int{}
	for j, r := range refs {
		labels[j] = a.ifaceLabel(r)
		seen[labels[j]]++
	}
	for j, r := range refs {
		if seen[labels[j]] > 1 {
			labels[j] += " (" + r.Name + ")"
		}
	}
	return refs, labels
}

// addProxy adds a proxy on the first free port from 1080, through the first
// connected interface no proxy goes out through yet.
func (a *App) addProxy(exits []api.InterfaceRef) {
	used, taken := map[int]bool{}, map[string]bool{}
	for _, p := range a.cfg.Proxies {
		used[p.Port] = true
		taken[p.Via.Key()] = true
	}
	port := 1080
	for used[port] {
		port++
	}
	via := exits[0]
	for _, r := range exits {
		if i := r.Resolve(a.ifaces); i != nil && i.Up && !taken[r.Key()] {
			via = r
			break
		}
	}
	a.change(func(cfg *api.Config) {
		cfg.Proxies = append(cfg.Proxies, api.Proxy{Port: port, Via: via, Enabled: true})
	})
}

// setProxyPort gives a proxy the port typed for it, if it is one no other
// proxy uses, from 1024 on; or says why it isn't, and keeps it.
func (a *App) setProxyPort(k int, text string) {
	s := &a.settings
	port, err := strconv.Atoi(text)
	if err == nil && port == a.cfg.Proxies[k].Port {
		s.editingPort = 0
		return
	}
	ok := err == nil && port >= 1024 && port <= 65535
	for j, p := range a.cfg.Proxies {
		if j != k && p.Port == port {
			ok = false
		}
	}
	if !ok {
		a.toast(locales.UseAFreePort())
		return
	}
	s.editingPort = 0
	a.change(func(cfg *api.Config) { cfg.Proxies[k].Port = port })
}
