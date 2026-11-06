package ui

import (
	"context"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/egoist/mygo/plugins/glass"
	"github.com/egoist/mygo/ui"

	"github.com/Kel077/pith/internal/api"
	"github.com/Kel077/pith/locales"
)

// activityPage is the state of the activity page.
type activityPage struct {
	view        int // 0 connections, 1 apps, 2 traffic
	span        int // of the traffic: 0 an hour, 1 a day, 2 a week
	history     api.Traffic
	historyErr  error
	historyKey  string // the span and minute history was read for
	historyBusy bool
	query       string
	paused      bool
	frozen      []*api.Connection // what shows while paused
	conns       ui.ListState
	apps        ui.ListState
}

// activityView shows what Pith routes: the speed through each interface,
// and every connection with the interface it really went out through.
func (a *App) activityView(c *ui.Context) {
	p := &a.activity
	pageHeader(c, locales.Activity(), "", func() {
		// The capsules are not all as high: centered on each other.
		ui.Row(c).Gap(8).Children(func() {
			segmentedCapsule(c, "activity-view", locales.Show(), &p.view, []string{locales.Connections(), locales.Apps(), locales.Traffic()}, nil)
			// Traffic has no use for the tools, which keep their room all the
			// same: the capsule of the views stays where it is.
			tools := ui.Row(c).Gap(8).AlignItems(ui.Center)
			if p.view == 2 {
				tools.Invisible()
			}
			tools.Children(func() { a.activityTools(c) })
		})
	})
	if p.view == 2 {
		a.trafficView(c)
		return
	}
	if !a.isRunning() {
		a.activityOffCapsule(c)
	} else {
		a.ifaceStrip(c)
	}
	if p.view == 1 {
		a.appsTable(c)
		return
	}
	a.connectionsTable(c)
}

// activitySearch filters the connections and the apps: a search field
// shaped as a capsule of glass, as high as the buttons beside it.
func activitySearch(c *ui.Context, query *string) {
	t := c.Theme()
	ui.SearchField(c.Key("activity-search"), query).Label(locales.Filter()).Placeholder(locales.Filter()).
		Width(200).Height(34).Padding(0, 14, 0, 12).Radius(999).Material(glass.Glass{}).Border(1, rim(t))
}

// activityOffCapsule says that routing is off, so that no connection
// comes, and turns it on: the state as the routing capsule shows it, and
// the action, in a capsule of glass; what the page shows is its tooltip.
func (a *App) activityOffCapsule(c *ui.Context) {
	t := c.Theme()
	ui.Row(c).Padding(0, 32, 14).Shrink(0).Children(func() {
		bar := capsule(c.Key("routing-off")).Gap(10).Padding(4, 4, 4, 14).Tooltip(locales.RoutingIsOffThisPageShows())
		bar.Children(func() {
			dot(c, t.TextMuted, 8, true)
			ui.Text(c, locales.NotRouting()).FontWeight(500).SingleLine()
			busy := a.busy != "" || a.status.State == api.Starting
			on := primaryCapsule(c, "routing-on", icPower, locales.TurnOnRouting()).Padding(6, 14, 6, 12).Focusable().Disabled(busy)
			if busy {
				on.Opacity(0.5)
			}
			if on.Clicked() {
				a.start(a.cfg.Selected)
			}
		})
	})
}

// ifaceStrip shows, for each interface that carried traffic in the last
// two minutes, its speed now and a chart of it.
func (a *App) ifaceStrip(c *ui.Context) {
	series := map[string][]api.TrafficSample{}
	for _, s := range a.traffic.Series {
		series[s.Interface] = append(series[s.Interface], s)
	}
	names := make([]string, 0, len(series))
	for name := range series {
		names = append(names, name)
	}
	slices.SortFunc(names, func(x, y string) int {
		return strings.Compare(a.ifaceLabel(api.InterfaceRef{Name: x}), a.ifaceLabel(api.InterfaceRef{Name: y}))
	})
	if len(names) == 0 {
		return
	}
	// The second the traffic was read in is not over: count from the one
	// before it.
	now := a.trafficAt.Truncate(time.Second)
	ui.Row(c).Padding(0, 32, 14).Gap(12).Shrink(0).Children(func() {
		for _, name := range names {
			a.ifaceSpeedCard(c, name, series[name], now)
		}
	})
}

