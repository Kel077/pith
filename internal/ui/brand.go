package ui

import (
	"bytes"
	"image"
	"image/png"

	"github.com/egoist/mygo/ui"
)

// appIconSVG is Pith's app icon: a slice of tangerine on the macOS icon
// grid, its five segments around a pale core.
var appIconSVG = ui.MustParseSVG([]byte(`<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 1024 1024">
<defs>
<linearGradient id="bg" x1="0" y1="0" x2="0" y2="1"><stop offset="0" stop-color="#ffa44d"/><stop offset="1" stop-color="#ec6512"/></linearGradient>
<linearGradient id="flesh" x1="0" y1="0" x2="0" y2="1"><stop offset="0" stop-color="#ffd596"/><stop offset="1" stop-color="#ffb359"/></linearGradient>
</defs>
<rect x="100" y="100" width="824" height="824" rx="186" fill="url(#bg)"/>
<circle cx="512" cy="512" r="300" fill="#fff7ec"/>
<circle cx="512" cy="512" r="262" fill="url(#flesh)"/>
<path d="M512 512L512 250M512 512L761.2 431M512 512L666 724M512 512L358 724M512 512L262.8 431" stroke="#fff7ec" stroke-width="22" stroke-linecap="round"/>
<circle cx="512" cy="512" r="46" fill="#fff7ec"/>
</svg>`))

// AppIcon renders the app icon, size pixels square.
func AppIcon(size int) *image.RGBA {
	return ui.Render(func(c *ui.Context) {
		c.Root().Background(ui.Transparent)
		ui.Image(c, appIconSVG).Size(float32(size), float32(size))
	}, size, size, 1)
}

var trayIcons [2][]byte

// TrayIcon renders the menu bar icon as a template PNG, 18 points square at
// twice the resolution: the mark, faint while routing is off. Call it on
// the main thread.
func TrayIcon(on bool) []byte {
	k := 0
	if on {
		k = 1
	}
	if trayIcons[k] == nil {
		trayIcons[k] = renderTrayIcon(on)
	}
	return trayIcons[k]
}

func renderTrayIcon(on bool) []byte {
	alpha := float32(1)
	if !on {
		alpha = 0.5
	}
	img := ui.Render(func(c *ui.Context) {
		c.Root().Background(ui.Transparent)
		ui.Box(c).Fill().Center().Children(func() {
			ui.Icon(c, pithMark).FontSize(16).TextColor(ui.RGBA(0, 0, 0, alpha))
		})
	}, 18, 18, 2)
	var b bytes.Buffer
	png.Encode(&b, img)
	return b.Bytes()
}
