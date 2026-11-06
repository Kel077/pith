# Changelog

All notable changes to Pith are listed here. The format follows [Keep a Changelog](https://keepachangelog.com/en/1.1.0/), and versions follow [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

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

[0.1.0]: https://github.com/Kel077/pith/releases/tag/v0.1.0
