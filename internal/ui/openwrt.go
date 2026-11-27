package ui

import (
	"slices"
	"strconv"
	"strings"

	"github.com/egoist/mygo/ui"

	"github.com/Kel077/pith/internal/api"
	"github.com/Kel077/pith/locales"
)

type openWrtDialog struct {
	open     bool
	settings api.OpenWrtRouter
	problem  string
}

func canConfigureOpenWrt(link *api.Interface) bool {
	return link.CanProxy() && (link.Kind == api.KindEthernet || link.Kind == api.KindWiFi)
}

func sameGatewayLink(a api.Interface, b *api.Interface) bool {
	return b != nil && a.Name == b.Name && a.HardwareAddr == b.HardwareAddr && a.HardwarePort == b.HardwarePort &&
		a.Kind == b.Kind && a.Up == b.Up && a.Router == b.Router && slices.Equal(a.IPv4, b.IPv4)
}

func (a *App) selectedOpenWrt(routers []api.OpenWrtRouter, name string) api.OpenWrtRouter {
	if i := api.OpenWrtIndex(routers, a.ifaces, name); i >= 0 {
		return routers[i]
	}
	return api.OpenWrtRouter{}
}

func (a *App) openWrtConfig(name string) api.OpenWrtRouter {
	return a.selectedOpenWrt(a.cfg.OpenWrt, name)
}

func (a *App) openWrtPending(name string) bool {
	return a.openWrtConfig(name) != a.selectedOpenWrt(a.router.saved, name)
}

func (a *App) clearGatewayStatus(name string) {
	delete(a.hotspot.info, name)
	delete(a.hotspot.none, name)
	delete(a.router.info, name)
	delete(a.router.renewals, name)
	delete(a.router.none, name)
	delete(a.router.problems, name)
}

func (a *App) openWrtConfigChanged(previous []api.OpenWrtRouter) {
	if slices.Equal(previous, a.cfg.OpenWrt) {
		return
	}
	for _, link := range a.ifaces {
		if a.selectedOpenWrt(previous, link.Name) != a.openWrtConfig(link.Name) {
			a.clearGatewayStatus(link.Name)
		}
	}
	a.hotspotNames()
	a.routerNames()
}

func (a *App) savedOpenWrt(config []api.OpenWrtRouter) {
	if slices.Equal(a.router.saved, config) {
		return
	}
	a.router.saved = slices.Clone(config)
	a.hotspotNames()
	a.routerNames()
	a.kickGateways()
}

func (a *App) openWrtSettings(c *ui.Context, link *api.Interface) {
	if !canConfigureOpenWrt(link) {
		return
	}
	label := locales.OpenWrtRouterSettings()
	b := glassCapsuleButton(c, "openwrt-settings", icTerminal, "").Shrink(0).Label(label).Tooltip(label).
		Disabled(!a.loaded || a.router.renewing[link.Name] || a.exitChanging(link.Name))
	if b.Clicked() {
		cfg := a.openWrtConfig(link.Name)
		cfg.Via = refOf(link)
		a.router.openWrt = openWrtDialog{open: true, settings: cfg}
	}
}

func (a *App) openWrtView(c *ui.Context) {
	d := &a.router.openWrt
	sheet(c, &d.open, 444, func() {
		sheetTitle(c, locales.OpenWrtOverSsh())
		muted(c, a.ifaceLabel(d.settings.Via)).Margin(3, 0, 12)
		var submitted bool
		sheetScroll(c, func() {
			ui.Column(c).Gap(10).Children(func() {
				for i, f := range []struct {
					key, label, hint string
					value            *string
				}{
					{"host", locales.SshHost(), "router.example.com", &d.settings.Host},
					{"user", locales.SshUser(), locales.UseSshConfig(), &d.settings.User},
					{"identity", locales.SshIdentityFile(), locales.UseSshConfig(), &d.settings.IdentityFile},
					{"command", locales.WanRotationCommand(), api.DefaultOpenWrtCommand, &d.settings.Command},
				} {
					ui.Column(c).Gap(4).Children(func() {
						muted(c, f.label).FontSize(12)
						input := sheetInput(c, ui.TextInput(c.Key(f.key), f.value)).Label(f.label).Placeholder(f.hint)
						if i == 0 {
							input.AutoFocus()
						}
						if input.Changed() {
							d.problem = ""
						}
						submitted = submitted || input.Submitted()
					})
				}
				if d.problem != "" {
					ui.Text(c, d.problem).FontSize(12).TextColor(c.Theme().Danger)
				}
			})
		})
		sheetFooter(c, func() {
			if a.openWrtConfig(d.settings.Via.Name).Host != "" && sheetButton(c, "remove", locales.Remove()).Clicked() {
				a.editOpenWrt(d.settings, true)
				d.open = false
			}
		}, func() {
			if sheetButton(c, "cancel", locales.Cancel()).Clicked() {
				d.open = false
			}
			missingHost := strings.TrimSpace(d.settings.Host) == ""
			if (sheetPrimary(c, "save", locales.Save(), missingHost).Clicked() || submitted) && !missingHost {
				r := d.settings
				r.Host, r.User = strings.TrimSpace(r.Host), strings.TrimSpace(r.User)
				r.IdentityFile, r.Command = strings.TrimSpace(r.IdentityFile), strings.TrimSpace(r.Command)
				if err := r.Validate(); err != nil {
					d.problem = errorText(err)
					return
				}
				a.editOpenWrt(r, false)
				d.open = false
			}
		})
	})
}

func (a *App) editOpenWrt(router api.OpenWrtRouter, remove bool) {
	a.change(func(cfg *api.Config) {
		index := api.OpenWrtIndex(cfg.OpenWrt, a.ifaces, router.Via.Name)
		routers := slices.Clone(cfg.OpenWrt)
		switch {
		case remove && index >= 0:
			routers = slices.Delete(routers, index, index+1)
		case remove:
		case index >= 0:
			routers[index] = router
		default:
			routers = append(routers, router)
		}
		cfg.OpenWrt = routers
	})
}

func openWrtRenewal(c *ui.Context, r api.RouterRenewal) {
	w := r.WAN
	for _, f := range []struct{ label, before, after string }{
		{"IPv4", r.Before.PublicIPv4, r.After.PublicIPv4},
		{"IPv6", r.Before.PublicIPv6, r.After.PublicIPv6},
		{locales.WanIpv6Prefix(), w.BeforeIPv6Prefix, w.IPv6Prefix},
	} {
		if f.before != "" || f.after != "" {
			renewalLine(c, f.label+"  "+observedAddressChange(f.before, f.after)).FontSize(12)
		}
	}
	if w.BeforeIPv4 != r.Before.PublicIPv4 || w.IPv4 != r.After.PublicIPv4 {
		renewalLine(c, locales.WanIpv4()+"  "+observedAddressChange(w.BeforeIPv4, w.IPv4)).FontSize(12)
	}
	if w.MACChanged {
		muted(c, locales.OpenWrtWanMacChanged(strconv.FormatFloat(r.Took.Seconds(), 'f', 1, 64))).FontSize(12)
	}
}
