package ui

import "github.com/egoist/mygo/ui"

// icon makes an icon of 24-unit strokes, drawn in the color of the text.
func icon(shapes string) *ui.SVG {
	return ui.MustParseSVG([]byte(`<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="1.8" stroke-linecap="round" stroke-linejoin="round">` + shapes + `</svg>`))
}

// Icons, drawn for Pith on a 24-unit grid.
var (
	icRules    = icon(`<path d="M4 6h3M4 12h3M4 18h3"/><path d="M11 6h9M11 12h9M11 18h9"/>`)
	icActivity = icon(`<path d="M3 12h4l2.5-6 5 12 2.5-6h4"/>`)
	icNetwork  = icon(`<rect x="9" y="3" width="6" height="5" rx="1"/><rect x="3" y="16" width="6" height="5" rx="1"/><rect x="15" y="16" width="6" height="5" rx="1"/><path d="M12 8v4M6 16v-2h12v2"/>`)
	icSettings = icon(`<path d="M4 7h10M18 7h2M4 17h2M10 17h10"/><circle cx="16" cy="7" r="2"/><circle cx="8" cy="17" r="2"/>`)

	icPlus        = icon(`<path d="M12 5v14M5 12h14"/>`)
	icCheck       = icon(`<path d="M20 6 9 17l-5-5"/>`)
	icX           = icon(`<path d="M18 6 6 18M6 6l12 12"/>`)
	icMore        = icon(`<circle cx="5" cy="12" r="1.2"/><circle cx="12" cy="12" r="1.2"/><circle cx="19" cy="12" r="1.2"/>`)
	icGrip        = icon(`<circle cx="9" cy="6" r="1"/><circle cx="15" cy="6" r="1"/><circle cx="9" cy="12" r="1"/><circle cx="15" cy="12" r="1"/><circle cx="9" cy="18" r="1"/><circle cx="15" cy="18" r="1"/>`)
	icArrow       = icon(`<path d="M5 12h14M13 6l6 6-6 6"/>`)
	icChevron     = icon(`<path d="m9 6 6 6-6 6"/>`)
	icChevronUp   = icon(`<path d="m6 15 6-6 6 6"/>`)
	icChevronDown = icon(`<path d="m6 9 6 6 6-6"/>`)
	icSearch      = icon(`<circle cx="11" cy="11" r="6.5"/><path d="m20 20-4.2-4.2"/>`)
	icInspect     = icon(`<circle cx="11" cy="11" r="6.5"/><path d="m20 20-4.2-4.2M8.5 11l2 2 3.5-4"/>`)
	icTrash       = icon(`<path d="M4 7h16M10 11v6M14 11v6M6 7l1 13h10l1-13M9 7V4h6v3"/>`)
	icBlock       = icon(`<circle cx="12" cy="12" r="8.5"/><path d="m6 6 12 12"/>`)
	icReturn      = icon(`<path d="M9 14 4 9l5-5"/><path d="M4 9h10.5a5.5 5.5 0 0 1 0 11H11"/>`)
	icPause       = icon(`<path d="M9 5v14M15 5v14"/>`)
	icPlay        = icon(`<path d="M7 5v14l11-7z"/>`)
	icEraser      = icon(`<path d="m7 21-4-4L14 6l6 6-9 9H7zM20 21H11"/>`)
	icUpload      = icon(`<path d="M12 15V4M7 9l5-5 5 5M5 20h14"/>`)
	icDownload    = icon(`<path d="M12 4v11M7 10l5 5 5-5M5 20h14"/>`)
	icLock        = icon(`<rect x="5" y="11" width="14" height="9" rx="2"/><path d="M8 11V8a4 4 0 0 1 8 0v3"/>`)
	icSwap        = icon(`<path d="M5 8h13M15 5l3 3-3 3M19 16H6M9 13l-3 3 3 3"/>`)
	icRefresh     = icon(`<path d="M20 11a8 8 0 0 0-14.6-4.5L4 8M4 4v4h4M4 13a8 8 0 0 0 14.6 4.5L20 16M20 20v-4h-4"/>`)
	icAlert       = icon(`<path d="M12 4 2.8 19.5h18.4z"/><path d="M12 10v4M12 17h.01"/>`)
	icInfo        = icon(`<circle cx="12" cy="12" r="8.5"/><path d="M12 11v5M12 8h.01"/>`)
	icLink        = icon(`<path d="M14 4h6v6M20 4l-9 9M18 14v5H5V6h5"/>`)
	icPower       = icon(`<path d="M12 3v8M7.5 6.5a7 7 0 1 0 9 0"/>`)
	icShield      = icon(`<path d="M12 3 5 6v5c0 4.5 3 8 7 10 4-2 7-5.5 7-10V6z"/><path d="m9 12 2 2 4-4"/>`)
	icApp         = icon(`<rect x="4" y="4" width="16" height="16" rx="4"/><path d="M9 9h6v6H9z"/>`)
	icTerminal    = icon(`<rect x="3" y="5" width="18" height="14" rx="2"/><path d="m7 10 3 2-3 2M12.5 15H17"/>`)
	icCopy        = icon(`<rect x="8" y="8" width="12" height="12" rx="2"/><path d="M16 8V5a1 1 0 0 0-1-1H5a1 1 0 0 0-1 1v10a1 1 0 0 0 1 1h3"/>`)
	icEdit        = icon(`<path d="M15 5l4 4M4 20l1-4.5L16.5 4a2.1 2.1 0 0 1 3 3L8 18.5z"/>`)

	icWiFi        = icon(`<path d="M2.5 9a14 14 0 0 1 19 0M5.5 12.5a9.5 9.5 0 0 1 13 0M8.5 16a5 5 0 0 1 7 0"/><path d="M12 19.5h.01"/>`)
	icEthernet    = icon(`<path d="M4 5h16a1 1 0 0 1 1 1v9a1 1 0 0 1-1 1h-4v3H8v-3H4a1 1 0 0 1-1-1V6a1 1 0 0 1 1-1z"/><path d="M7.5 9v2M10.5 9v2M13.5 9v2M16.5 9v2"/>`)
	icCellular    = icon(`<rect x="7" y="2.5" width="10" height="19" rx="2.5"/><path d="M11 18.5h2"/>`)
	icThunderbolt = icon(`<path d="M13 3 5 13.5h6L10 21l8-10.5h-6z"/>`)
	icVPN         = icon(`<rect x="5" y="10.5" width="14" height="10" rx="2"/><path d="M8 10.5V7a4 4 0 0 1 8 0v3.5"/>`)
	icOther       = icon(`<circle cx="12" cy="12" r="8.5"/><path d="M3.5 12h17M12 3.5a13 13 0 0 1 0 17M12 3.5a13 13 0 0 0 0 17"/>`)
)

// pithMark is the mark of Pith: a tangerine cut across, five segments
// around its core, in the color of the text.
var pithMark = ui.MustParseSVG([]byte(`<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="1.8" stroke-linecap="round"><circle cx="12" cy="12" r="9"/><path d="M12 9.3V3M14.57 11.17l5.99-1.95M13.59 14.18l3.7 5.1M10.41 14.18l-3.7 5.1M9.43 11.17 3.44 9.22"/><circle cx="12" cy="12" r="1.5" fill="currentColor" stroke="none"/></svg>`))
