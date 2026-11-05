package ui

import (
	"context"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/egoist/mygo/ui"
	"github.com/Kel077/pith/internal/api"
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
	t := c.Theme()
	p := &a.activity
	pageHeader(c, tr("Activity", "活动"), func() {
		ui.Segmented(c, &p.view, tr("Connections", "连接"), tr("Apps", "应用"), tr("Traffic", "流量")).Label(tr("Show", "显示"))
		if p.view == 2 {
			return
		}
		ui.SearchField(c.Key("activity-search"), &p.query).Label(tr("Filter", "筛选")).Placeholder(tr("Filter", "筛选")).Width(200)
		ic, label := icPause, tr("Pause", "暂停")
		if p.paused {
			ic, label = icPlay, tr("Resume", "继续")
		}
		if iconButton(c, ic, label).Clicked() {
			p.paused = !p.paused
			p.frozen = nil
			if p.paused {
				p.frozen = slices.Clone(a.conns)
			}
		}
		if iconButton(c, icEraser, tr("Clear", "清空")).Disabled(len(a.conns) == 0).Clicked() {
			a.clearConnections()
		}
	})
	if p.view == 2 {
		a.trafficView(c)
		return
	}
	if !a.isRunning() {
		ui.Row(c).Margin(0, 24, 12).Padding(10, 14).Gap(10).Radius(10).Background(subtle(t, 0.05)).Shrink(0).Children(func() {
			ui.Icon(c, icInfo).TextColor(t.TextMuted)
			muted(c, tr("Routing is off. This page shows the connections Pith routes.", "分流没有打开。这里显示经过橘核分流的连接。")).Grow(1)
			if ui.PrimaryButton(c, tr("Turn On", "打开分流")).Disabled(a.busy != "" || a.status.State == api.Starting).Clicked() {
				a.start(a.cfg.Selected)
			}
		})
	} else {
		a.ifaceStrip(c)
	}
	if p.view == 1 {
		a.appsTable(c)
		return
	}
	a.connectionsTable(c)
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
	ui.Row(c).Padding(0, 24, 12).Gap(10).Shrink(0).Children(func() {
		for _, name := range names {
			a.ifaceSpeedCard(c, name, series[name], now)
		}
	})
}

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
	ui.Column(c.Key("speed-"+name)).Grow(1).Basis(0).MinWidth(180).Padding(12, 14).Gap(6).Radius(12).Border(1, t.Border).Children(func() {
		ui.Row(c).Gap(8).Children(func() {
			dot(c, col, 8, false)
			ui.Text(c, a.ifaceLabel(api.InterfaceRef{Name: name})).FontWeight(600).SingleLine().Grow(1)
			ui.Text(c, "↓ "+rate(nowDown)).FontSize(12).Font("monospace").SingleLine()
			ui.Text(c, "↑ "+rate(nowUp)).FontSize(12).Font("monospace").TextColor(t.TextMuted).SingleLine()
		})
		sparkline(c, down[:], up[:], col).Height(36)
	})
}

// sparkline draws received bytes as a filled line, and sent bytes as a
// fainter line, on one scale.
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
		p.FillPath(area, col.Alpha(0.16))
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
	colTime   = 64
	colApp    = 170
	colRoute  = 190
	colBytes  = 132
	tablePadX = 24
)

func (a *App) connectionsTable(c *ui.Context) {
	t := c.Theme()
	conns := a.shownConns()
	ui.Row(c).Padding(6, tablePadX+12).Gap(12).Shrink(0).BorderWidth(0, 0, 1, 0).BorderColor(t.Border).Children(func() {
		for _, h := range []struct {
			s string
			w float32
		}{{tr("Time", "时间"), colTime}, {tr("App", "应用"), colApp}, {tr("Destination", "目标"), 0}, {tr("Route", "出口"), colRoute}, {tr("Traffic", "流量"), colBytes}} {
			e := ui.Text(c, h.s).FontSize(12).FontWeight(600).TextColor(t.TextMuted).SingleLine()
			if h.w == colBytes {
				e.TextAlign(ui.End)
			}
			if h.w > 0 {
				e.Width(h.w).Shrink(0)
			} else {
				e.Grow(1)
			}
		}
	})
	// Newest first: at the top, the list stays there as connections come.
	a.activity.conns.KeepOffset = true
	a.activity.conns.Key = func(i int) any {
		if i < len(conns) {
			return conns[i].ID
		}
		return i
	}
	now := a.Now()
	ui.List(c, &a.activity.conns, len(conns), func(i int) {
		a.connRow(c, conns[i], now)
	}).Grow(1).Padding(4, tablePadX, 12).Label(tr("Connections", "连接")).Children(func() {
		if len(conns) == 0 {
			text := tr("No connections yet.", "还没有连接。")
			if a.activity.query != "" {
				text = tr("No connection matches.", "没有匹配的连接。")
			}
			muted(c, text).Padding(24).Margin(0, ui.Auto)
		}
	})
}

