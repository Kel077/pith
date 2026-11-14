package ui

import (
	"cmp"
	"context"
	"fmt"
	"net/netip"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/egoist/mygo/ui"

	"github.com/Kel077/pith/internal/api"
	"github.com/Kel077/pith/locales"
)

// upstreamDialog asks for the proxy a local proxy goes out through.
type upstreamDialog struct {
	open    bool
	port    int // of the local proxy
	address string
}

// deviceDialog asks for a device that may use the shared proxies.
type deviceDialog struct {
	open          bool
	address, name string
}

// proxiesGroup is the settings' group of local proxies: a row a port, each
// with what it goes out through, one interface, the rules of a scene or
// another proxy, and a last row that adds one.
func (a *App) proxiesGroup(c *ui.Context) {
	group(c, locales.LocalProxies(), func() {
		// The rows are drawn from the list as it is now: a click in one may
		// change the list, or have a save that fails put another in its
		// place, while the rows after it are still to draw. A row is removed
		// once all are drawn.
		proxies := slices.Clone(a.cfg.Proxies)
		remove := -1
		for k, p := range proxies {
			if a.proxyRow(c, k, p) {
				remove = k
			}
		}
		if remove >= 0 {
			port := proxies[remove].Port
			a.change(func(cfg *api.Config) {
				cfg.Proxies = slices.DeleteFunc(slices.Clone(cfg.Proxies), func(p api.Proxy) bool { return p.Port == port })
			})
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
// port of, and how it is doing; on the right, a button that changes its
// public IP when it goes out through another Pith that lets this Mac, what
// it goes out through, a button that shares it with the devices, one that
// lets what uses it change its IP, its switch, and a button that removes
// it: proxyRow tells when that one is clicked.
func (a *App) proxyRow(c *ui.Context, k int, p api.Proxy) (remove bool) {
	s := &a.settings
	st := a.proxyStatus(p.Port)
	ui.Row(c.Key(fmt.Sprint("proxy-", k))).Gap(16).Padding(9, settingsInset).MinHeight(52).Children(func() {
		ui.Column(c).Grow(1).MinWidth(0).Gap(2).Children(func() {
			if s.editingPort == p.Port {
				in := networkField(c, ui.TextInput(c.Key("proxy-port"), &s.port)).Padding(1, 10).FontWeight(500).Width(120).AutoFocus().Placeholder(strconv.Itoa(p.Port))
				if at := a.proxyAt(p.Port); in.Submitted() && at >= 0 {
					a.setProxyPort(at, s.port)
				}
			} else {
				addr := ui.Text(c, a.proxyAddress(p)).FontWeight(500).FontFeatures("tnum").SingleLine().Cursor(ui.CursorText).Tooltip(locales.DoubleClickToChangeThePort())
				if addr.DoubleClicked() {
					s.editingPort, s.port = p.Port, strconv.Itoa(p.Port)
				}
			}
			a.proxyState(c, p, st)
		})
		ui.Row(c).Shrink(0).Gap(12).AlignItems(ui.Center).Children(func() {
			if up := st.Upstream; p.Enabled && up != nil && up.CanChangeIP {
				busy := s.changing[p.Port] || st.Changing
				change := glassCapsuleButton(c, "change-proxy-ip", nil, locales.ChangeIp()).Shrink(0).Disabled(busy)
				if change.Clicked() && !busy {
					a.changeProxyIP(p.Port)
				}
			}
			choices, at := a.exitChoices(p)
			options := make([]string, len(choices))
			for j := range choices {
				options[j] = choices[j].label
			}
			chosen := options[at]
			if settingsSelect(c, &chosen, options).Label(locales.Interfaces()).Changed() {
				if j, at := slices.Index(options, chosen), a.proxyAt(p.Port); j >= 0 && at >= 0 {
					a.chooseExit(at, choices[j])
				}
			}
			if rowToggle(c, "share-proxy", icShare, locales.ShareWithDevices(), p.Shared) {
				a.changeProxy(p.Port, func(q *api.Proxy) { q.Shared = !p.Shared })
			}
			if p.Scene != "" {
				// By a scene's rules it has no one IP to change: the space of
				// the button keeps the rows in line.
				ui.Box(c).Size(26, 26).Shrink(0)
			} else if rowToggle(c, "let-change-ip", icSwap, locales.LetAppsAndDevicesChangeItsIp(), p.ChangeIP) {
				a.changeProxy(p.Port, func(q *api.Proxy) { q.ChangeIP = !p.ChangeIP })
			}
			on := p.Enabled
			if ui.Switch(c, &on).Label("127.0.0.1:" + strconv.Itoa(p.Port)).Changed() {
				a.changeProxy(p.Port, func(q *api.Proxy) { q.Enabled = on })
			}
			remove = quietButton(c, "remove-proxy", icX, locales.Remove(), false)
		})
	})
	return remove
}

// proxyAt is where the proxy on port is in the configuration as it is now:
// -1 when it is gone.
func (a *App) proxyAt(port int) int {
	return slices.IndexFunc(a.cfg.Proxies, func(p api.Proxy) bool { return p.Port == port })
}

// changeProxy changes the proxy on port, if it is still there.
func (a *App) changeProxy(port int, fn func(p *api.Proxy)) {
	a.change(func(cfg *api.Config) {
		for k := range cfg.Proxies {
			if cfg.Proxies[k].Port == port {
				fn(&cfg.Proxies[k])
			}
		}
	})
}

// exitChoice is one thing a proxy can go out through, in its menu.
type exitChoice struct {
	label    string
	via      api.InterfaceRef // an interface
	scene    string           // the rules of a scene, by its ID
	upstream bool             // the proxy it goes out through now
	other    bool             // asks for another proxy
}

// exitChoices is the menu of what p can go out through, and which of them
// it does: the interfaces; the rules of each scene; the proxy it goes out
// through, when it does; and a last choice that asks for one. A choice is
// what it is, not how it reads: labels that would read the same are told
// apart by a number.
func (a *App) exitChoices(p api.Proxy) ([]exitChoice, int) {
	var choices []exitChoice
	at := 0
	refs, labels := a.proxyExits(p.Via)
	for j, r := range refs {
		if p.Via.Name != "" && r.Key() == p.Via.Key() {
			at = len(choices)
		}
		choices = append(choices, exitChoice{label: labels[j], via: r})
	}
	for j := range a.cfg.Scenes {
		if a.cfg.Scenes[j].ID == p.Scene {
			at = len(choices)
		}
		choices = append(choices, exitChoice{label: locales.RulesOfScene(sceneName(&a.cfg.Scenes[j])), scene: a.cfg.Scenes[j].ID})
	}
	if p.Upstream != "" {
		at = len(choices)
		choices = append(choices, exitChoice{label: p.Upstream, upstream: true})
	}
	choices = append(choices, exitChoice{label: locales.AnotherProxy(), other: true})
	seen := map[string]bool{}
	for j := range choices {
		label := choices[j].label
		for n := 2; seen[label]; n++ {
			label = choices[j].label + " (" + strconv.Itoa(n) + ")"
		}
		choices[j].label = label
		seen[label] = true
	}
	return choices, at
}

// chooseExit sends the proxy at k out through what was chosen in its menu.
func (a *App) chooseExit(k int, choice exitChoice) {
	switch {
	case choice.other:
		a.settings.upstream = upstreamDialog{open: true, port: a.cfg.Proxies[k].Port, address: a.cfg.Proxies[k].Upstream}
	case choice.upstream:
		// What it goes out through already.
	case choice.scene != "":
		a.setProxyScene(k, choice.scene)
	default:
		a.change(func(cfg *api.Config) {
			cfg.Proxies[k].Via, cfg.Proxies[k].Upstream, cfg.Proxies[k].Scene = choice.via, "", ""
		})
	}
}

// rowToggle is a small button of a proxy's row that turns something of the
// proxy on and off, as sharing it with the devices: tinted while it is on,
// without a face until hovered while it is off.
func rowToggle(c *ui.Context, key string, ic *ui.SVG, label string, on bool) bool {
	t := c.Theme()
	b := ui.Box(c.Key(key)).Size(26, 26).Radius(7).Center().Label(label).Tooltip(label).Shrink(0).Cursor(ui.CursorPointer)
	col := t.TextMuted
	switch {
	case on:
		col = t.Accent
		b.Background(t.Accent.Alpha(0.14))
	case b.Hovered():
		b.Background(subtle(t, 0.08))
	}
	b.Children(func() { ui.Icon(c, ic).FontSize(15).TextColor(col) })
	return b.Clicked()
}

// exitChanging tells whether the public IP of the interface named name is
// being changed, as the backend says of a local proxy that goes out
// through it: by a button of Pith, or because what uses the proxy asked.
func (a *App) exitChanging(name string) bool {
	return a.exitsChanging(a.status)[name]
}

// exitsChanging is the interfaces, by name, whose public IP is being
// changed as st says of the local proxies that go out through them.
func (a *App) exitsChanging(st api.Status) map[string]bool {
	var names map[string]bool
	for _, ps := range st.Proxies {
		if !ps.Changing {
			continue
		}
		for _, p := range a.cfg.Proxies {
			if p.Port != ps.Port || p.Via.Name == "" {
				continue
			}
			if i := p.Via.Resolve(a.ifaces); i != nil {
				if names == nil {
					names = map[string]bool{}
				}
				names[i.Name] = true
			}
		}
	}
	return names
}

// proxyStatus is how the proxy on port is doing, as the backend last said.
func (a *App) proxyStatus(port int) api.ProxyStatus {
	for _, st := range a.status.Proxies {
		if st.Port == port {
			return st
		}
	}
	return api.ProxyStatus{Port: port}
}

// changeProxyIP changes the public IP of what the proxy on port goes out
// through: the Pith of the Mac its upstream is on has its hotspot or its
// router get another. The proxy's row says what it did.
func (a *App) changeProxyIP(port int) {
	s := &a.settings
	if s.changing[port] {
		return
	}
	if s.changing == nil {
		s.changing, s.changes = map[int]bool{}, map[int]api.ProxyIPChange{}
	}
	s.changing[port] = true
	delete(s.changes, port)
	var change api.ProxyIPChange
	a.async(func(ctx context.Context) (err error) {
		// As long as a router may take: see renewRouter.
		ctx, cancel := context.WithTimeout(ctx, 8*time.Minute)
		defer cancel()
		change, err = a.Service.ChangeProxyIP(ctx, port)
		return err
	}, func() {
		delete(s.changing, port)
		s.changes[port] = change
	}, func(error) { delete(s.changing, port) })
}

// proxyAddress is where a proxy listens, as the apps of this Mac type it,
// or, for a shared one, as the other devices do: on the first address of
// this Mac.
func (a *App) proxyAddress(p api.Proxy) string {
	host := "127.0.0.1"
	if addrs := a.ownAddresses(); p.Shared && len(addrs) > 0 {
		host = addrs[0]
	}
	return host + ":" + strconv.Itoa(p.Port)
}

// ownAddresses is the IPv4 addresses other devices reach this Mac at: those
// of the interfaces that are connected, the default one's first.
func (a *App) ownAddresses() []string {
	var first, rest []string
	for i := range a.ifaces {
		if !a.ifaces[i].Up {
			continue
		}
		for _, s := range a.ifaces[i].IPv4 {
			if addr, err := netip.ParseAddr(s); err != nil || addr.IsLoopback() || addr.IsLinkLocalUnicast() {
				continue
			}
			if a.ifaces[i].Primary {
				first = append(first, s)
			} else {
				rest = append(rest, s)
			}
		}
	}
	return append(first, rest...)
}

// proxyState says how a proxy is doing, under its address: off, what keeps
// it from working, that its IP is being changed, or that it listens, with
// what goes through it. A proxy that goes out through another Pith says
// what that one goes out through too, and its public IP, or what the last
// change here did to it.
func (a *App) proxyState(c *ui.Context, p api.Proxy, st api.ProxyStatus) {
	t := c.Theme()
	up := st.Upstream
	var code api.ErrorCode
	if st.Error != nil {
		code = st.Error.Code
	}
	ui.Row(c).Gap(6).AlignItems(ui.Center).Children(func() {
		switch {
		case !p.Enabled:
			stateLine(c, t.TextMuted, true, locales.ProxyOff(), t.TextMuted)
		case code == api.ErrPortInUse:
			stateLine(c, t.Danger, false, locales.PortInUse(), t.Danger)
		case code == api.ErrInterfaceUnavailable, code == api.ErrUpstreamUnreachable:
			stateLine(c, t.Warning, true, locales.NotConnected(), t.Warning)
		case code == api.ErrUpstreamRefused:
			stateLine(c, t.Warning, true, locales.NotAllowed(), t.Warning)
		case code == api.ErrDNSUnavailable:
			stateLine(c, t.Warning, true, locales.NoDns(), t.Warning)
		case st.Error != nil:
			stateLine(c, t.Danger, false, errorText(st.Error), t.Danger)
		case st.Listening && (st.Changing || a.settings.changing[p.Port]):
			stateLine(c, t.Warning, true, locales.ChangingItsIp(), t.TextMuted)
		case st.Listening && up != nil && !up.Online:
			stateLine(c, t.Warning, true, locales.NotConnected(), t.Warning)
		case st.Listening:
			words := []string{locales.ProxyListening()}
			if up != nil {
				if up.Interface != "" {
					words = append(words, up.Interface)
				}
				ip := cmp.Or(up.PublicIPv4, up.PublicIPv6)
				// What the last change did, while its address is the one in use.
				if ch, ok := a.settings.changes[p.Port]; ok && cmp.Or(ch.PublicIPv4, ch.PublicIPv6) == ip {
					ip = ipChangeText(ch.BeforeIPv4, ch.BeforeIPv6, ch.PublicIPv4, ch.PublicIPv6)
				}
				if ip != "" {
					words = append(words, ip)
				}
			}
			words = append(words, locales.PanelTotals(st.Open, byteSize(st.Received), byteSize(st.Sent)))
			stateLine(c, running(t), false, strings.Join(words, " · "), t.TextMuted)
		default:
			stateLine(c, t.TextMuted, true, locales.ProxyStarting(), t.TextMuted)
		}
	})
}

// stateLine is a dot and a few words under the title of a row.
func stateLine(c *ui.Context, col ui.Color, hollow bool, s string, text ui.Color) {
	dot(c, col, 6, hollow)
	ui.Text(c, s).FontSize(12).TextColor(text).FontFeatures("tnum").SingleLine().Shrink(1)
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

// setProxyScene sends the proxy at k out by the rules of a scene.
func (a *App) setProxyScene(k int, scene string) {
	a.change(func(cfg *api.Config) {
		cfg.Proxies[k].Via, cfg.Proxies[k].Upstream, cfg.Proxies[k].Scene = api.InterfaceRef{}, "", scene
		// By rules it has no one IP for what uses it to change.
		cfg.Proxies[k].ChangeIP = false
	})
}

// addProxy adds a proxy on the first free port from 1080, through the first
// connected interface no proxy goes out through yet.
func (a *App) addProxy(exits []api.InterfaceRef) {
	used, taken := map[int]bool{}, map[string]bool{}
	for _, p := range a.cfg.Proxies {
		used[p.Port] = true
		if p.Via.Name != "" {
			taken[p.Via.Key()] = true
		}
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

// setUpstream sends the proxy on port out through the proxy at address, a
// host and a port, as typed or as pasted from another app's settings; or
// says why it can't, and keeps what the proxy goes out through.
func (a *App) setUpstream(port int, address string) bool {
	address = strings.TrimSpace(address)
	for _, scheme := range []string{"socks5h://", "socks5://", "socks://"} {
		if len(address) > len(scheme) && strings.EqualFold(address[:len(scheme)], scheme) {
			address = address[len(scheme):]
		}
	}
	address = strings.TrimSuffix(address, "/")
	for k, p := range a.cfg.Proxies {
		if p.Port != port {
			continue
		}
		next := cloneConfig(a.cfg)
		next.Proxies[k].Via, next.Proxies[k].Upstream, next.Proxies[k].Scene = api.InterfaceRef{}, address, ""
		if address == "" || next.Validate() != nil {
			a.toast(locales.UseHostAndPort())
			return false
		}
		a.change(func(cfg *api.Config) { cfg.Proxies[k] = next.Proxies[k] })
		return true
	}
	return true // the proxy is gone: nothing to ask again
}

// upstreamView asks for the proxy a local proxy goes out through.
func (a *App) upstreamView(c *ui.Context) {
	d := &a.settings.upstream
	sheet(c, &d.open, 360, func() {
		sheetTitle(c, locales.GoOutThroughAnotherProxy())
		in := sheetInput(c, ui.TextInput(c.Key("upstream"), &d.address)).Placeholder(locales.HostPort()).AutoFocus().Margin(14, 0, 0)
		submit := in.Submitted()
		sheetFooter(c, nil, func() {
			if sheetButton(c, "cancel", locales.Cancel()).Clicked() {
				d.open = false
			}
			empty := strings.TrimSpace(d.address) == ""
			ok := sheetPrimary(c, "done", locales.Done(), empty).Clicked()
			if (ok || submit) && !empty && a.setUpstream(d.port, d.address) {
				d.open = false
			}
		})
	})
}

// ---- devices ----------------------------------------------------------------

// devicesGroup is the settings' group of the other devices that may use
// the shared proxies, there once a proxy is shared: a row a device, with
// what it moves; then the devices of a listed range, and those that tried
// and were refused, each with a button that allows it; and a last row with
// this Mac's addresses, which the devices type, and a button that adds one.
func (a *App) devicesGroup(c *ui.Context) {
	shared := false
	for _, p := range a.cfg.Proxies {
		shared = shared || p.Shared
	}
	if !shared && len(a.cfg.Devices) == 0 {
		return
	}
	t := c.Theme()
	group(c, locales.Devices(), func() {
		// Drawn from the list as it is now, as the proxies are.
		devices := slices.Clone(a.cfg.Devices)
		listed := map[string]bool{}
		remove := -1
		for k, d := range devices {
			address, removed := a.deviceRow(c, k, d)
			listed[address] = true
			if removed {
				remove = k
			}
		}
		if remove >= 0 {
			address := devices[remove].Address
			a.settings.renaming = ""
			a.change(func(cfg *api.Config) {
				cfg.Devices = slices.DeleteFunc(slices.Clone(cfg.Devices), func(d api.Device) bool { return d.Address == address })
			})
		}
		full := len(a.cfg.Devices) >= api.MaxDevices
		for _, st := range a.status.Devices {
			if !listed[st.Address] {
				a.visitorRow(c, st, full)
			}
		}
		ui.Row(c).Gap(16).Padding(9, settingsInset).MinHeight(52).Children(func() {
			ui.Row(c).Grow(1).MinWidth(0).Gap(8).AlignItems(ui.Center).Children(func() {
				if addrs := a.ownAddresses(); len(addrs) > 0 {
					ui.Text(c, locales.ThisMac()).FontSize(12).TextColor(t.TextMuted).SingleLine().Shrink(0)
					ui.Text(c, strings.Join(addrs, " · ")).FontSize(12).FontFeatures("tnum").SingleLine().Shrink(1)
				}
			})
			add := glassCapsuleButton(c, "add-device", icPlus, locales.AddDevice()).Focusable().Shrink(0).Disabled(full)
			if add.Clicked() && !full {
				a.settings.device = deviceDialog{open: true}
			}
		})
	})
}

// deviceRow is the row of a listed device: its name, which a double-click
// changes, or its address; under it, its address when it has a name, and
// what it moves through the shared proxies; on the right, a button that
// removes it. It returns the device's address, empty for a range, and
// whether that button was clicked.
func (a *App) deviceRow(c *ui.Context, k int, d api.Device) (single string, remove bool) {
	t := c.Theme()
	s := &a.settings
	address := d.Address
	if prefix, err := d.Prefix(); err == nil {
		address = prefix.String()
		if prefix.IsSingleIP() {
			address = prefix.Addr().String()
			single = address
		}
	}
	ui.Row(c.Key(fmt.Sprint("device-", k))).Gap(16).Padding(9, settingsInset).MinHeight(52).Children(func() {
		ui.Column(c).Grow(1).MinWidth(0).Gap(2).Children(func() {
			if s.renaming == d.Address {
				in := networkField(c, ui.TextInput(c.Key("device-name"), &s.deviceName)).Padding(1, 10).FontWeight(500).Width(220).AutoFocus().Placeholder(address)
				if in.Submitted() {
					a.renameDevice(d.Address, s.deviceName)
				}
			} else {
				title := d.Name
				if title == "" {
					title = address
				}
				name := ui.Text(c, title).FontWeight(500).FontFeatures("tnum").SingleLine().Cursor(ui.CursorText).Tooltip(locales.DoubleClickToRename())
				if name.DoubleClicked() {
					s.renaming, s.deviceName = d.Address, d.Name
				}
			}
			ui.Row(c).Gap(6).AlignItems(ui.Center).Children(func() {
				prefix := ""
				if d.Name != "" {
					prefix = address + " · "
				}
				st, seen := a.deviceStatus(single)
				switch {
				case single == "":
					if d.Name != "" {
						ui.Text(c, address).FontSize(12).TextColor(t.TextMuted).FontFeatures("tnum").SingleLine()
					}
				case seen:
					stateLine(c, running(t), st.Open == 0, prefix+locales.PanelTotals(st.Open, byteSize(st.Received), byteSize(st.Sent)), t.TextMuted)
				default:
					stateLine(c, t.TextMuted, true, prefix+locales.DeviceIdle(), t.TextMuted)
				}
			})
		})
		remove = quietButton(c, "remove-device", icX, locales.Remove(), false)
	})
	return single, remove
}

// renameDevice names the device listed by address, wherever it is in the
// list by now, and ends its renaming.
func (a *App) renameDevice(address, name string) {
	a.settings.renaming = ""
	name = strings.TrimSpace(name)
	a.change(func(cfg *api.Config) {
		for k := range cfg.Devices {
			if cfg.Devices[k].Address == address {
				cfg.Devices[k].Name = name
			}
		}
	})
}

// deviceStatus is what the device at address moved through the shared
// proxies, if it connected since Pith opened.
func (a *App) deviceStatus(address string) (api.DeviceStatus, bool) {
	for _, st := range a.status.Devices {
		if st.Address == address && address != "" {
			return st, true
		}
	}
	return api.DeviceStatus{}, false
}

// visitorRow is the row of a device that connected without being listed by
// its own address: one of a listed range, with what it moves, or one that
// was refused, with a button that allows it.
func (a *App) visitorRow(c *ui.Context, st api.DeviceStatus, full bool) {
	t := c.Theme()
	ui.Row(c.Key("visitor-"+st.Address)).Gap(16).Padding(9, settingsInset).MinHeight(52).Children(func() {
		ui.Column(c).Grow(1).MinWidth(0).Gap(2).Children(func() {
			ui.Text(c, st.Address).FontWeight(500).FontFeatures("tnum").SingleLine()
			ui.Row(c).Gap(6).AlignItems(ui.Center).Children(func() {
				if st.Allowed {
					stateLine(c, running(t), st.Open == 0, locales.PanelTotals(st.Open, byteSize(st.Received), byteSize(st.Sent)), t.TextMuted)
				} else {
					stateLine(c, t.Warning, true, locales.DeviceRefused(), t.Warning)
				}
			})
		})
		if st.Allowed {
			return
		}
		allow := glassCapsuleButton(c, "allow-device", icCheck, locales.Allow()).Focusable().Shrink(0).Disabled(full)
		if allow.Clicked() && !full {
			a.addDevice(st.Address, "")
		}
	})
}

// addDevice lists the device at address, an IPv4 address or a range of
// them, among those that may use the shared proxies; or says why it can't.
func (a *App) addDevice(address, name string) bool {
	d := api.Device{Address: strings.TrimSpace(address), Name: strings.TrimSpace(name)}
	if prefix, err := d.Prefix(); err == nil {
		// As Pith compares it: 192.0.2.10, 192.0.2.0/24.
		if d.Address = prefix.String(); prefix.IsSingleIP() {
			d.Address = prefix.Addr().String()
		}
	}
	next := cloneConfig(a.cfg)
	next.Devices = append(next.Devices, d)
	if next.Validate() != nil {
		a.toast(locales.UseAnIpv4AddressOrRange())
		return false
	}
	a.change(func(cfg *api.Config) { cfg.Devices = append(cfg.Devices, d) })
	return true
}

// deviceView asks for a device to list: its address, and a name if wanted.
func (a *App) deviceView(c *ui.Context) {
	d := &a.settings.device
	sheet(c, &d.open, 360, func() {
		sheetTitle(c, locales.AddDevice())
		address := sheetInput(c, ui.TextInput(c.Key("device-address"), &d.address)).Placeholder(locales.IpAddressOrRange()).AutoFocus().Margin(14, 0, 0)
		name := sheetInput(c, ui.TextInput(c.Key("device-name"), &d.name)).Placeholder(locales.DeviceName()).Margin(8, 0, 0)
		submit := address.Submitted() || name.Submitted()
		sheetFooter(c, nil, func() {
			if sheetButton(c, "cancel", locales.Cancel()).Clicked() {
				d.open = false
			}
			empty := strings.TrimSpace(d.address) == ""
			ok := sheetPrimary(c, "done", locales.Done(), empty).Clicked()
			if (ok || submit) && !empty && a.addDevice(d.address, d.name) {
				d.open = false
			}
		})
	})
}
