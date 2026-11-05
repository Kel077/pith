package ui

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"github.com/egoist/mygo/ui"

	"github.com/Kel077/pith/internal/api"
	"github.com/Kel077/pith/locales"
)

// hotspotPoll is how often a hotspot is read while a view shows it.
const hotspotPoll = 10 * time.Second

// hotspots is what the window knows of the mobile hotspots behind the
// interfaces.
type hotspots struct {
	info     map[string]api.Hotspot // by interface
	none     map[string]bool        // interfaces that lead to no hotspot Pith knows
	renewing map[string]bool
	renewals map[string]api.HotspotRenewal
	unlock   unlockDialog

	seen  atomic.Int64             // when a view last showed hotspots: Unix nanoseconds
	names atomic.Pointer[[]string] // the interfaces to read, for the poller
	kick  chan struct{}            // read them now
}

type unlockDialog struct {
	open     bool
	iface    string
	password string
	wrong    bool
	busy     bool
}

func newHotspots() hotspots {
	return hotspots{info: map[string]api.Hotspot{}, none: map[string]bool{}, renewing: map[string]bool{},
		renewals: map[string]api.HotspotRenewal{}, kick: make(chan struct{}, 1)}
}

// watchHotspots tells the poller a view shows hotspots, and has it read
// them at once when none did a moment ago.
func (a *App) watchHotspots() {
	prev := a.hotspot.seen.Swap(time.Now().UnixNano())
	if time.Since(time.Unix(0, prev)) > 2*hotspotPoll {
		select {
		case a.hotspot.kick <- struct{}{}:
		default:
		}
	}
}

// pollHotspots reads the hotspots every hotspotPoll while a view shows
// them; never otherwise.
func (a *App) pollHotspots(ctx context.Context) {
	t := time.NewTicker(hotspotPoll)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		case <-a.hotspot.kick:
		}
		if time.Since(time.Unix(0, a.hotspot.seen.Load())) > 2*hotspotPoll {
			continue
		}
		if names := a.hotspot.names.Load(); names != nil {
			for _, name := range *names {
				h, err := a.Service.Hotspot(ctx, name)
				a.Post(func() { a.setHotspot(name, h, err) })
			}
		}
	}
}

// readHotspots reads them at once, for tests.
func (a *App) readHotspots(ctx context.Context) {
	if names := a.hotspot.names.Load(); names != nil {
		for _, name := range *names {
			h, err := a.Service.Hotspot(ctx, name)
			a.setHotspot(name, h, err)
		}
	}
}

func (a *App) setHotspot(name string, h api.Hotspot, err error) {
	switch {
	case api.Code(err) == api.ErrUnsupported:
		a.hotspot.none[name] = true
		delete(a.hotspot.info, name)
		a.hotspotNames()
	case err == nil:
		a.hotspot.info[name] = h
	}
	// Otherwise keep what was read last: a hotspot that reconnects doesn't
	// answer for a moment.
}

// hotspotNames tells the poller which interfaces may lead to hotspots: the
// connected cellular ones, but those found to lead to none.
func (a *App) hotspotNames() {
	var names []string
	for _, i := range a.ifaces {
		if i.Up && i.Kind == api.KindCellular && !a.hotspot.none[i.Name] {
			names = append(names, i.Name)
		} else if !i.Up {
			delete(a.hotspot.info, i.Name)
		}
	}
	a.hotspot.names.Store(&names)
}

// renewHotspot has the hotspot behind an interface reconnect to its
// carrier, for new public addresses.
func (a *App) renewHotspot(name string) {
	if a.hotspot.renewing[name] {
		return
	}
	a.hotspot.renewing[name] = true
	delete(a.hotspot.renewals, name)
	var r api.HotspotRenewal
	a.async(func(ctx context.Context) (err error) {
		ctx, cancel := context.WithTimeout(ctx, 3*time.Minute)
		defer cancel()
		r, err = a.Service.RenewHotspot(ctx, name)
		return err
	}, func() {
		delete(a.hotspot.renewing, name)
		a.hotspot.renewals[name] = r
		a.probes[name] = r.After
		select { // read its new state
		case a.hotspot.kick <- struct{}{}:
		default:
		}
	}, func(error) { delete(a.hotspot.renewing, name) })
}

// hotspotStrip shows a hotspot's signal and cell on its interface's card:
// the bars, the technology and the band, the carrier, and how strong and
// clean the signal is. A lock asks for its admin password when Pith has
// none.
func (a *App) hotspotStrip(c *ui.Context, h api.Hotspot, col ui.Color) {
	t := c.Theme()
	ui.Row(c.Key("hotspot-"+h.Interface)).Gap(10).Padding(10, 12).Radius(12).Background(subtle(t, 0.03)).AlignItems(ui.Center).Children(func() {
		signalBars(c, h.Bars, col, h.Connected)
		if h.Technology != "" {
			ui.Text(c, h.Technology).FontSize(14).FontWeight(650).SingleLine().Shrink(0)
		}
		if cell := h.Cell; cell != nil {
			if cell.Band != "" {
				chip := tagChip(c, nil, cell.Band, col)
				if f := bandFrequency(cell.Band); f != "" {
					chip.Tooltip(f)
				}
			}
			if cell.Operator != "" {
				ui.Text(c, cell.Operator).FontSize(12).TextColor(t.TextMuted).SingleLine().Shrink(1)
			}
			if cell.Roaming {
				tagChip(c, nil, locales.Roaming(), t.Warning)
			}
		}
		ui.Spacer(c)
		switch {
		case h.Locked:
			lock := glassCapsuleButton(c, "unlock", icLock, "").Label(locales.HotspotPassword()).Shrink(0).Tooltip(locales.HotspotPassword())
			if lock.Clicked() {
				a.hotspot.unlock = unlockDialog{open: true, iface: h.Interface}
			}
		case h.Cell != nil:
			ui.Text(c, fmt.Sprintf("%d dBm · %d dB", h.Cell.Signal, h.Cell.SNR)).FontSize(12).FontFeatures("tnum").
				TextColor(t.TextMuted).SingleLine().Shrink(0).Tooltip(locales.SignalAndSnr())
		}
	})
}

