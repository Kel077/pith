package ui

import (
	"fmt"
	"slices"
	"time"

	"github.com/egoist/mygo/ui"
	"github.com/Kel077/pith/internal/api"
)

// trafficSpan is a period the traffic chart shows: bars of a step, or of
// local calendar days for the week.
type trafficSpan struct {
	en, zh string
	bars   int
	step   time.Duration // of the bars, and of the series asked for; days ask for hours
	days   bool          // bars are local days, which DST makes 23 or 25 hours
}

var trafficSpans = []trafficSpan{
	{"Last hour", "1 小时", 60, time.Minute, false},
	{"Last day", "24 小时", 48, 30 * time.Minute, false},
	{"Last week", "7 天", 7, time.Hour, true},
}

// since is where a span starts: the start of the first bar, so that bars
// line up with the clock, and with local midnight for days.
func (s trafficSpan) since(now time.Time) time.Time {
	if s.days {
		y, m, d := now.Date()
		return time.Date(y, m, d-s.bars+1, 0, 0, 0, 0, now.Location())
	}
	return now.Truncate(s.step).Add(-time.Duration(s.bars-1) * s.step)
}

// bar is the bar a sample falls in, or -1 outside the span.
func (s trafficSpan) bar(since, at time.Time) int {
	if at.Before(since) {
		return -1
	}
	var i int
	if s.days {
		at = at.In(since.Location())
		y, m, d := at.Date()
		day := time.Date(y, m, d, 0, 0, 0, 0, since.Location())
		// Whole days between two local midnights, in spite of DST.
		i = int((day.Sub(since) + 12*time.Hour) / (24 * time.Hour))
	} else {
		i = int(at.Sub(since) / s.step)
	}
	if i >= s.bars {
		return -1
	}
	return i
}

// barStart is when bar k starts.
func (s trafficSpan) barStart(since time.Time, k int) time.Time {
	if s.days {
		return since.AddDate(0, 0, k)
	}
	return since.Add(time.Duration(k) * s.step)
}

// trafficView shows how much went through each interface over a period.
func (a *App) trafficView(c *ui.Context) {
	t := c.Theme()
	p := &a.activity
	span := trafficSpans[p.span]
	now := a.Now()
	key := fmt.Sprint(p.span, now.Truncate(30*time.Second).Unix())
	if p.historyKey != key && !p.historyBusy {
		a.loadHistory(span, now, key)
	}
	since := span.since(now)
	h := p.history
	ui.Scroll(c).Grow(1).Padding(0, 24, 24).Gap(16).Children(func() {
		ui.Row(c).Gap(8).Children(func() {
			labels := make([]string, len(trafficSpans))
			for i, s := range trafficSpans {
				labels[i] = tr(s.en, s.zh)
			}
			ui.Segmented(c, &p.span, labels...).Label(tr("Period", "时段"))
			ui.Spacer(c)
			if p.historyBusy && len(h.Series) == 0 {
				ui.Spinner(c)
			}
		})
		if p.historyErr != nil {
			muted(c, errorText(p.historyErr))
			return
		}
		totals := seriesTotals(h, span, since)
		ui.Row(c).Gap(10).Wrap().Children(func() {
			for _, it := range totals {
				a.trafficTotal(c, it)
			}
			if len(totals) == 0 && !p.historyBusy {
				muted(c, tr("No traffic in this period.", "这段时间没有流量。"))
			}
		})
		if len(totals) > 0 {
			box := card(c).Padding(16, 16, 12)
			box.Children(func() { a.trafficChart(c, h, span, since) })
		}
		ui.Row(c).Gap(6).Children(func() {
			ui.Icon(c, icInfo).FontSize(12).TextColor(t.TextMuted)
			ui.Text(c, tr("Only what Pith routed: the bytes apps sent and received, without the network's own overhead.", "只统计经过橘核分流的流量：应用收发的数据，不含网络协议开销。")).FontSize(12).TextColor(t.TextMuted)
		})
	})
}

// seriesTotals sums the series in the span by interface, most bytes first,
// so that the totals are those of the bars.
func seriesTotals(h api.Traffic, span trafficSpan, since time.Time) []api.InterfaceTraffic {
	by := map[string]*api.InterfaceTraffic{}
	var out []*api.InterfaceTraffic
	for _, s := range h.Series {
		if span.bar(since, s.At) < 0 {
			continue
		}
		it := by[s.Interface]
		if it == nil {
			it = &api.InterfaceTraffic{Interface: s.Interface}
			by[s.Interface] = it
			out = append(out, it)
		}
		it.Sent += s.Sent
		it.Received += s.Received
	}
	slices.SortFunc(out, func(x, y *api.InterfaceTraffic) int {
		return int(int64(y.Sent+y.Received) - int64(x.Sent+x.Received))
	})
	totals := make([]api.InterfaceTraffic, len(out))
	for i, it := range out {
		totals[i] = *it
	}
	return totals
}