func (a *App) connRow(c *ui.Context, cn *api.Connection, now time.Time) {
	t := c.Theme()
	row := ui.Row(c).Gap(12).Padding(7, 12).Radius(8)
	if row.Hovered() {
		row.Background(subtle(t, 0.04))
	}
	open := cn.Ended.IsZero()
	row.Children(func() {
		ui.Text(c, clock(cn.Started, now)).FontSize(12).Font("monospace").TextColor(t.TextMuted).Width(colTime).Shrink(0).SingleLine()
		ui.Row(c).Gap(8).Width(colApp).Shrink(0).Children(func() {
			a.appIconView(c, cn.App, 18)
			ui.Text(c, appName(cn.App)).SingleLine().Shrink(1)
		})
		ui.Column(c).Grow(1).MinWidth(0).Gap(1).Children(func() {
			ui.Row(c).Gap(6).Children(func() {
				host := cn.Domain
				if host == "" {
					host = cn.Remote
				}
				ui.Text(c, host).SingleLine().Shrink(1)
				if open {
					dot(c, running(t), 6, false).Tooltip(tr("Open", "进行中"))
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
		ui.Column(c).Width(colBytes).Shrink(0).Gap(1).AlignItems(ui.End).Children(func() {
			ui.Text(c, "↓ "+byteSize(cn.Received)).FontSize(12).Font("monospace").SingleLine()
			ui.Text(c, "↑ "+byteSize(cn.Sent)).FontSize(11).Font("monospace").TextColor(t.TextMuted).SingleLine()
		})
	})
	row.Tooltip(a.connTooltip(cn))
	row.ContextMenu(func(m *ui.Menu) {
		if m.Item(trf("New Rule for %s…", "为「%s」新建规则…", appName(cn.App))).Disabled(cn.App.Path == "").Chosen() {
			a.ruleFromConnection(cn, false)
		}
		if cn.Domain != "" || cn.Remote != "" {
			label := tr("New Rule for This Destination…", "为这个目标新建规则…")
			if m.Item(label).Chosen() {
				a.ruleFromConnection(cn, true)
			}
		}
		m.Separator()
		if m.Item(tr("Show Rule", "查看规则")).Disabled(cn.RuleID == "").Chosen() {
			a.showRule(cn.RuleID)
		}
		if m.Item(tr("Show Program in Finder", "在访达中显示程序")).Disabled(cn.Process.Path == "").Chosen() {
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
		tagChip(c, icBlock, tr("Blocked", "已拦截"), t.Danger)
	case api.Unreachable:
		tagChip(c, icAlert, tr("Failed", "连接失败"), t.Warning).Tooltip(cn.Problem)
	case api.System:
		// macOS's own routing: a quiet chip, apart from the rules' colors.
		if cn.Interface != "" {
			a.systemChip(c, cn.Interface).Tooltip(tr("No rule matched: the system's network", "没有命中规则，走系统默认网络"))
		}
		if cn.RuleID != "" {
			ui.Text(c, tr("fallback", "回退")).FontSize(11).TextColor(t.Warning).SingleLine().Tooltip(tr("None of its rule's interfaces connected", "规则里的网卡都没连上，回退到系统网络"))
		}
	default:
		if cn.Interface != "" {
			a.ifaceNameChip(c, cn.Interface)
		}
		if cn.Outcome == api.Fallback {
			ui.Text(c, tr("backup", "备选")).FontSize(11).TextColor(t.Warning).SingleLine().Tooltip(tr("Its rule's first interface didn't connect", "规则里的首选网卡没连上，用了备选网卡"))
		}
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
				if zh() {
					b.WriteString(fmt.Sprintf("\n「%s」第 %d 条规则：%s", sceneName(sc), k+1, a.ruleTitle(&sc.Rules[k])))
				} else {
					b.WriteString(fmt.Sprintf("\nRule %d of “%s”: %s", k+1, sceneName(sc), a.ruleTitle(&sc.Rules[k])))
				}
			}
		}
	}
	if cn.Problem != "" {
		b.WriteString("\n" + cn.Problem)
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

func (a *App) appsTable(c *ui.Context) {
	t := c.Theme()
	totals := a.appTotals()
	var peak uint64 = 1
	for _, tot := range totals {
		peak = max(peak, tot.sent+tot.recv)
	}
	ui.Row(c).Padding(6, tablePadX+12).Gap(12).Shrink(0).BorderWidth(0, 0, 1, 0).BorderColor(t.Border).Children(func() {
		ui.Text(c, tr("App", "应用")).FontSize(12).FontWeight(600).TextColor(t.TextMuted).Grow(1)
		ui.Text(c, tr("Interfaces", "网卡")).FontSize(12).FontWeight(600).TextColor(t.TextMuted).Width(220).Shrink(0)
		ui.Text(c, tr("Connections", "连接")).FontSize(12).FontWeight(600).TextColor(t.TextMuted).Width(80).Shrink(0)
		ui.Text(c, tr("Traffic", "流量")).FontSize(12).FontWeight(600).TextColor(t.TextMuted).Width(200).Shrink(0)
	})
	ui.List(c, &a.activity.apps, len(totals), func(i int) {
		tot := totals[i]
		row := ui.Row(c.Key(tot.app.Path)).Gap(12).Padding(9, 12).Radius(8)
		if row.Hovered() {
			row.Background(subtle(t, 0.04))
		}
		row.Children(func() {
			ui.Row(c).Gap(10).Grow(1).MinWidth(0).Children(func() {
				a.appIconView(c, tot.app, 24)
				ui.Text(c, appName(tot.app)).SingleLine().Shrink(1)
				if tot.open > 0 {
					ui.Text(c, trf("%d open", "%d 个进行中", tot.open)).FontSize(11).TextColor(running(t)).SingleLine()
				}
			})
			ui.Row(c).Width(220).Shrink(0).Gap(6).Children(func() {
				for _, name := range tot.ifaces[:min(len(tot.ifaces), 2)] {
					a.ifaceNameChip(c, name)
				}
			})
			ui.Text(c, strconv.Itoa(tot.count)).Width(80).Shrink(0).Font("monospace").FontSize(12)
			ui.Column(c).Width(200).Shrink(0).Gap(4).Children(func() {
				ui.Row(c).Gap(10).Children(func() {
					ui.Text(c, "↓ "+byteSize(tot.recv)).FontSize(12).Font("monospace").SingleLine()
					ui.Text(c, "↑ "+byteSize(tot.sent)).FontSize(12).Font("monospace").TextColor(t.TextMuted).SingleLine()
				})
				frac := float32(tot.sent+tot.recv) / float32(peak)
				ui.Box(c).FillWidth().Height(4).Radius(2).Background(subtle(t, 0.07)).Children(func() {
					ui.Box(c).WidthPercent(max(2, frac*100)).Height(4).Radius(2).Background(t.Accent)
				})
			})
		})
		row.ContextMenu(func(m *ui.Menu) {
			if m.Item(trf("New Rule for %s…", "为「%s」新建规则…", appName(tot.app))).Disabled(tot.app.Path == "").Chosen() {
				a.openEditor(a.sceneID, -1)
				a.editor.rule.Apps = []api.AppRef{tot.app}
				a.page = "rules"
			}
		})
	}).Grow(1).Padding(4, tablePadX, 12).Label(tr("Apps", "应用")).Children(func() {
		if len(totals) == 0 {
			muted(c, tr("No apps yet.", "还没有应用。")).Padding(24).Margin(0, ui.Auto)
		}
	})
}