// ifaceSpeedCard is a small pane of an interface's speed: now, and over
// the last minute.
func (a *App) ifaceSpeedCard(c *ui.Context, name string, samples []api.TrafficSample, now time.Time) {
	t := c.Theme()
	col := a.colorOf(name)
	// The last full second is the speed now; the chart is the last minute.
	const span = 60
	var down, up [span]float32
	var nowDown, nowUp uint64
	for _, s := range samples {
		k := int(now.Sub(s.At) / time.Second)
		if k >= 1 && k <= span {
			down[span-k] = float32(s.Received)
			up[span-k] = float32(s.Sent)
		}
		if k == 1 {
			nowDown, nowUp = s.Received, s.Sent
		}
	}
	pane(c.Key("speed-"+name)).Grow(1).Basis(0).MinWidth(180).Padding(12, 16, 12).Gap(8).Children(func() {
		ui.Row(c).Gap(8).Children(func() {
			dot(c, col, 8, false)
			ui.Text(c, a.ifaceLabel(api.InterfaceRef{Name: name})).FontWeight(600).SingleLine().Grow(1).MinWidth(0)
			ui.Text(c, "↓ "+rate(nowDown)).FontSize(12).FontWeight(600).FontFeatures("tnum").SingleLine()
			ui.Text(c, "↑ "+rate(nowUp)).FontSize(12).FontFeatures("tnum").TextColor(t.TextMuted).SingleLine()
		})
		sparkline(c, down[:], up[:], col).Height(36)
	})
}

// sparkline draws received bytes as a line over an area fading out below
// it, and sent bytes as a fainter line, on one scale.
func sparkline(c *ui.Context, down, up []float32, col ui.Color) ui.Element {
	return ui.Box(c).FillWidth().Draw(func(p *ui.Painter, r ui.Rect) {
		peak := float32(1)
		for i := range down {
			peak = max(peak, down[i], up[i])
		}
		n := len(down)
		pt := func(i int, v float32) (float32, float32) {
			return r.X + r.W*float32(i)/float32(n-1), r.Y + r.H - 1 - (r.H-2)*v/peak
		}
		area := &ui.Path{}
		line := make([][2]float32, n)
		for i, v := range down {
			x, y := pt(i, v)
			line[i] = [2]float32{x, y}
			if i == 0 {
				area.MoveTo(x, r.Y+r.H)
			}
			area.LineTo(x, y)
		}
		area.LineTo(r.X+r.W, r.Y+r.H).Close()
		p.FillPathGradient(area, ui.LinearGradient{From: col.Alpha(0.30), To: col.Alpha(0.02), Angle: 180})
		p.StrokePolyline(line, 1.5, col)
		upLine := make([][2]float32, n)
		for i, v := range up {
			x, y := pt(i, v)
			upLine[i] = [2]float32{x, y}
		}
		p.StrokePolyline(upLine, 1, col.Alpha(0.45))
	})
}

// shownConns are the connections the table shows: all, or those frozen by
// a pause, filtered by the search.
func (a *App) shownConns() []*api.Connection {
	p := &a.activity
	src := a.conns
	if p.paused {
		src = p.frozen
	}
	q := strings.ToLower(strings.TrimSpace(p.query))
	if q == "" {
		return src
	}
	out := make([]*api.Connection, 0, len(src))
	for _, cn := range src {
		if strings.Contains(strings.ToLower(appName(cn.App)+" "+cn.Domain+" "+cn.Remote+" "+cn.Process.Name+" "+cn.Interface), q) {
			out = append(out, cn)
		}
	}
	return out
}