// renewalText tells what changing a hotspot's IP did: its public IP before
// and after.
func renewalText(r api.HotspotRenewal) string {
	ip := func(p api.Probe) string { return cmpOr(p.PublicIPv4, p.PublicIPv6) }
	before, after := ip(r.Before), ip(r.After)
	switch {
	case after == "":
		return ""
	case before == after:
		return locales.IpUnchanged(after)
	case before == "":
		return after
	}
	return before + " → " + after
}

// signalBars draws a signal of 0 to 5 bars, rising left to right, in col;
// the bars not lit, and all of them when not connected, faint.
func signalBars(c *ui.Context, bars int, col ui.Color, connected bool) {
	t := c.Theme()
	ui.Row(c).Gap(2).AlignItems(ui.End).Height(16).Shrink(0).Label(strconv.Itoa(bars) + "/5").Children(func() {
		for i := range 5 {
			fill := subtle(t, 0.14)
			if connected && i < bars {
				fill = col
			}
			ui.Box(c).Size(3.5, 5+float32(i)*2.75).Radius(1.25).Background(fill)
		}
	})
}

// bandFrequency is the frequency of a cellular band, by its name as
// hotspots show it ("n41", "B66", "LTE B2"), or "" when unknown.
func bandFrequency(band string) string {
	b := strings.ToLower(strings.TrimSpace(band))
	b = strings.TrimPrefix(b, "lte")
	b = strings.TrimSpace(b)
	nr := strings.HasPrefix(b, "n")
	n, err := strconv.Atoi(strings.TrimLeft(b, "nb"))
	if err != nil {
		return ""
	}
	if nr {
		switch n {
		case 77:
			return "3.7 GHz"
		case 78:
			return "3.5 GHz"
		case 79:
			return "4.7 GHz"
		case 258:
			return "26 GHz"
		case 260:
			return "39 GHz"
		case 261:
			return "28 GHz"
		}
	}
	return bandsMHz[n]
}

// bandsMHz names the frequencies of the bands LTE and 5G NR share numbers
// for.
var bandsMHz = map[int]string{
	1: "2100 MHz", 2: "1900 MHz", 3: "1800 MHz", 4: "1700/2100 MHz", 5: "850 MHz", 7: "2600 MHz", 8: "900 MHz",
	12: "700 MHz", 13: "700 MHz", 14: "700 MHz", 17: "700 MHz", 20: "800 MHz", 25: "1900 MHz", 26: "850 MHz",
	28: "700 MHz", 29: "700 MHz", 30: "2300 MHz", 38: "2600 MHz", 39: "1900 MHz", 40: "2300 MHz", 41: "2.5 GHz",
	46: "5 GHz", 48: "3.5 GHz", 66: "1700/2100 MHz", 70: "1700/2000 MHz", 71: "600 MHz",
}

// unlockView asks for a hotspot's admin password, and keeps it once the
// hotspot takes it.
func (a *App) unlockView(c *ui.Context) {
	d := &a.hotspot.unlock
	t := c.Theme()
	sheet(c, &d.open, 360, func() {
		sheetTitle(c, locales.HotspotPassword())
		in := sheetInput(c, ui.TextInput(c.Key("hotspot-password"), &d.password)).Password().AutoFocus().Margin(14, 0, 0)
		if in.Changed() {
			d.wrong = false
		}
		submit := in.Submitted()
		sheetFooter(c, func() {
			if d.wrong {
				ui.Text(c, locales.WrongPassword()).FontSize(12).TextColor(t.Danger).SingleLine()
			}
		}, func() {
			if sheetButton(c, "cancel", locales.Cancel()).Clicked() {
				d.open = false
			}
			ok := sheetPrimary(c, "save", locales.Save(), d.password == "" || d.busy).Clicked()
			if (ok || submit) && d.password != "" && !d.busy {
				d.busy = true
				name, password := d.iface, d.password
				wrong := false
				a.async(func(ctx context.Context) error {
					// A refused password is said in the sheet, not in a toast.
					err := a.Service.SetHotspotPassword(ctx, name, password)
					wrong = api.Code(err) == api.ErrWrongPassword
					if wrong {
						return nil
					}
					return err
				}, func() {
					if wrong {
						d.busy, d.wrong = false, true
						return
					}
					*d = unlockDialog{}
					select {
					case a.hotspot.kick <- struct{}{}:
					default:
					}
				}, func(error) { d.busy = false })
			}
		})
	})
}
