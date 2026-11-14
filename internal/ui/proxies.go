package ui

import (
	"fmt"
	"net/netip"
	"slices"
	"strconv"
	"strings"

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
		// A row is removed once all are drawn: the rows after it are still
		// drawn from the list as it was.
		remove := -1
		for k := range a.cfg.Proxies {
			if a.proxyRow(c, k) {
				remove = k
			}
		}
		if remove >= 0 {
			a.change(func(cfg *api.Config) {
				cfg.Proxies = append(cfg.Proxies[:remove:remove], cfg.Proxies[remove+1:]...)
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
// port of, and how it is doing; on the right, what it goes out through, a
// button that shares it with the devices, its switch, and a button that
// removes it: proxyRow tells when that one is clicked.
func (a *App) proxyRow(c *ui.Context, k int) (remove bool) {
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
				addr := ui.Text(c, a.proxyAddress(p)).FontWeight(500).FontFeatures("tnum").SingleLine().Cursor(ui.CursorText).Tooltip(locales.DoubleClickToChangeThePort())
				if addr.DoubleClicked() {
					s.editingPort, s.port = p.Port, strconv.Itoa(p.Port)
				}
			}
			a.proxyState(c, p)
		})
		ui.Row(c).Shrink(0).Gap(12).AlignItems(ui.Center).Children(func() {
			choices, at := a.exitChoices(p)
			options := make([]string, len(choices))
			for j := range choices {
				options[j] = choices[j].label
			}
			chosen := options[at]
			if settingsSelect(c, &chosen, options).Label(locales.Interfaces()).Changed() {
				if j := slices.Index(options, chosen); j >= 0 {
					a.chooseExit(k, choices[j])
				}
			}
			if shareButton(c, p.Shared) {
				a.change(func(cfg *api.Config) { cfg.Proxies[k].Shared = !p.Shared })
			}
			on := p.Enabled
			if ui.Switch(c, &on).Label("127.0.0.1:" + strconv.Itoa(p.Port)).Changed() {
				a.change(func(cfg *api.Config) { cfg.Proxies[k].Enabled = on })
			}
			remove = quietButton(c, "remove-proxy", icX, locales.Remove(), false)
		})
	})
	return remove
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

// shareButton is the small button that shares a proxy with the devices:
// tinted while it does, without a face until hovered while it doesn't.
func shareButton(c *ui.Context, on bool) bool {
	t := c.Theme()
	b := ui.Box(c.Key("share-proxy")).Size(26, 26).Radius(7).Center().Label(locales.ShareWithDevices()).Tooltip(locales.ShareWithDevices()).Shrink(0).Cursor(ui.CursorPointer)
	col := t.TextMuted
	switch {
	case on:
		col = t.Accent
		b.Background(t.Accent.Alpha(0.14))
	case b.Hovered():
		b.Background(subtle(t, 0.08))
	}
	b.Children(func() { ui.Icon(c, icShare).FontSize(15).TextColor(col) })
	return b.Clicked()
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
		case st.Listening:
			stateLine(c, running(t), false, locales.ProxyListening()+" · "+locales.PanelTotals(st.Open, byteSize(st.Received), byteSize(st.Sent)), t.TextMuted)
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
		listed := map[string]bool{}
		remove := -1
		for k := range a.cfg.Devices {
			address, removed := a.deviceRow(c, k)
			listed[address] = true
			if removed {
				remove = k
			}
		}
		if remove >= 0 {
			a.settings.renaming = 0
			a.change(func(cfg *api.Config) {
				cfg.Devices = append(cfg.Devices[:remove:remove], cfg.Devices[remove+1:]...)
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
func (a *App) deviceRow(c *ui.Context, k int) (single string, remove bool) {
	t := c.Theme()
	d := a.cfg.Devices[k]
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
			if s.renaming == k+1 {
				in := networkField(c, ui.TextInput(c.Key("device-name"), &s.deviceName)).Padding(1, 10).FontWeight(500).Width(220).AutoFocus().Placeholder(address)
				if in.Submitted() {
					name := strings.TrimSpace(s.deviceName)
					s.renaming = 0
					a.change(func(cfg *api.Config) { cfg.Devices[k].Name = name })
				}
			} else {
				title := d.Name
				if title == "" {
					title = address
				}
				name := ui.Text(c, title).FontWeight(500).FontFeatures("tnum").SingleLine().Cursor(ui.CursorText).Tooltip(locales.DoubleClickToRename())
				if name.DoubleClicked() {
					s.renaming, s.deviceName = k+1, d.Name
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
