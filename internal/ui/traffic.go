package ui

import (
	"fmt"
	"slices"
	"time"

	"github.com/egoist/mygo/ui"

	"github.com/Kel077/pith/internal/api"
	"github.com/Kel077/pith/locales"
)

// trafficSpan is a period the traffic chart shows: bars of a step, or of
// local calendar days for the week.
type trafficSpan struct {
	label func() string
	bars  int
	step  time.Duration // of the bars, and of the series asked for; days ask for hours
	days  bool          // bars are local days, which DST makes 23 or 25 hours
}

var trafficSpans = []trafficSpan{
	{locales.SpanHour, 60, time.Minute, false},
	{locales.SpanDay, 48, 30 * time.Minute, false},
	{locales.SpanWeek, 7, time.Hour, true},
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

// trafficView shows how much went through each interface over a period:
// the totals in small panes, and the bars of the period in a large one.
func (a *App) trafficView(c *ui.Context) {
	p := &a.activity
	span := trafficSpans[p.span]
	now := a.Now()
	key := fmt.Sprint(p.span, now.Truncate(30*time.Second).Unix())
	if p.historyKey != key && !p.historyBusy {
		a.loadHistory(span, now, key)
	}
	since := span.since(now)
	h := p.history
	ui.Scroll(c).Grow(1).Padding(0, 32, 24).Gap(14).Children(func() {
		ui.Row(c).Gap(8).Shrink(0).Children(func() {
			labels := make([]string, len(trafficSpans))
			for i, s := range trafficSpans {
				labels[i] = s.label()
			}
			segmentedCapsule(c, "traffic-span", locales.Period(), &p.span, labels, nil)
			ui.Spacer(c)
			if p.historyBusy && len(h.Series) == 0 {
				ui.Spinner(c)
			}
		})
		if p.historyErr != nil {
			activityEmptyPane(c, errorText(p.historyErr))
			return
		}
		totals := seriesTotals(h, span, since)
		if len(totals) == 0 {
			if !p.historyBusy {
				activityEmptyPane(c, locales.NoTrafficInThisPeriod())
			}
			return
		}
		ui.Row(c).Gap(12).Wrap().Children(func() {
			for _, it := range totals {
				a.trafficTotal(c, it)
			}
		})
		pane(c).Padding(16, 20, 14).Gap(10).Children(func() { a.trafficChart(c, h, span, since) })
	})
}

// activityEmptyPane takes the place of the totals and the chart: a pane
// with a short line, as the tables' when they have no rows.
func activityEmptyPane(c *ui.Context, s string) {
	pane(c).Padding(28, 24).Children(func() {
		muted(c, s).TextAlign(ui.Center).Margin(0, ui.Auto)
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

// trafficTotal is the total of one interface over the period, in a small
// pane; what it counts is its tooltip.
func (a *App) trafficTotal(c *ui.Context, it api.InterfaceTraffic) {
	t := c.Theme()
	tile := pane(c.Key("total-"+it.Interface)).Grow(1).Basis(0).MinWidth(200).Padding(14, 18, 16).Gap(8)
	tile.Tooltip(locales.OnlyWhatPithRoutedTheBytes())
	tile.Children(func() {
		ui.Row(c).Gap(8).Children(func() {
			dot(c, a.colorOf(it.Interface), 8, false)
			ui.Text(c, a.ifaceLabel(api.InterfaceRef{Name: it.Interface})).FontWeight(600).SingleLine().Shrink(1)
		})
		ui.Row(c).Gap(14).AlignItems(ui.Baseline).Children(func() {
			ui.Text(c, "↓ "+byteSize(it.Received)).FontSize(22).FontWeight(600).FontFeatures("tnum").LetterSpacing(-0.2).SingleLine()
			ui.Text(c, "↑ "+byteSize(it.Sent)).FontSize(13).FontFeatures("tnum").TextColor(t.TextMuted).SingleLine()
		})
	})
}

// trafficChart draws a bar a step, its interfaces stacked in their colors:
// rounded at the top, fading a little toward the baseline.
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
	grid := hairline(t)
	ui.Row(c).Gap(8).Children(func() {
		per := locales.PerDay()
		if !span.days {
			per = locales.ChartPer(stepText(span.step))
		}
		ui.Text(c, per).FontSize(12).FontWeight(500).TextColor(t.TextMuted)
		ui.Spacer(c)
		ui.Text(c, locales.ChartPeak(byteSize(peak))).FontSize(12).FontFeatures("tnum").TextColor(t.TextMuted)
	})
	ui.Box(c).FillWidth().Height(180).Draw(func(p *ui.Painter, r ui.Rect) {
		for k := 0; k <= 2; k++ {
			y := r.Y + r.H*float32(k)/2
			p.Line(r.X, y, r.X+r.W, y, 1, grid)
		}
		// Bars in the middle of their slots: slim ones for many steps, and
		// for the days of a week not wider than a finger.
		w := r.W / float32(span.bars)
		bw := min(w-min(w*0.3, 6), 44)
		radius := min(max(4, bw/8), bw/2)
		// The bottom of a bar is as solid as fade; its top, whole.
		const fade = 0.62
		for i, b := range bars {
			var sum uint64
			for _, v := range b {
				sum += v
			}
			if sum == 0 {
				continue
			}
			x := r.X + float32(i)*w + (w-bw)/2
			base := r.Y + r.H
			top := base - r.H*float32(sum)/float32(peak)
			high := max(base-top, 1)
			alpha := func(y float32) float32 { return 1 - (1-fade)*(y-top)/high }
			// Clipped to a box reaching below the baseline by its radius:
			// only the bar's top corners round.
			p.Clip(ui.Rect{X: x, Y: top, W: bw, H: base - top + radius}, radius, func() {
				y := base
				for _, name := range names {
					v := b[name]
					if v == 0 {
						continue
					}
					bh := r.H * float32(v) / float32(peak)
					col := colors[name]
					p.FillGradient(ui.Rect{X: x, Y: y - bh, W: bw, H: bh},
						ui.LinearGradient{From: col.Alpha(alpha(y - bh)), To: col.Alpha(alpha(y)), Angle: 180}, 0)
					y -= bh
				}
			})
		}
	})
	ui.Row(c).Children(func() {
		if span.days {
			// Each day under its bar.
			for k := range span.bars {
				ui.Text(c, timeLabel(span.barStart(since, k), span)).FontSize(11).FontFeatures("tnum").TextColor(t.TextMuted).
					Grow(1).Basis(0).TextAlign(ui.Center).SingleLine()
			}
			return
		}
		for k, bar := range []int{0, span.bars / 2, span.bars - 1} {
			e := ui.Text(c, timeLabel(span.barStart(since, bar), span)).FontSize(11).FontFeatures("tnum").TextColor(t.TextMuted).Grow(1)
			switch k {
			case 1:
				e.TextAlign(ui.Center)
			case 2:
				e.TextAlign(ui.End)
			}
		}
	})
	ui.Row(c).Gap(16).Margin(4, 0, 0).Children(func() {
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
		return locales.StepHours(int(d.Hours()))
	case d > time.Minute:
		return locales.StepMinutes(int(d.Minutes()))
	}
	return locales.StepMinute()
}

func timeLabel(at time.Time, span trafficSpan) string {
	if span.days {
		return at.Format("01-02")
	}
	return at.Format("15:04")
}
