package ui

import (
	"slices"
	"strings"

	"github.com/egoist/mygo/plugins/glass"
	"github.com/egoist/mygo/ui"

	"github.com/Kel077/pith/internal/api"
	"github.com/Kel077/pith/locales"
)

// notice is something the user should know about routing: a warning of
// the backend, or why routing stopped.
type notice struct {
	text   string // as short as it can be
	detail string // the whole of it, shown on hover
	danger bool
	action string
	do     func()
}

// notices are what the rules page and the panel show above everything.
func (a *App) notices() []notice {
	var out []notice
	if helperUntrusted(a.status) {
		// No "Try again" and no password prompt: the fix is an explicit
		// reinstall in Settings, or of Pith itself.
		out = append(out, notice{
			text:   locales.PithCanTVerifyItsHelper(),
			danger: true,
			action: locales.Settings(),
			do: func() {
				a.page = "settings"
				if a.OpenWindow != nil {
					a.OpenWindow()
				}
			},
		})
	} else if a.status.State == api.Failed && a.status.Error != nil {
		out = append(out, notice{
			text:   locales.RoutingStopped() + errorText(a.status.Error),
			danger: true,
			action: locales.TryAgain(),
			do:     func() { a.start(cmpOr(a.status.SceneID, a.cfg.Selected)) },
		})
	}
	if !a.isRunning() {
		return out
	}
	var down []string
	for _, w := range a.status.Warnings {
		switch w.Code {
		case api.WarnVPN:
			out = append(out, notice{text: locales.WarnVpnShort(w.Detail), detail: locales.WarnVpn(w.Detail)})
		case api.WarnInterfaceDown:
			down = append(down, a.ifaceLabel(api.InterfaceRef{Name: w.Detail}))
		case api.WarnDNSUnobserved:
			// Not shown: on most home networks the router is the DNS server,
			// so a bar would show at every start, while domain rules still
			// work from the names connections carry (TLS, HTTP, QUIC).
		case api.WarnNoDomains:
			out = append(out, notice{
				text:   locales.RecognizingDomainsIsOff(),
				detail: locales.RecognizingDomainsIsOffSoDomain(),
				action: locales.TurnOnDomains(),
				do: func() {
					a.change(func(cfg *api.Config) { cfg.Settings.SniffDomains = true })
				},
			})
		}
	}
	if len(down) > 0 {
		out = append(out, notice{text: locales.WarnInterfaceDownShort(list(down)), detail: locales.WarnInterfaceDown(list(down))})
	}
	return out
}

func cmpOr(a, b string) string {
	if a != "" {
		return a
	}
	return b
}

// noticeBar shows a notice in a bar of glass tinted amber, or red for a
// failure, with its action as a capsule; the whole of it shows on hover.
// Compact is the menu bar panel's: smaller, and tinted rather than glass,
// as glass over the system's material of the panel would show the
// panel's emptiness, gray.
func noticeBar(c *ui.Context, n notice, compact bool) {
	t := c.Theme()
	col := t.Warning
	ic := icAlert
	if n.danger {
		col = t.Danger
	}
	bar := ui.Row(c).Gap(10).Padding(10, 10, 10, 14).Radius(14)
	if compact {
		fill := col.Alpha(0.13)
		if t.Dark {
			fill = col.Alpha(0.17)
		}
		bar.Padding(7, 7, 7, 11).Gap(8).Radius(12).Background(fill).Border(1, col.Alpha(0.16))
	} else {
		bar.Material(glass.Glass{Tint: col.Alpha(0.16), Clarity: 0.5}).Border(1, rim(t))
	}
	if n.detail != "" {
		bar.Tooltip(n.detail)
	}
	bar.Children(func() {
		icon := ui.Icon(c, ic).TextColor(col).Shrink(0)
		text := ui.Text(c, n.text).Grow(1).MinWidth(0)
		if compact {
			icon.FontSize(13)
			text.FontSize(12).FontWeight(500)
		}
		if n.action == "" || n.do == nil {
			return
		}
		var b ui.Element
		if compact {
			b = ui.Row(c.Key("notice-action")).Padding(4, 11).Radius(999).Cursor(ui.CursorPointer).Label(n.action).
				Background(col.Alpha(0.16)).Focusable()
			if b.Hovered() {
				b.Background(col.Alpha(0.24))
			}
			b.Children(func() { ui.Text(c, n.action).FontSize(12).FontWeight(600).TextColor(col).SingleLine() })
		} else {
			b = glassCapsuleButton(c, "notice-action", nil, n.action).Focusable()
		}
		if b.Clicked() {
			n.do()
		}
	})
}

// shadowedBy returns the 1-based number of the rule that keeps the rule at
// index i from ever deciding, or 0: an earlier enabled route rule, or any
// enabled block rule, that matches every connection rule i matches.
func shadowedBy(rules []api.Rule, i int) int {
	r := rules[i]
	if !r.Enabled {
		return 0
	}
	for j, o := range rules {
		if j == i || !o.Enabled {
			continue
		}
		earlierRoute := j < i && o.Action == api.Route && r.Action == api.Route
		blocks := o.Action == api.Block && (r.Action == api.Route || j < i)
		if (earlierRoute || blocks) && covers(o, r) {
			return j + 1
		}
	}
	return 0
}

// covers reports whether rule o matches every connection rule r matches.
func covers(o, r api.Rule) bool {
	if o.Protocol != api.AnyProtocol && o.Protocol != r.Protocol {
		return false
	}
	if len(o.Apps) > 0 {
		if len(r.Apps) == 0 {
			return false
		}
		// An app matches by its path, or anywhere by its bundle ID: o's
		// app covers r's when it matches all of that too.
		for _, app := range r.Apps {
			if !slices.ContainsFunc(o.Apps, func(x api.AppRef) bool {
				return x.BundleID != "" && x.BundleID == app.BundleID || x.Path == app.Path && app.BundleID == ""
			}) {
				return false
			}
		}
	}
	if len(o.Targets) > 0 {
		if len(r.Targets) == 0 {
			return false
		}
		for _, s := range r.Targets {
			t, err := api.ParseTarget(s)
			if err != nil {
				return false
			}
			if !slices.ContainsFunc(o.Targets, func(os string) bool {
				ot, err := api.ParseTarget(os)
				if err != nil {
					return false
				}
				switch {
				case t.IsDomain() && ot.IsDomain():
					return ot.MatchHost(t.Domain) && (!t.Subdomains || ot.Subdomains && (t.Domain == ot.Domain || strings.HasSuffix(t.Domain, "."+ot.Domain)))
				case !t.IsDomain() && !ot.IsDomain():
					return ot.Prefix.Bits() <= t.Prefix.Bits() && ot.Prefix.Contains(t.Prefix.Addr())
				}
				return false
			}) {
				return false
			}
		}
	}
	return true
}
