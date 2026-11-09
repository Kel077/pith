# Pith — interface design

Pith decides which network each new connection leaves through. The
interface keeps to that, and makes three things plain at a glance: whether
routing is on, by which rules, and where each connection really went.

## Principles

1. **Native and calm.** It looks like a Mac app of today: light glass over
   a soft background, capsules, hairlines, the system font, the system's
   light and dark appearances.
2. **Say little.** Titles, labels, values and status words; no paragraphs.
   What needs explaining goes in a tooltip, not on the page.
3. **A rule is a sentence.** "WeChat → iPhone USB › Ethernet › System"
   reads left to right as what it does.
4. **Four places.** Rules (with scenes and the check), Activity
   (connections, apps, traffic), Network (interfaces and tests), Settings.
5. **The switch is always in reach**: the routing capsule at the top right
   of the window, and at the top of the menu bar panel.
6. **Saving applies.** Edits save as they are made, and the running scene
   takes them for new connections.
7. **Say where it really went.** A connection's interface shows in that
   interface's color when a rule sent it there, and as a quiet outlined
   chip when macOS routed it.

## The look

- **Ambient background.** A warm off-white (dark: near-black) with three
  soft glows: tangerine at the top left, rose at the right, periwinkle at
  the bottom (`ambient` in `internal/ui/glass.go`).
- **Panes of light glass** over it (`glassy`, `pane`, `paneRow`,
  `paneRows`): MyGo's glass material with some clarity, so the glows show
  through; a one-pixel rim catching the light; a wide, faint shadow;
  radius 18. Lists are one pane whose rows are divided by hairlines, as in
  System Settings, never a stack of boxes.
- **Capsules** for navigation and actions: the pages and the scenes as
  segmented capsules of glass whose chosen segment is a bright pill
  (`segmentedCapsule`, keyboard- and VoiceOver-friendly); quiet actions as
  glass capsules (`glassCapsuleButton`); the page's main action as a
  tangerine capsule lit from above (`primaryCapsule`).
- **Color.** Tangerine (`#ee7a1e`, dark `#ff9a45`) only for the main
  action, the chosen state and switches. Green for routing that runs, red
  for blocking, amber for fallbacks and warnings. Each interface keeps one
  calm color everywhere (Ethernet blue, cellular violet, Wi-Fi teal, then
  others), shown as a dot, a chip, a chart's series; a disconnected one is
  a faded ring.
- **Type.** The system font. Page titles 28 pt bold; row titles 14 pt
  semibold; body 13 pt; secondary 12 pt muted. Numbers that change use
  tabular figures (`FontFeatures("tnum")`) so they don't jitter.
- **Spacing.** A 4 pt grid; 32 pt page margins; panes 18 pt round,
  capsules fully round.

## The window

A hidden title bar. At the top, a 64 pt bar: the traffic lights on the
left, level with the capsule of the four pages in the middle, and the
routing capsule on the right (a dot, the running scene, the switch). Each
page has a large title and its actions on the right.

- **Rules.** The scenes as a segmented capsule (the running one with a
  green dot), "+" and "…" beside it, and an ⓘ whose tooltip says how rules
  decide. The rules are rows of one pane: app icons, names, what they
  match, the route as chips, a switch. A rule an earlier rule covers says
  so in amber. Hover shows a drag handle; right-click edits, duplicates,
  moves to the top or deletes (with Undo).
- **Rule editor** and **Check**: sheets of glass, labeled rows, chips,
  segmented capsules, the ordered interfaces as a pane of rows.
- **Activity.** Connections (live, newest first), Apps, and Traffic (bars
  per interface over an hour, a day or a week), each as one pane.
- **Network.** A pane per connected interface, a compact group for the
  others, and a reachability test. A pane's foot tells its speed test: the
  download and upload large, and over them the public IP, the server and
  the latency. A hotspot Pith can talk to shows as
  itself on its pane, drawn without its maker's marks, and its screen
  says what it reports, as the device's own screen would: the bars in the
  interface's color and 5G or LTE, the carrier, the band large with its
  frequency, and the signal's strength and SNR. The screen stays white in
  the dark, as the device's does. Locked, it shows a lock that asks for
  the admin password. "Change hotspot IP" has the hotspot reconnect, and
  then says the old and the new IP.
- **Settings.** Groups of rows, as System Settings has them.

## The menu bar

A template icon, faint while routing is off and solid while it runs,
optionally followed by the download speed. A click drops a panel down from
the menu bar, centered under the icon, on the popover's material but
without its arrow: the switch, the scenes, the interfaces with their speed,
and buttons to open the window or quit.

## Language

English and Simplified Chinese, following the system unless Settings says
otherwise. Messages live in `locales/en.json`, which describes them, and
`locales/zh-Hans.json`; MyGo generates a function per message
(`locales.AddRule()`), and the views say them every frame.

## Reviewing the views

`go test ./internal/ui -run 'Snapshots$' -snapshots=DIR -lang=en` (or
`-lang=zh-Hans`) renders every page, the editor, the check, the panel,
the warnings and the first launch, light and dark, from the mock backend.
`go run ./cmd/pith -mock -page activity -lang zh-Hans` runs the real app
on the mock.
