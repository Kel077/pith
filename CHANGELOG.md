# Changelog

All notable changes to Pith are listed here. The format follows [Keep a Changelog](https://keepachangelog.com/en/1.1.0/), and versions follow [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

## [Unreleased]

### Fixed

- Testing an interface no longer waits 5 seconds when its DNS server leaves a query unanswered, as the M2000 does with the first one after a quiet while: Pith asks again after 0.4 seconds. A test through the M2000 took over 5 seconds; it now takes about one.
- **Change hotspot IP** no longer ends with **No internet**, or with only an IPv6 address, while the hotspot is still restarting its DNS: Pith tests it every second until it answers as it did before, for up to 25 seconds. It also tests the old IP while it reads the hotspot's settings, rather than first.

## [0.1.1] - 2026-11-07

A bug fix. It updates the helper, so macOS asks for your password once.

### Fixed

- During a speed test, or whenever many connections open and close at once, the routing switch no longer turns itself off and on with **Helper not answering**. The helper used to drop Pith's live updates when more than 128 piled up in a second; a second's updates now arrive together, however many connections changed.
- When the live updates stop but the helper still answers, Pith reconnects at once instead of reporting the helper.

## [0.1.0] - 2026-11-06

The first release, for Macs with Apple silicon.

### Added

- Rules by app (with its helpers and the system services that connect for it) and by destination (domains, IP addresses and ranges, IPv4 and IPv6), each with an ordered list of interfaces to fall back on; block rules; a check that tells which rule a connection would match.
- Rules apply at once: open connections they now send elsewhere close, for their apps to reconnect, and Activity marks them.
- Scenes: one set of rules for each place, switched from the menu bar.
- Activity: live connections with the interface each really used, traffic by app, and bytes through each interface over time.
- Network: each interface's addresses, router and DNS; a test of its latency and public address; one server reached through each interface, to compare them.
- The Inseego 5G MiFi M2000 hotspot: its signal, network, band and carrier, and **Change hotspot IP**.
- Local SOCKS5 proxies on 127.0.0.1, each port going out through one interface and failing rather than using another, with routing on or off.
- A helper checked by its exact build, not by the app's version: updating the app alone asks for no password.
- Safe start, resume routing on launch, keep routing after quitting, open at login, and the download speed in the menu bar.
- Import and export of a scene's rules, and a backup of everything.
- English and Simplified Chinese, light and dark.

### Known limitations

- Pith is signed ad hoc, not notarized: the first time, macOS asks you to allow it in Privacy & Security.
- Not yet tested with a VPN that carries all traffic, such as a Tailscale exit node.
- With iCloud Private Relay on, domain and IP rules don't see where Safari goes.
- Local proxies carry TCP only: no UDP, and no VPN interface as their exit.

[Unreleased]: https://github.com/Kel077/pith/compare/v0.1.1...HEAD
[0.1.1]: https://github.com/Kel077/pith/releases/tag/v0.1.1
[0.1.0]: https://github.com/Kel077/pith/releases/tag/v0.1.0
