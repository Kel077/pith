# Changelog

All notable changes to Pith are listed here. The format follows [Keep a Changelog](https://keepachangelog.com/en/1.1.0/), and versions follow [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

## [Unreleased]

The next release updates the routing helper. Upgrading from v0.1.1 asks for your administrator password once to replace it.

### Added

- **Share with devices** on a local proxy lets other devices use the Mac's port over IPv4. **Devices** shows open connection counts and bytes per device. Unlisted visitors appear as refused. Choose **Allow** beside a visitor, or add an IPv4 address or CIDR range. The list applies to every shared port.
- Each local proxy accepts SOCKS5, HTTP CONNECT and plain HTTP on the same TCP port. Choose **Another proxy…** to forward through a SOCKS5 proxy without authentication, including another Mac's shared Pith port. Choose a scene to route new connections by its destination rules, even when that scene is inactive. Rules restricted to an app are skipped.
- Scripts can read an exit's status and public addresses with `GET /` and request a renewal with `POST /change-ip`. The POST needs **Let apps and devices change its IP** on each port in the chain. A proxy through another Pith shows its upstream network and public IP, and offers **Change IP** when the upstream permits it.
- **Change IP** on a Verizon Router CHR30A-R's Ethernet or Wi-Fi card renews its WAN IPv4 lease. Pith needs the router's admin password and another working network for the Mac. Devices behind the router lose internet access during each release. The provider can return the same address.
- `Pith mcp` lets MCP clients use the running app. Tools inspect configuration and traffic, test networks, edit rules and scenes, control routing, manage shared proxies and change a supported exit's IP. Edits appear in the window. The app accepts tool calls through a Unix socket for the current user.

### Changed

- **Test speed** replaces the public-address timing test on Network cards. It measures latency, download and upload through the selected interface and shows the public IP and Cloudflare data center when available. A full test transfers 25 MB down and 8 MB up. **Test All** runs one interface at a time. **Reach a server** still tests a host and port.
- launchd now runs the helper with `ProcessType=Interactive` so its forwarded connections avoid the background traffic class.
- Interface tests and interface-bound proxies retry a lost DNS query after 400 ms. If the interface's resolvers fail, they can query eligible resolvers from the Mac's DNS configuration through the same interface. This helps while a reconnected hotspot's DNS is starting up.

### Fixed

- A transient network-settings read no longer drops the helper's interface stream. After a stream loss, Pith reconnects and checks the helper's status if setup fails. A busy helper that still answers no longer makes **Repair** appear and disappear.
- A proxy accepts up to 1,024 simultaneous sessions per port and 2,048 across ports. At capacity, a new connection can replace the least recently active eligible stream. Pages with many connections no longer hit the old 128-session limit.
- Direct and scene-based proxy exits reject destinations assigned to this Mac, including names that resolve to those addresses. Scene ports match IP and CIDR rules against the addresses they actually dial and can try the next address while an earlier one is still connecting.
- Plain HTTP forwarding strips hop-by-hop headers and request trailers and preserves an absent User-Agent. The port's own HTTP endpoints check the Host and browser headers. Status reads have reserved capacity while address changes are pending.
- Removing a proxy in the middle of the list no longer crashes the Settings view. Exit choices with the same display name remain distinct.
- **Change hotspot IP** now measures the old addresses while reading the M2000's settings. It waits for a new cellular session when the device reports its duration, then retries the public-address test as connectivity returns. A missing IPv4 result and an unchanged IPv6 address no longer produce a misleading “unchanged” message.
- Port status retries failed or incomplete public-address lookups with backoff. A canceled reader or an older interface snapshot can no longer overwrite a current result. A slow or busy upstream Pith keeps its last known status. Changing a port's IP permission preserves its active streams.
- Requests for the same interface join one renewal. Changing a second interface at the same time returns `busy` (HTTP `503` on a proxy port). A router renewal interrupted after release attempts to reacquire the lease, with recovery on the next app launch if needed.

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
