# 橘核 Pith — interface design

Pith does one thing: it decides which network each new connection leaves
through. The interface keeps to that, and tries to make three answers
obvious at a glance: **is routing on, by which rules, and where did each
connection really go.**

## Principles

1. **A rule is a sentence.** "WeChat → iPhone USB › Ethernet › System" reads
   left to right as what it does: these apps, to these places, go out
   through these interfaces, in this order. No table of checkboxes.
2. **Four places, not ten.** Rules (with scenes and the rule check),
   Activity (connections, apps, traffic), Network (interfaces and tests),
   Settings.
3. **The switch is always in reach.** The routing switch sits at the foot
   of the sidebar on every page and at the top of the menu bar panel,
   with the scene that runs and for how long.
4. **Saving applies.** Edits save as they are made and the running scene
   picks them up for new connections; there is no "restart the scene"
   step to remember.
5. **Say where it really went.** Every connection shows the interface it
   left through, filled in that interface's color when a rule sent it
   there, and as a quiet outlined chip when macOS routed it.
6. **Quiet by default.** Neutral surfaces from the system's light or dark
   appearance; color only for state and for the user's next action.

## Color

- **Accent — tangerine** `#ee7a1e` (dark: `#ff9a45`): primary buttons,
  the chosen scene, switches, the sidebar's icons. Nothing else is orange.
- **Running — green**: the dot of routing that runs, open connections.
- **Danger — red**: block rules and blocked connections. **Warning —
  amber**: fallbacks and failures.
- **Interfaces** each get one color from a calm palette by their kind —
  Ethernet blue, cellular violet, Wi-Fi teal, then others — assigned so
  that two never share one. The color follows the interface everywhere:
  rule chips, the activity's chips and charts, the traffic bars, the
  menu bar panel. A disconnected interface shows as a ring, faded.

## Type and spacing

The system font (SF Pro / PingFang SC) through MyGo. Page titles 20 pt
bold; row titles 14 pt semibold; body 13 pt; secondary 12 pt in the
theme's muted text; numbers that change (speeds, bytes, times) in the
monospaced face so they do not jitter. A 4 pt grid: 24 pt page margins,
12 pt card radius, 8 pt between rows.

## Pages

- **Rules.** Scenes as pills under the title (the running one with a green
  dot); "…" holds rename, duplicate, import, export and delete. Each rule
  is a card: app icons and names, then its destinations and protocol in
  muted text, then the route as chips, then its switch. Hover shows a
  drag handle; right-click edits, duplicates, moves to the top or
  deletes (with Undo). A banner offers to switch when the page shows a
  scene other than the running one. New rules go to the top, since they
  are usually exceptions to the rules below them.
- **Rule editor.** One dialog, top to bottom in the order of the sentence:
  apps (installed apps with icons and search, or any program), destinations
  as chips (paste a list to add many; invalid ones stay in the field in
  red), whether domains include subdomains, protocol, action, the ordered
  interfaces, the fallback, a note. Problems show only after Save.
- **Rule check.** Scene, app, destination (a URL works), optional actual
  IP and protocol give the outcome first — "goes out through Ethernet, by
  rule 3" — then each rule's verdict.
- **Activity.** Connections (live, newest first; pause and clear; search),
  Apps (traffic per app with a bar), and Traffic (stacked bars per
  interface over an hour, a day or a week, with totals). Right-click a
  connection to make a rule from its app or its destination.
- **Network.** A card per connected interface (addresses, router, DNS,
  hardware address, a test of latency and public IP; double-click the
  name to rename it), a compact list of the others, and a reachability
  test of one server through every interface.
- **Settings.** General, Routing, Helper, Data, About, as grouped rows with
  an explanation under each label.

## The menu bar

A template icon, faint while routing is off, solid while it runs,
optionally followed by the download speed. A click opens a panel:
the switch, the scenes (a check on the one that runs), the connected
interfaces with their speed, totals, and buttons to open the window or
quit. A right-click offers the same two in a menu.

## First run

The window opens on Rules with an empty state that explains rules in one
sentence. Turning routing on without the helper explains, in three
lines, what the helper does and does not do, before macOS asks for the
password.

## Language

English and Simplified Chinese, following the system unless Settings
chooses one. Strings live where they show, in both languages:
`tr("Rules", "规则")`.

## Reviewing the views

`go test ./internal/ui -run Snapshots -snapshots=DIR` renders every page,
the editor, the check, the panel and the first run, light and dark, from
the mock backend into PNGs. `go run ./cmd/pith -mock -page activity
-lang zh-Hans` runs the real app on the mock.
