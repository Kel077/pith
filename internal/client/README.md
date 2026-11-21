# App backend

`internal/client.Client` implements `api.Service` in the menu bar app. Construct it
with `client.New(client.Options{ResourcesDir: resourcesDir, ConfigDir: configDir, Version: version})`.
The default configuration directory is `$HOME/Library/Application Support/Pith`.
Construction opens no listeners. Call `Close` when the app quits.

## Services in the app

| Service | Behavior |
| --- | --- |
| `LoadConfig`, `SaveConfig` | Read or atomically replace `config.json`. A missing file yields the default configuration. A corrupt file returns an error on load. Loading activates enabled local proxy ports and checks for an interrupted router renewal. Saving updates affected listeners and applies a changed running scene. |
| `Interfaces`, `Check` | Read links, service labels, addresses, routers, DNS and default status. The UI applies saved aliases. The checker predicts a scene's decision using the current interfaces. For a name, it returns the resolved addresses and evaluates the first. Supply `CheckInput.Address` to check a particular address. |
| `Probe`, `SpeedTest` | An explicit probe target makes a bound TCP connection. An empty target tests IPv4 and IPv6 through ipify. The independent Cloudflare test measures latency and download/upload speed. [Network tests](../../docs/ARCHITECTURE.md#network-tests) |
| `Hotspot`, `Router` and their password/renewal methods | Use the selected interface to talk to the M2000 or CHR30A-R. Credentials go in the user's keychain. Router renewal additionally needs the verified helper and a working standby interface. [Router renewal](../../docs/ARCHITECTURE.md#renewing-a-routers-address) |
| Local proxies and `ChangeProxyIP` | The app owns forwarding, port replies, upstream status and device counters. Scene exits use the compiled destination rules. The Network page and ports share address renewals. [Local proxy listeners](../../docs/ARCHITECTURE.md#local-proxy-listeners) |

A failed helper reload after saving returns an error indicating that the file
was saved. Removing a running scene stops routing. A change limited to proxies
or devices can be applied locally unless an earlier routing update is pending.

## Services in the helper

`Start`, `Stop`, `Connections`, `ClearConnections` and `Traffic` use authenticated
HTTP/JSON over the helper's Unix socket. The shipped `cmd/pithd` links the routing
engine. The client checks installed file identity and the executing root peer
before sending a request. `InstallHelper` and `UninstallHelper` use macOS
administrator authorization. The helper reference lists the managed paths and
the upgrade guide describes artifact verification. [Helper IPC](../helper/README.md) · [Helper updates](../../docs/HELPER-VERSIONING.md)

`Status` combines the helper's routing state with app-owned proxy and device
status. `Subscribe` starts with Status and Interfaces, then sends updates.
A lost helper stream triggers reconnection. Local proxy updates continue while
the helper is absent or unavailable. Closing the client ends its listeners and
subscriptions. The UI's quit preference governs whether the helper stops routing.

The MCP socket belongs to the running app. `Pith mcp` forwards calls there so
configuration edits use the same UI state and saver as a window edit. [MCP tools](../../docs/ARCHITECTURE.md#tools-for-agents)

## Tests

Ordinary client tests use synthetic interfaces, loopback HTTP servers and
temporary configuration. Opt-in tests cover a live bound probe and a Cloudflare
speed test. Router yield and renewal tests use real hardware and the installed
helper. Their opt-in variables and network effects are described in the
[hardware test reference](../../docs/ROOT-TEST.md#router-renewal-and-primary-yield).
