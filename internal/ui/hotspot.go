package ui

import (
	"context"
	_ "embed"
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
	delete(a.speeds, name) // of its old connection
	var r api.HotspotRenewal
	a.async(func(ctx context.Context) (err error) {
		ctx, cancel := context.WithTimeout(ctx, 3*time.Minute)
		defer cancel()
		r, err = a.Service.RenewHotspot(ctx, name)
		return err
	}, func() {
		delete(a.hotspot.renewing, name)
		a.hotspot.renewals[name] = r
		select { // read its new state
		case a.hotspot.kick <- struct{}{}:
		default:
		}
	}, func(error) { delete(a.hotspot.renewing, name) })
}

// The picture of a hotspot: its enclosure and a blank screen, without
// marks, on which hotspotDevice draws what the hotspot reports.
//
//go:embed assets/m2000.svg
var hotspotSVG []byte

var hotspotPicture = ui.MustParseSVG(hotspotSVG)

// Where the screen is in the picture, in percent of it, and the picture's
// width over its height.
const (
	screenLeft, screenTop, screenWidth, screenHeight = 33.89, 25.0, 32.22, 49.43
	hotspotAspect                                    = 1440.0 / 704
)

// The colors of the screen, which stays white in the dark as it does on the
// device.
var (
	screenInk   = ui.Hex("#15171a")
	screenFaint = ui.Hex("#7b7f86")
	screenRule  = ui.RGBA(20, 22, 26, 0.12)
)

// hotspotDevice shows a hotspot as itself, whose screen tells what it
// reports: the bars and 5G or LTE, the carrier, the band large with its
// frequency, and the signal's strength and SNR. Locked, the screen shows a
// lock that asks for its admin password.
func (a *App) hotspotDevice(c *ui.Context, h api.Hotspot, col ui.Color) {
	ui.Row(c.Key("hotspot-" + h.Interface)).Justify(ui.Center).Children(func() {
		ui.Box(c).WidthPercent(100).MaxWidth(400).AspectRatio(hotspotAspect).Children(func() {
			ui.Image(c, hotspotPicture).Absolute().Left(0).Top(0).WidthPercent(100).HeightPercent(100).Label(h.Model)
			screen := ui.Column(c).Absolute().LeftPercent(screenLeft).TopPercent(screenTop).WidthPercent(screenWidth).HeightPercent(screenHeight).
				Padding(6, 8, 7).Clip()
			if h.Locked {
				screen.Cursor(ui.CursorPointer).Label(locales.HotspotPassword()).Tooltip(locales.HotspotPassword())
				if screen.Clicked() {
					a.hotspot.unlock = unlockDialog{open: true, iface: h.Interface}
				}
			}
			screen.Children(func() { hotspotScreen(c, h, col) })
		})
	})
}

func hotspotScreen(c *ui.Context, h api.Hotspot, col ui.Color) {
	cell := h.Cell
	// The status bar, as the device has it.
	ui.Row(c).Gap(5).AlignItems(ui.Center).Padding(0, 0, 4).Children(func() {
		signalBars(c, h.Bars, col, h.Connected, 11)
		if h.Technology != "" {
			ui.Text(c, h.Technology).FontSize(10.5).FontWeight(700).TextColor(screenInk).SingleLine().Shrink(0)
		}
		ui.Spacer(c)
		if cell != nil && cell.Operator != "" {
			ui.Text(c, cell.Operator).FontSize(9).FontWeight(500).TextColor(screenFaint).SingleLine().Shrink(1)
		}
	})
	ui.Box(c).Height(1).Background(screenRule)
	ui.Column(c).Grow(1).Justify(ui.Center).AlignItems(ui.Center).Gap(1).Children(func() {
		switch {
		case h.Locked:
			ui.Icon(c, icLock).FontSize(20).TextColor(screenFaint)
		case !h.Connected:
			ui.Text(c, locales.NotConnected()).FontSize(11).TextColor(screenFaint).SingleLine()
		case cell != nil && cell.Band != "":
			ui.Text(c, cell.Band).FontSize(25).FontWeight(700).LetterSpacing(-0.6).TextColor(screenInk).SingleLine()
			if f := bandFrequency(cell.Band); f != "" {
				ui.Text(c, f).FontSize(9).TextColor(screenFaint).SingleLine()
			}
			if cell.Roaming {
				ui.Text(c, locales.Roaming()).FontSize(9).FontWeight(600).TextColor(ui.Hex("#c77700")).SingleLine()
			}
		}
	})
	if cell != nil && !h.Locked {
		ui.Row(c).Children(func() {
			ui.Text(c, fmt.Sprintf("%d dBm", cell.Signal)).FontSize(9).FontFeatures("tnum").TextColor(screenFaint).SingleLine()
			ui.Spacer(c)
			ui.Text(c, fmt.Sprintf("SNR %d dB", cell.SNR)).FontSize(9).FontFeatures("tnum").TextColor(screenFaint).SingleLine()
		}).Tooltip(locales.SignalAndSnr())
	}
}

// renewalText tells what changing a hotspot's IP did: its public IPv4 before
// and after, or its IPv6 when it has no IPv4 after.
func renewalText(r api.HotspotRenewal) string {
	before, after := r.Before.PublicIPv4, r.After.PublicIPv4
	if after == "" {
		before, after = r.Before.PublicIPv6, r.After.PublicIPv6
	}
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

// signalBars draws a signal of 0 to 5 bars, rising left to right to
// height, in col; the bars not lit, and all of them when not connected,
// faint.
func signalBars(c *ui.Context, bars int, col ui.Color, connected bool, height float32) {
	unit := height / 16
	ui.Row(c).Gap(2 * unit).AlignItems(ui.End).Height(height).Shrink(0).Label(strconv.Itoa(bars) + "/5").Children(func() {
		for i := range 5 {
			fill := screenRule
			if connected && i < bars {
				fill = col
			}
			ui.Box(c).Size(3.5*unit, (5+float32(i)*2.75)*unit).Radius(1.25 * unit).Background(fill)
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
