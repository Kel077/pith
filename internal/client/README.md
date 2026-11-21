# App backend

`internal/client.Client` implements `api.Service` in the menu bar app. Construct it
with `client.New(client.Options{ResourcesDir: resourcesDir, ConfigDir: configDir, Version: version})`.
An empty `ConfigDir` selects `$HOME/Library/Application Support/Pith`.
Construction creates no files or listeners, starts no process and leaves the
helper alone. Call `Close` when the app quits.

## Services in the app

| Service | Behavior |
| --- | --- |
| `LoadConfig`, `SaveConfig` | Read or atomically replace validated JSON in `config.json`. A missing file yields `api.DefaultConfig`. Loading a corrupt or incompatible file reports the error and leaves the file in place. Saving syncs the private temporary file, renames it, and syncs the directory. Loading activates enabled local proxy ports and checks for an interrupted router renewal. Saving updates affected listeners and applies a changed running scene. |
| `Interfaces`, `Check` | Read native links, their kind, system service labels, addresses, primary routes, routers and DNS. Aliases come from the configuration. `Check` explains each rule in the supplied scene (the selected scene by default), including block priority and the order in which interfaces would be tried. |
| `Probe`, `SpeedTest` | A `host:port` target makes a TCP connection. An empty target tests the IPv4 and IPv6 HTTPS endpoints provided by [ipify](https://www.ipify.org/). DNS and transport sockets bind to the selected interface. Either family succeeding sets `OK`. A network failure returns `OK=false` and `Problem`. Cancellation and malformed inputs can return errors. The independent Cloudflare test measures latency and download/upload speed. [Network tests](../../docs/ARCHITECTURE.md#network-tests) |
| `Hotspot`, `Router` and their password/renewal methods | Use the selected interface to talk to the M2000 or CHR30A-R. Credentials go in the user's keychain. Router renewal additionally needs the verified helper and a working standby interface. [Router renewal](../../docs/ARCHITECTURE.md#renewing-a-routers-address) |
| Local proxies and `ChangeProxyIP` | The app owns forwarding, port replies, upstream status and device counters. Scene exits use the compiled destination rules. The Network page and ports share address renewals. [Local proxy listeners](../../docs/ARCHITECTURE.md#local-proxy-listeners) |

Interface discovery reads SystemConfiguration and the routing table. `netif.Watch`
is event driven. Cancel the subscription to release its watcher. The raw interface
events carry the system's labels. The UI looks up the user's aliases separately.

For a name, `Check` uses Go's default system resolver, returns the resolved list
in `Addresses` and evaluates the first. Darwin's native resolver is available
with `CGO_ENABLED=0` in the standard build. The `netgo` build tag selects Go's
resolver. Supply `CheckInput.Address` to test a particular address without a
lookup. The checker predicts where a connection would go from the current
interfaces. Only the real connection tests reachability. A reference with a
hardware address cannot use a different adapter that reuses its BSD name.

A failed helper reload after saving returns an error indicating that the file
was saved. Removing a running scene stops routing. A change limited to proxies
or devices can be applied locally unless an earlier routing update is pending.

## Services in the helper

`Start`, `Stop`, `Connections`, `ClearConnections` and `Traffic` use authenticated
HTTP/JSON over the helper's Unix socket. The shipped `cmd/pithd` links the routing
engine. The server's fallback `IdleBackend` reports `Stopped` and refuses
`Start` when no routing engine was supplied. The shipped binary supplies the
real engine. The client checks installed file identity and the executing root peer
before sending a request. The server accepts only root and the installing
user by kernel peer UID. With a valid compiled identity and neither an installed
file nor a socket, `Status` reports `Stopped` without an error. Routing operations
then return `ErrHelperMissing`. A verified file with different bytes or a
protocol mismatch gives `ErrHelperOutdated`. A running peer's unverifiable
identity is `ErrHelperUntrusted`.

`InstallHelper` checks the bundled `ResourcesDir/bin/pithd` against the compiled
identity before showing the macOS administrator dialog. It verifies the staged
copy's SHA-256 and attempts to restore the previous helper, plist and launchd
registration if replacement fails. Canceling the dialog returns `ErrCanceled`.
`UninstallHelper` stops the service and removes its installation and helper data.
The helper reference lists the changed paths. The upgrade guide describes
artifact verification. [Helper IPC](../helper/README.md) · [Helper updates](../../docs/HELPER-VERSIONING.md)

`Status` combines the helper's routing state with app-owned proxy and device
status. `Subscribe` starts with Status and Interfaces, then sends updates.
A lost helper stream triggers reconnection. Local proxy updates continue while
the helper is absent or unavailable. Closing the client ends its listeners and
subscriptions. The UI's quit preference governs whether the helper stops routing.

The helper obtains app attribution from `internal/engine/attribution`. It reads
the TCP/UDP `pcblist_n` sysctls and uses `proc_info` for executable paths and
unique process identities. `Process` records the socket owner and `App` the
app of the effective PID when present. Nested `.app` bundles resolve to the
outermost app. `pcblist_n` exports the effective PID. It supplies no delegated
executable UUID. The decoder uses the published XNU definitions and record layout without
copying Apple implementation code. [PCB headers](https://github.com/apple-oss-distributions/xnu/blob/main/bsd/netinet/in_pcb.h) · [Socket headers](https://github.com/apple-oss-distributions/xnu/blob/main/bsd/sys/socketvar.h) · [Record producer](https://github.com/apple-oss-distributions/xnu/blob/main/bsd/netinet/in_pcblist.c) · [libproc wrappers](https://github.com/apple-oss-distributions/xnu/blob/main/libsyscall/wrappers/libproc/libproc.c)

Synthetic tests cover delegated sockets. A bounded direct Safari/WebKit run
also verified delegated attribution on hardware. Background `nsurlsessiond`
delegation remains a separate acceptance case. [Safari result](../../docs/NEXT-ACCEPTANCE.md#recorded-safari-hardware-acceptance)

The MCP socket belongs to the running app. `Pith mcp` forwards calls there so
configuration edits use the same UI state and saver as a window edit. [MCP tools](../../docs/ARCHITECTURE.md#tools-for-agents)

## Tests

Ordinary client tests use synthetic interfaces, loopback HTTP servers and
temporary configuration. Live PCB tests check ownership without printing socket
snapshots. For a single live network probe, replace the placeholder with the
chosen interface's BSD name.

```sh
PITH_TEST_INTERFACE='<BSD name>' PITH_TEST_TARGET=example.com:443 \
  go test ./internal/client -run '^TestLiveProbeInterface$' -v
```

Omit `PITH_TEST_TARGET` to probe both public addresses. The test logs success,
latency and which families answered. Public IPs stay out of its log. The
Cloudflare speed test has its own opt-in. Router yield and renewal tests use
real hardware and the installed helper. Their opt-in variables and network effects are described in the
[hardware test reference](../../docs/ROOT-TEST.md#router-renewal-and-primary-yield).
