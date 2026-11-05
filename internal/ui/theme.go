package ui

import (
	"hash/fnv"
	"slices"
	"strings"

	"github.com/egoist/mygo/ui"

	"github.com/Kel077/pith/internal/api"
)

// Pith's look: the system's neutrals, light or dark, with one warm accent,
// the color of a tangerine, kept for what the user acts on and for routing
// that runs. Each interface has a color of its own, the same everywhere it
// shows: in rules, the activity and charts.

var (
	tangerine     = ui.Hex("#ee7a1e")
	tangerineDark = ui.Hex("#ff9a45")
	green         = ui.Hex("#22a559")
	greenDark     = ui.Hex("#34c46e")
)

// ifacePalette holds the interfaces' colors, calm and apart from the accent
// and from each other in both appearances.
var ifacePalette = []ui.Color{
	ui.Hex("#3b82f6"), // blue
	ui.Hex("#8b5cf6"), // violet
	ui.Hex("#14b8a6"), // teal
	ui.Hex("#ec4899"), // pink
	ui.Hex("#0ea5e9"), // sky
	ui.Hex("#a3a635"), // olive
}

// theme returns the window's theme: the system's, with Pith's accent.
func theme(c *ui.Context) *ui.Theme {
	t := *c.Theme()
	if t.Dark {
		t.Accent = tangerineDark
		t.AccentHover = ui.Hex("#ffab63")
		t.AccentPressed = ui.Hex("#f08a35")
		t.AccentText = ui.Hex("#1f1208")
		t.Selection = tangerineDark.Alpha(0.35)
		t.Focus = tangerineDark.Alpha(0.6)
		t.Success = greenDark
		t.Background = ui.Hex("#1c1c1e")
		t.Surface = ui.Hex("#2a2a2d")
		t.SurfaceHover = ui.Hex("#323236")
		t.Border = ui.Hex("#3a3a3e")
	} else {
		t.Accent = tangerine
		t.AccentHover = ui.Hex("#de6c12")
		t.AccentPressed = ui.Hex("#c95f0c")
		t.AccentText = ui.Hex("#ffffff")
		t.Selection = tangerine.Alpha(0.22)
		t.Focus = tangerine.Alpha(0.5)
		t.Success = green
		t.Surface = ui.Hex("#f5f5f4")
		t.SurfaceHover = ui.Hex("#ececea")
		t.Border = ui.Hex("#e2e2df")
	}
	t.Radius = 7
	return &t
}

// kindColor is the index in ifacePalette an interface of a kind prefers.
var kindColor = map[api.InterfaceKind]int{
	api.KindEthernet: 0, api.KindCellular: 1, api.KindWiFi: 2,
	api.KindVPN: 3, api.KindThunderbolt: 4, api.KindOther: 5,
}

// colorOf is the color of the interface with the key k (InterfaceRef.Key):
// its kind's, unless another interface took it, and stable while the
// interfaces stay.
func (a *App) colorOf(k string) ui.Color {
	if len(a.colors) == 0 || a.colorsFor != len(a.ifaces) {
		a.assignColors()
	}
	if col, ok := a.colors[k]; ok {
		return col
	}
	return ifaceColor(k)
}

// colorFor is the color of an interface a rule names.
func (a *App) colorFor(ref api.InterfaceRef) ui.Color {
	if i := a.iface(ref); i != nil {
		return a.colorOf(i.Name)
	}
	return ifaceColor(ref.Key())
}

func (a *App) assignColors() {
	a.colors = map[string]ui.Color{}
	a.colorsFor = len(a.ifaces)
	used := map[int]bool{}
	// Connected interfaces choose first, so that they get their kind's
	// color; then the rest, by name.
	sorted := slices.Clone(a.ifaces)
	slices.SortFunc(sorted, func(x, y api.Interface) int {
		if x.Up != y.Up {
			if x.Up {
				return -1
			}
			return 1
		}
		return strings.Compare(x.Name, y.Name)
	})
	for _, i := range sorted {
		k := kindColor[i.Kind]
		for n := 0; used[k] && n < len(ifacePalette); n++ {
			k = (k + 1) % len(ifacePalette)
		}
		used[k] = true
		a.colors[i.Name] = ifacePalette[k]
	}
}

// ifaceColor is a color for an interface Pith does not see now, by its key.
func ifaceColor(k string) ui.Color {
	h := fnv.New32a()
	h.Write([]byte(k))
	return ifacePalette[h.Sum32()%uint32(len(ifacePalette))]
}

// subtle is a color for quiet fills, as chips and hovered rows: the text
// color, faint.
func subtle(t *ui.Theme, a float32) ui.Color { return t.Text.Alpha(a) }

// running is the color of routing that runs.
func running(t *ui.Theme) ui.Color { return t.Success }