// Column widths of the connections table.
const (
	colTime  = 68
	colApp   = 170
	colRoute = 190
	colBytes = 120
	// tablePadX is the room between a table's pane and its columns.
	tablePadX = 18
)

// activityTable holds a table of the page in one pane of glass: the titles
// of its columns, then its rows divided by hairlines, as many as fit; the
// rows scroll in the pane when the window is too short for them.
func activityTable(c *ui.Context, content func()) {
	ui.Column(c).Grow(1).MinHeight(0).Padding(0, 32, 20).Children(func() {
		pane(c).Shrink(1).MinHeight(0).Clip().Children(content)
	})
}

// activityTableHead is the row of a table's column titles, over a hairline.
func activityTableHead(c *ui.Context, titles func()) {
	ui.Row(c).Padding(10, tablePadX, 9).Gap(12).Shrink(0).BorderWidth(0, 0, 1, 0).BorderColor(hairline(c.Theme())).Children(titles)
}

// activityColumnTitle is the title of a column w DIPs wide, or of the one
// taking the rest when w is 0; end puts it at the column's end, over
// numbers.
func activityColumnTitle(c *ui.Context, s string, w float32, end bool) {
	e := ui.Text(c, s).FontSize(12).FontWeight(500).TextColor(c.Theme().TextMuted).SingleLine()
	if end {
		e.TextAlign(ui.End)
	}
	if w > 0 {
		e.Width(w).Shrink(0)
	} else {
		e.Grow(1).MinWidth(0)
	}
}

func (a *App) connectionsTable(c *ui.Context) {
	t := c.Theme()
	conns := a.shownConns()
	// Newest first: at the top, the list stays there as connections come.
	a.activity.conns.KeepOffset = true
	a.activity.conns.Key = func(i int) any {
		if i < len(conns) {
			return conns[i].ID
		}
		return i
	}
	now := a.Now()
	activityTable(c, func() {
		activityTableHead(c, func() {
			activityColumnTitle(c, locales.Time(), colTime, false)
			activityColumnTitle(c, locales.App(), colApp, false)
			activityColumnTitle(c, locales.Destination(), 0, false)
			activityColumnTitle(c, locales.Route(), colRoute, false)
			activityColumnTitle(c, locales.Traffic(), colBytes, true)
		})
		ui.List(c, &a.activity.conns, len(conns), func(i int) {
			a.connRow(c, conns[i], now)
		}).Shrink(1).MinHeight(0).Dividers(1, hairline(t)).Label(locales.Connections()).Children(func() {
			if len(conns) == 0 {
				text := locales.NoConnectionsYet()
				if a.activity.query != "" {
					text = locales.NoConnectionMatches()
				}
				muted(c, text).Padding(28).Margin(0, ui.Auto)
			}
		})
	})
}

