# Capture transaction

`Build` and `Snapshot` are read-only. `Prepare` is privileged: it rejects exact
route conflicts, creates one `utun`, records a mode-0600 journal with its interface
name/index and the original routing table, and configures its addresses. No
capture routes exist yet. The caller starts the packet stack, then `Activate`
rechecks conflicts and adds the planned routes. It never replaces a default or
an existing route. Failure at any point calls the same rollback as `Close`.

Before creating the utun, `SelectAddresses` reserves the first unused IPv4 `/30`
and, when enabled, IPv6 ULA `/126` from small candidate lists. It checks both the
routing table and assigned interface prefixes, including scoped/local/reject
routes and the requested test destinations. No candidate overlaps the standard
fake-IP or Tailscale pools. Exhausting either enabled family returns
`ErrRouteConflict` before any network change; the selected subnets are journaled.

Full capture uses the eight IPv4 prefixes in `internal/captureprefix` and,
optionally, the corresponding eight IPv6 prefixes. They exclude `0.0.0.0/8` and
`::/8` to preserve Darwin's zero-address default fallback for bound sockets; see
[architecture and XNU references](../../../docs/ARCHITECTURE.md#capturing-and-forwarding).
Every prefix is independently checked and journaled on the owned utun.
LAN DNS servers receive host routes so their answers can be observed. Existing
more specific routes stay effective; Tailscale's range and DNS inside a tunnel
are not given overriding host routes. A pre-existing VPN default is saved for
system egress and reported as `WarnVPN`. A VPN already holding capture prefixes
causes `ErrRouteConflict` instead of being replaced.

Scoped development mode takes explicit CIDRs and adds only those routes. It does
not add the split defaults or DNS host routes. Tests inject a fake Driver and
therefore never create a utun or invoke privileged commands. The sole live test
reads and decodes the RIB without printing the machine's addresses.

Close deletes only planned routes whose utun name **and interface index** still
match, in reverse order. It closes the utun even if route deletion fails; kernel
utun teardown removes the remaining routes, including on process death. Startup
recovery removes stale metadata only after verifying those routes are absent.
A journal that still appears live is a conflict requiring review, not permission
to guess ownership of another VPN's routes.

Native calls follow the macOS SDK's `route(8)` and `ifconfig(8)` manuals and the
MIT-licensed `golang.zx2c4.com/wireguard/tun` API. Kernel cleanup and coexistence
are still subject to the agreed root/scoped test session on the actual Mac.
