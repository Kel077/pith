package ui

import (
	"slices"
	"strings"

	"github.com/egoist/mygo/ui"
	"github.com/Kel077/pith/internal/api"
)

// notice is something the user should know about routing: a warning of
// the backend, or why routing stopped.
type notice struct {
	text   string
	danger bool
	action string
	do     func()
}

// notices are what the rules page and the panel show above everything.
func (a *App) notices() []notice {
	var out []notice
	if a.status.State == api.Failed && a.status.Error != nil {
		out = append(out, notice{
			text:   tr("Routing stopped: ", "分流已停止：") + errorText(a.status.Error),
			danger: true,
			action: tr("Try Again", "重试"),
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
			out = append(out, notice{text: trf("Another VPN (%s) carries all traffic: connections no rule matches still go through it.",
				"另一个 VPN（%s）正在接管所有流量，未命中规则的连接仍会经过它。", w.Detail)})
		case api.WarnInterfaceDown:
			down = append(down, a.ifaceLabel(api.InterfaceRef{Name: w.Detail}))
		case api.WarnNoDomains:
			out = append(out, notice{
				text:   tr("Recognizing domains is off, so domain rules may not match.", "识别域名已关闭，域名规则可能匹配不到。"),
				action: tr("Turn On", "打开"),
				do: func() {
					a.change(func(cfg *api.Config) { cfg.Settings.SniffDomains = true })
				},
			})
		}
	}
	if len(down) > 0 {
		out = append(out, notice{text: trf("%s is not connected: its rules use their next interface.", "%s 没有连接，用到它的规则会改用下一张网卡。", list(down))})
	}
	return out
}

func cmpOr(a, b string) string {
	if a != "" {
		return a
	}
	return b
}

// noticeBar shows a notice in a tinted bar.
func noticeBar(c *ui.Context, n notice, compact bool) {
	t := c.Theme()
	col := t.Warning
	ic := icAlert
	if n.danger {
		col = t.Danger
	}
	bar := ui.Row(c).Gap(10).Padding(9, 12).Radius(10).Background(col.Alpha(0.1))
	if compact {
		bar.Padding(7, 10).Gap(8)
	}
	bar.Children(func() {
		ui.Icon(c, ic).TextColor(col).Shrink(0)
		text := ui.Text(c, n.text).Grow(1).MinWidth(0)
		if compact {
			text.FontSize(12)
		}
		if n.action != "" && n.do != nil {
			if ui.Button(c, n.action).Clicked() {
				n.do()
			}
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
