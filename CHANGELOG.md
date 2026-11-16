# Changelog

All notable changes to Pith are listed here. The format follows [Keep a Changelog](https://keepachangelog.com/en/1.1.0/), and versions follow [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

## [Unreleased]

### Added

- **Share** on a local proxy lets the devices listed under **Devices** use it at the Mac's address: another computer, or a phone or tablet whose Wi-Fi settings take an HTTP proxy. A device is listed by its IPv4 address or a range of them; one that isn't listed is refused, and appears under **Devices** with **Allow** beside it. Each device shows its open connections and what it has moved. Sharing asks for no password: share on networks you trust.
- A local proxy can go out through **Another proxy…**, a port another Mac's Pith shares or any SOCKS5 proxy without a password, so that a second Mac keeps the same ports on 127.0.0.1 and leaves through the first Mac's networks.
- A local proxy can go out by a scene's rules rather than through one interface: a block rule refuses a connection, a rule's interfaces are tried in order, and what no rule matches goes where macOS sends it. Rules that name apps are left out, since a proxy can't tell the app.
- Local proxies speak HTTP as well as SOCKS5 on the same port, for apps and devices that take an HTTP proxy alone.
- A local proxy's port answers two plain HTTP requests made to the port itself, for scripts and tools that drive browsers through it: `GET /` tells what it goes out through, whether it is online and its public IP addresses, and `POST /change-ip` changes that IP, once **Let apps and devices change its IP** is on for the proxy. A proxy through another Mac's Pith passes both on, and its row shows the other Mac's network and public IP, with **Change IP** when that Mac allows it. Pith changes one network's IP at a time, and answers only requests that name the port by an IP address, `localhost`, a name without dots or a `.local` name; requests that a web page makes are refused.
- An AI agent that speaks the Model Context Protocol, as Claude Code and Codex do, can work Pith for you. `/Applications/Pith.app/Contents/MacOS/Pith mcp` is the server to add to it. Its tools read the status, the scenes and rules, the interfaces, the connections and the traffic, test an interface, and check which rule a connection would match; they add, change, move and remove rules and scenes, turn routing on and off, set up local proxies and the devices that may use shared ones, and change the IP of a hotspot, a router or a local proxy's exit. What an agent changes shows in Pith at once. Only programs that run as you on the Mac reach the tools, through the Pith that is open; installing the helper, the admin passwords of hotspots and routers, and Pith's settings stay yours.
- **Change IP** on the card of an Ethernet or Wi-Fi interface whose router is a Verizon Router CHR30A-R, with its admin password: the router gives its public IPv4 address up and asks Verizon for another, waiting longer each time Verizon gives the same one back, three times at most. Meanwhile another connected network carries the Mac as macOS's default, rules skip the interface, and local proxies through it refuse connections rather than use another network. Everything else on the router's network is offline meanwhile, from seconds to over a minute. It updates the helper, which makes the other network the default meanwhile, so macOS asks for your password once.

### Changed

- **Test** on an interface's card is now **Test speed**: through that interface, Cloudflare's nearest data center measures the latency, a download of 25 MB and an upload of 8 MB, and tells the public IP it saw. The old test showed how long two HTTPS requests took, IPv4 to a nearby server and IPv6 to one in Utah, DNS lookups and TLS handshakes included: several times the real latency. **Test All** tests the interfaces one after another. A test uses about 33 MB, which counts on a metered hotspot.
- The helper runs as an app does, no longer as a background process: on a busy Mac, macOS gave it the least time, and the connections it routes yielded to the Mac's other traffic.
- When an interface's DNS servers don't answer, its tests and local proxies ask the Mac's own, the ones in its network settings that any network reaches, still through that interface. A hotspot that has just reconnected may be restarting its DNS: the M2000 refuses queries for some ten seconds, during which **Change hotspot IP** used to wait and local proxies couldn't reach any name.

### Fixed

- A local proxy no longer connects to the Mac itself by one of its network addresses. Only loopback addresses were refused, so a page opened through a proxy, whose name resolved to the Mac's address on the local network, could reach what the Mac serves there.
- Removing a local proxy other than the last of the list no longer fails. The rows after it were drawn from the shortened list, which could quit Pith.
- The helper no longer comes and goes as **Helper not answering**, with **Repair** appearing and disappearing, while the Mac is busy. The helper ended its live updates to Pith whenever one read of the network settings failed, as one can on a busy Mac, and Pith reported it whenever they ended twice within three seconds, or when it took more than half a second to answer. A failed read now keeps the updates going, and Pith reports the helper only when it isn't running, or doesn't answer within five seconds.
- Pages opened through a local proxy no longer fail with `ERR_CONNECTION_RESET` once the browser has 128 connections open through its port. A browser keeps a connection for every site a page loads from, and one news page full of ads came close to that alone. A port now takes 1024 connections, and when one is full, the connection idle the longest closes to make room: the browser opens it again when it needs it.
- Testing an interface no longer waits 5 seconds when its DNS server leaves a query unanswered, as the M2000 does with the first one after a quiet while: Pith asks again after 0.4 seconds. A test through the M2000 took over 5 seconds; it now takes about one. Local proxies ask again too, rather than wait 0.75 seconds for another server.
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