func (a *App) connRow(c *ui.Context, cn *api.Connection, now time.Time) {
	t := c.Theme()
	row := ui.Row(c).Gap(12).Padding(9, tablePadX)
	if row.Hovered() {
		row.Background(subtle(t, 0.03))
	}
	open := cn.Ended.IsZero()
	row.Children(func() {
		ui.Text(c, clock(cn.Started, now)).FontSize(12).FontFeatures("tnum").TextColor(t.TextMuted).Width(colTime).Shrink(0).SingleLine()
		ui.Row(c).Gap(8).Width(colApp).Shrink(0).Children(func() {
			a.appIconView(c, cn.App, 20)
			ui.Text(c, appName(cn.App)).SingleLine().Shrink(1)
		})
		ui.Column(c).Grow(1).MinWidth(0).Gap(2).Children(func() {
			ui.Row(c).Gap(6).Children(func() {
				host := cn.Domain
				if host == "" {
					host = cn.Remote
				}
				ui.Text(c, host).FontWeight(500).SingleLine().Shrink(1)
				if open {
					dot(c, running(t), 6, false).Tooltip(locales.ConnectionOpen())
				}
			})
			detail := strings.ToUpper(string(cn.Protocol))
			if cn.Domain != "" {
				detail += " · " + cn.Remote
			} else if _, port, ok := strings.Cut(cn.Remote, "]:"); ok {
				detail += " · " + port
			}
			ui.Text(c, detail).FontSize(11).TextColor(t.TextMuted).SingleLine()
		})
		ui.Row(c).Width(colRoute).Shrink(0).Gap(6).Children(func() { a.outcomeView(c, cn) })
		ui.Column(c).Width(colBytes).Shrink(0).Gap(2).AlignItems(ui.End).Children(func() {
			ui.Text(c, "↓ "+byteSize(cn.Received)).FontSize(12).FontFeatures("tnum").SingleLine()
			ui.Text(c, "↑ "+byteSize(cn.Sent)).FontSize(11).FontFeatures("tnum").TextColor(t.TextMuted).SingleLine()
		})
	})
	row.Tooltip(a.connTooltip(cn))
	row.ContextMenu(func(m *ui.Menu) {
		if m.Item(locales.NewRuleForApp(appName(cn.App))).Disabled(cn.App.Path == "").Chosen() {
			a.ruleFromConnection(cn, false)
		}
		if cn.Domain != "" || cn.Remote != "" {
			label := locales.NewRuleForThisDestination()
			if m.Item(label).Chosen() {
				a.ruleFromConnection(cn, true)
			}
		}
		m.Separator()
		if m.Item(locales.ShowRule()).Disabled(cn.RuleID == "").Chosen() {
			a.showRule(cn.RuleID)
		}
		if m.Item(locales.ShowProgramInFinder()).Disabled(cn.Process.Path == "").Chosen() {
			a.Desktop.Reveal(cn.Process.Path)
		}
	})
}

// outcomeView shows where a connection went: the interface, and how it got
// there when that is not simply by its rule.
func (a *App) outcomeView(c *ui.Context, cn *api.Connection) {
	t := c.Theme()
	switch cn.Outcome {
	case api.Blocked:
		tagChip(c, icBlock, locales.Blocked(), t.Danger)
	case api.Unreachable:
		tagChip(c, icAlert, locales.Failed(), t.Warning).Tooltip(cn.Problem)
	case api.System:
		// macOS's own routing: a quiet chip, apart from the rules' colors.
		if cn.Interface != "" {
			a.systemChip(c, cn.Interface).Tooltip(locales.NoRuleMatchedTheSystemS())
		}
		if cn.RuleID != "" {
			ui.Text(c, locales.OutcomeFallback()).FontSize(11).TextColor(t.Warning).SingleLine().Tooltip(locales.NoneOfItsRuleSInterfaces())
		}
	default:
		if cn.Interface != "" {
			a.ifaceNameChip(c, cn.Interface)
		}
		if cn.Outcome == api.Fallback {
			ui.Text(c, locales.OutcomeBackup()).FontSize(11).TextColor(t.Warning).SingleLine().Tooltip(locales.ItsRuleSFirstInterfaceDidn())
		}
	}
	// Closed by a change of the rules, for its app to reconnect: not a failure.
	if cn.EndReason == api.EndRulesChanged {
		ui.Text(c, locales.EndRulesChanged()).FontSize(11).TextColor(t.TextMuted).SingleLine().Shrink(1).Tooltip(locales.ClosedToReconnect())
	}
}

// connTooltip tells all about a connection.
func (a *App) connTooltip(cn *api.Connection) string {
	var b strings.Builder
	b.WriteString(appName(cn.App))
	if cn.Process.Name != "" {
		b.WriteString(" — " + cn.Process.Name + " (PID " + strconv.Itoa(cn.Process.PID) + ")")
	}
	if cn.RuleID != "" {
		for i := range a.cfg.Scenes {
			sc := &a.cfg.Scenes[i]
			if k := ruleIndex(sc, cn.RuleID); k >= 0 {
				b.WriteString("\n" + locales.ConnRule(k+1, sceneName(sc), a.ruleTitle(&sc.Rules[k])))
			}
		}
	}
	if cn.Problem != "" {
		b.WriteString("\n" + cn.Problem)
	}
	if cn.EndReason == api.EndRulesChanged {
		b.WriteString("\n" + locales.ClosedToReconnect())
	}
	return b.String()
}