// loadHistory reads the series of a span. Without the helper there is no
// history: the page says so, rather than a toast every half minute.
func (a *App) loadHistory(span trafficSpan, now time.Time, key string) {
	p := &a.activity
	p.historyBusy = true
	q := api.TrafficQuery{Since: span.since(now), Step: span.step}
	if a.syncIcons {
		h, err := a.Service.Traffic(a.ctx, q)
		p.historyBusy, p.history, p.historyErr, p.historyKey = false, h, err, key
		return
	}
	go func() {
		h, err := a.Service.Traffic(a.ctx, q)
		a.Post(func() { p.historyBusy, p.history, p.historyErr, p.historyKey = false, h, err, key })
	}()
}

// trafficTotal is the total of one interface over the period.
func (a *App) trafficTotal(c *ui.Context, it api.InterfaceTraffic) {
	t := c.Theme()
	ui.Column(c.Key("total-"+it.Interface)).Padding(12, 14).Gap(6).Radius(12).Border(1, t.Border).MinWidth(200).Children(func() {
		ui.Row(c).Gap(8).Children(func() {
			dot(c, a.colorOf(it.Interface), 8, false)
			ui.Text(c, a.ifaceLabel(api.InterfaceRef{Name: it.Interface})).FontWeight(600).SingleLine()
		})
		ui.Row(c).Gap(14).AlignItems(ui.Baseline).Children(func() {
			ui.Text(c, "↓ "+byteSize(it.Received)).FontSize(18).FontWeight(600).Font("monospace")
			ui.Text(c, "↑ "+byteSize(it.Sent)).FontSize(13).Font("monospace").TextColor(t.TextMuted)
		})
	})
}

// trafficChart draws a bar a step, its interfaces stacked in their colors.
func (a *App) trafficChart(c *ui.Context, h api.Traffic, span trafficSpan, since time.Time) {
	t := c.Theme()
	bars := make([]map[string]uint64, span.bars)
	var names []string
	for _, s := range h.Series {
		i := span.bar(since, s.At)
		if i < 0 {
			continue
		}
		if bars[i] == nil {
			bars[i] = map[string]uint64{}
		}
		bars[i][s.Interface] += s.Sent + s.Received
		if !slices.Contains(names, s.Interface) {
			names = append(names, s.Interface)
		}
	}
	slices.Sort(names)
	var peak uint64 = 1
	for _, b := range bars {
		var sum uint64
		for _, v := range b {
			sum += v
		}
		peak = max(peak, sum)
	}
	colors := map[string]ui.Color{}
	for _, name := range names {
		colors[name] = a.colorOf(name)
	}
	grid := subtle(t, 0.07)
	ui.Row(c).Gap(8).Children(func() {
		per := tr("Per day", "每天")
		if !span.days {
			per = tr("Per ", "每") + stepText(span.step)
		}
		ui.Text(c, per).FontSize(12).TextColor(t.TextMuted)
		ui.Spacer(c)
		ui.Text(c, tr("Peak ", "峰值 ")+byteSize(peak)).FontSize(12).TextColor(t.TextMuted)
	})
	ui.Box(c).FillWidth().Height(180).Draw(func(p *ui.Painter, r ui.Rect) {
		for k := 0; k <= 2; k++ {
			y := r.Y + r.H*float32(k)/2
			p.Line(r.X, y, r.X+r.W, y, 1, grid)
		}
		w := r.W / float32(span.bars)
		gap := min(w*0.25, 3)
		for i, b := range bars {
			y := r.Y + r.H
			for _, name := range names {
				v := b[name]
				if v == 0 {
					continue
				}
				bh := r.H * float32(v) / float32(peak)
				p.Fill(ui.Rect{X: r.X + float32(i)*w + gap/2, Y: y - bh, W: w - gap, H: bh}, colors[name], min(2, (w-gap)/2))
				y -= bh
			}
		}
	})
	ui.Row(c).Children(func() {
		for k, bar := range []int{0, span.bars / 2, span.bars - 1} {
			e := ui.Text(c, timeLabel(span.barStart(since, bar), span)).FontSize(11).TextColor(t.TextMuted).Grow(1)
			switch k {
			case 1:
				e.TextAlign(ui.Center)
			case 2:
				e.TextAlign(ui.End)
			}
		}
	})
	ui.Row(c).Gap(14).Margin(4, 0, 0).Children(func() {
		for _, name := range names {
			ui.Row(c).Gap(6).Children(func() {
				dot(c, colors[name], 8, false)
				ui.Text(c, a.ifaceLabel(api.InterfaceRef{Name: name})).FontSize(12).TextColor(t.TextMuted)
			})
		}
	})
}

func stepText(d time.Duration) string {
	switch {
	case d >= time.Hour:
		return trf("%d hours", "%d 小时", int(d.Hours()))
	case d > time.Minute:
		return trf("%d minutes", "%d 分钟", int(d.Minutes()))
	}
	return tr("minute", "分钟")
}

func timeLabel(at time.Time, span trafficSpan) string {
	if span.days {
		return at.Format("01-02")
	}
	return at.Format("15:04")
}
