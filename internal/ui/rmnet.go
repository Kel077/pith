package ui

import (
	"fmt"
	"strconv"
	"strings"

	"github.com/egoist/mygo/ui"

	"github.com/Kel077/pith/internal/api"
	"github.com/Kel077/pith/locales"
)

// The USB modem has no screen. Its radio measurements belong together in
// the interface card, above the Mac's addresses.
func rmnetDevice(c *ui.Context, h api.Hotspot, col ui.Color, label string) {
	t := c.Theme()
	ui.Column(c).Gap(6).Padding(10, 12).Radius(12).Background(col.Alpha(0.07)).Children(func() {
		ui.Row(c).Gap(7).AlignItems(ui.Center).Children(func() {
			signalBars(c, h.Bars, col, h.Connected, 15)
			switch {
			case h.At.IsZero():
				muted(c, locales.ModemStatusUnavailable())
			case !h.Connected:
				muted(c, locales.NotConnected())
			case h.Technology != "":
				ui.Text(c, h.Technology).FontSize(14).FontWeight(600).SingleLine()
			}
			if h.Cell != nil && h.Cell.Operator != "" {
				muted(c, h.Cell.Operator).SingleLine()
			}
			ui.Spacer(c)
			if label != h.Model {
				muted(c, h.Model).FontSize(12).SingleLine()
			}
		})
		cell := h.Cell
		if cell == nil || !h.Connected {
			return
		}
		bands, aggregated := rmnetBands(cell)
		if bands != "" {
			title := locales.CellularBands()
			if aggregated {
				title = locales.CarrierAggregation()
			}
			ui.Text(c, title).FontSize(11).TextColor(t.TextMuted)
			ui.Text(c, bands).FontSize(15).FontWeight(600).FontFeatures("tnum")
		}
		var signal []string
		if cell.RSRP != nil {
			signal = append(signal, fmt.Sprintf("RSRP %d dBm", *cell.RSRP))
		}
		if cell.RSRQ != nil {
			signal = append(signal, fmt.Sprintf("RSRQ %d dB", *cell.RSRQ))
		}
		if cell.SINR != nil {
			signal = append(signal, fmt.Sprintf("SINR %g dB", *cell.SINR))
		}
		if cell.Roaming {
			signal = append(signal, locales.Roaming())
		}
		if len(signal) != 0 {
			ui.Text(c, strings.Join(signal, " · ")).FontSize(12).FontFeatures("tnum").TextColor(t.TextMuted)
		}
	})
}

// Keep repeated bands. An LTE anchor and one NR carrier are dual
// connectivity; aggregation requires multiple components of the same RAT.
func rmnetBands(cell *api.HotspotCell) (string, bool) {
	var bands []string
	var nr, lte int
	for _, carrier := range cell.Carriers {
		if carrier.Band == "" {
			continue
		}
		band := carrier.Band
		if carrier.BandwidthMHz > 0 {
			band += fmt.Sprintf(" (%g MHz)", carrier.BandwidthMHz)
		}
		bands = append(bands, band)
		switch {
		case carrier.RAT == "NR5G", carrier.RAT == "" && strings.HasPrefix(carrier.Band, "n"):
			nr++
		case carrier.RAT == "LTE", carrier.RAT == "" && strings.HasPrefix(carrier.Band, "B"):
			lte++
		}
	}
	if len(bands) == 0 {
		return cell.Band, false
	}
	return strings.Join(bands, " + "), nr > 1 || lte > 1
}

func rmnetRenewal(c *ui.Context, r api.HotspotRenewal) {
	for _, f := range []struct{ family, before, after string }{
		{"IPv4", r.Before.PublicIPv4, r.After.PublicIPv4}, {"IPv6", r.Before.PublicIPv6, r.After.PublicIPv6},
	} {
		renewalLine(c, f.family+"  "+observedAddressChange(f.before, f.after)).FontSize(12)
	}
	muted(c, locales.HotspotRenewalTime(strconv.FormatFloat(r.Took.Seconds(), 'f', 1, 64))).FontSize(12)
}

func observedAddressChange(before, after string) string {
	switch {
	case before != "" && before == after:
		return locales.IpUnchanged(after)
	case before != "" && after != "":
		return before + " → " + after
	case before != "":
		return before + " → " + locales.AddressNotObserved()
	case after != "":
		return locales.AddressNotObserved() + " → " + after
	default:
		return locales.AddressNotObserved()
	}
}