// ruleFromConnection opens the editor on a new rule for a connection's app
// or destination, in the scene the rules page shows.
func (a *App) ruleFromConnection(cn *api.Connection, destination bool) {
	sceneID := a.sceneID
	a.openEditor(sceneID, -1)
	e := a.editor
	if destination {
		target := cn.Domain
		if target == "" {
			if ap, err := parseAddrPort(cn.Remote); err == nil {
				target = ap
			}
		}
		if t, err := api.ParseTarget(target); err == nil {
			e.targets = []string{t.String()} // the host seen, alone
		}
	} else {
		e.rule.Apps = []api.AppRef{cn.App}
	}
	a.page = "rules"
}

// showRule shows the rules page with the scene that holds a rule.
func (a *App) showRule(id string) {
	for i := range a.cfg.Scenes {
		if k := ruleIndex(&a.cfg.Scenes[i], id); k >= 0 {
			a.sceneID = a.cfg.Scenes[i].ID
			a.page = "rules"
			a.openEditor(a.sceneID, k)
			return
		}
	}
}

func parseAddrPort(s string) (string, error) {
	if i := strings.LastIndex(s, ":"); i > 0 {
		return strings.Trim(s[:i], "[]"), nil
	}
	return s, nil
}

func (a *App) clearConnections() {
	a.activity.frozen = nil
	a.async(func(ctx context.Context) error { return a.Service.ClearConnections(ctx) }, func() {
		kept := a.conns[:0]
		for _, cn := range a.conns {
			if cn.Ended.IsZero() {
				kept = append(kept, cn)
			} else {
				delete(a.byID, cn.ID)
			}
		}
		a.conns = kept
	}, nil)
}

// appTotals sums the connections the interface holds, by app.
type appTotal struct {
	app        api.AppRef
	sent, recv uint64
	count      int
	open       int
	ifaces     []string
}

func (a *App) appTotals() []appTotal {
	byPath := map[string]*appTotal{}
	var order []*appTotal
	q := strings.ToLower(strings.TrimSpace(a.activity.query))
	for _, cn := range a.conns {
		if q != "" && !strings.Contains(strings.ToLower(appName(cn.App)), q) {
			continue
		}
		tot := byPath[cn.App.Path]
		if tot == nil {
			tot = &appTotal{app: cn.App}
			byPath[cn.App.Path] = tot
			order = append(order, tot)
		}
		tot.sent += cn.Sent
		tot.recv += cn.Received
		tot.count++
		if cn.Ended.IsZero() {
			tot.open++
		}
		if cn.Interface != "" && !slices.Contains(tot.ifaces, cn.Interface) {
			tot.ifaces = append(tot.ifaces, cn.Interface)
		}
	}
	out := make([]appTotal, len(order))
	for i, tot := range order {
		out[i] = *tot
	}
	slices.SortStableFunc(out, func(x, y appTotal) int {
		return int(int64(y.sent+y.recv) - int64(x.sent+x.recv))
	})
	return out
}

// Column widths of the apps table.
const (
	activityColIfaces  = 220
	activityColConns   = 80
	activityColTraffic = 200
)

func (a *App) appsTable(c *ui.Context) {
	t := c.Theme()
	totals := a.appTotals()
	var peak uint64 = 1
	for _, tot := range totals {
		peak = max(peak, tot.sent+tot.recv)
	}
	activityTable(c, func() {
		activityTableHead(c, func() {
			activityColumnTitle(c, locales.App(), 0, false)
			activityColumnTitle(c, locales.Interfaces(), activityColIfaces, false)
			activityColumnTitle(c, locales.Connections(), activityColConns, false)
			activityColumnTitle(c, locales.Traffic(), activityColTraffic, false)
		})
		ui.List(c, &a.activity.apps, len(totals), func(i int) {
			tot := totals[i]
			row := ui.Row(c.Key(tot.app.Path)).Gap(12).Padding(11, tablePadX)
			if row.Hovered() {
				row.Background(subtle(t, 0.03))
			}
			row.Children(func() {
				ui.Row(c).Gap(10).Grow(1).MinWidth(0).Children(func() {
					a.appIconView(c, tot.app, 26)
					ui.Text(c, appName(tot.app)).FontWeight(500).SingleLine().Shrink(1)
					if tot.open > 0 {
						ui.Row(c).Gap(5).Shrink(0).Children(func() {
							dot(c, running(t), 6, false)
							ui.Text(c, locales.AppOpenCount(tot.open)).FontSize(11).TextColor(t.TextMuted).SingleLine()
						})
					}
				})
				ui.Row(c).Width(activityColIfaces).Shrink(0).Gap(6).Children(func() {
					for _, name := range tot.ifaces[:min(len(tot.ifaces), 2)] {
						a.ifaceNameChip(c, name)
					}
				})
				ui.Text(c, strconv.Itoa(tot.count)).Width(activityColConns).Shrink(0).FontFeatures("tnum").FontSize(12)
				ui.Column(c).Width(activityColTraffic).Shrink(0).Gap(6).Children(func() {
					ui.Row(c).Gap(10).Children(func() {
						ui.Text(c, "↓ "+byteSize(tot.recv)).FontSize(12).FontFeatures("tnum").SingleLine()
						ui.Text(c, "↑ "+byteSize(tot.sent)).FontSize(12).FontFeatures("tnum").TextColor(t.TextMuted).SingleLine()
					})
					activityShareBar(c, float32(tot.sent+tot.recv)/float32(peak))
				})
			})
			row.ContextMenu(func(m *ui.Menu) {
				if m.Item(locales.NewRuleForApp(appName(tot.app))).Disabled(tot.app.Path == "").Chosen() {
					a.openEditor(a.sceneID, -1)
					a.editor.rule.Apps = []api.AppRef{tot.app}
					a.page = "rules"
				}
			})
		}).Shrink(1).MinHeight(0).Dividers(1, hairline(t)).Label(locales.Apps()).Children(func() {
			if len(totals) == 0 {
				muted(c, locales.NoAppsYet()).Padding(28).Margin(0, ui.Auto)
			}
		})
	})
}

// activityShareBar shows a share, from 0 to 1, as a soft bar of the
// accent, brightening toward its end, on a faint track.
func activityShareBar(c *ui.Context, share float32) {
	t := c.Theme()
	fill := ui.LinearGradient{From: t.Accent.Alpha(0.35), To: t.Accent, Angle: 90, Oklab: true}
	ui.Box(c).FillWidth().Height(5).Radius(999).Background(subtle(t, 0.06)).Children(func() {
		ui.Box(c).WidthPercent(max(2, share*100)).Height(5).Radius(999).LinearGradient(fill)
	})
}

// activityTools are the header's tools for connections and apps: the
// search, a button that pauses the list, and one that clears it.
func (a *App) activityTools(c *ui.Context) {
	p := &a.activity
	activitySearch(c, &p.query)
	ic, label := icPause, locales.Pause()
	if p.paused {
		ic, label = icPlay, locales.Resume()
	}
	// Buttons the keyboard reaches, as the theme's are.
	if glassCapsuleButton(c, "activity-pause", ic, "").Label(label).Tooltip(label).Focusable().Clicked() {
		p.paused = !p.paused
		p.frozen = nil
		if p.paused {
			p.frozen = slices.Clone(a.conns)
		}
	}
	empty := len(a.conns) == 0
	clear := glassCapsuleButton(c, "activity-clear", icEraser, "").Label(locales.Clear()).Tooltip(locales.Clear()).Focusable().Disabled(empty)
	if empty {
		clear.Opacity(0.4)
	}
	if clear.Clicked() {
		a.clearConnections()
	}
}
