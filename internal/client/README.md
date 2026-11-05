# Backend integration (M1)

Construct `client.New(client.Options{ResourcesDir: resourcesDir, ConfigDir:
configDir, Version: version})`. `ConfigDir` can be empty to use
`~/Library/Application Support/Pith`. Construction does not write or start
anything. `*client.Client` implements `api.Service`.

Available without a helper:

- `LoadConfig` / `SaveConfig`: validated JSON, private permissions, atomic
  replacement, file and directory sync. Missing file means `DefaultConfig`;
  corrupt or incompatible data is reported, never silently overwritten.
- `Interfaces`: native links, service labels, aliases, kind, addresses,
  primary/default routes, per-service routers and DNS. SystemConfiguration
  and routing table reads are read-only. `netif.Watch` is event driven and
  must be canceled when its subscriber exits.
- `Probe`: TCP `host:port`, or HTTPS requests to the IPv4 and IPv6 endpoints
  documented by [ipify](https://www.ipify.org/). DNS and transport sockets
  are bound to the requested interface. These requests only happen on Probe;
  no background public-address polling. Either family succeeding sets `OK`.
  A network failure is a Probe with `OK=false` and `Problem`; cancellation
  and invalid inputs are errors.
- `Check`: the selected scene by default, block priority, detailed reasons
  for every rule, and ordered interface fallback. For a domain resolving to
  several addresses it evaluates the first and returns all in `Addresses`;
  provide `CheckInput.Address` to test a specific one without a DNS lookup.
  It uses Go's default system resolver (Darwin uses the native resolver even
  with `CGO_ENABLED=0`, unless explicitly built with `netgo`). Interface
  availability predicts the route; it cannot guarantee the destination will
  connect. A MAC-bearing interface reference never falls back to a different
  adapter that happens to reuse its BSD name.

Helper operations currently return `ErrHelperMissing`; `Status` reports
`Stopped` and no installed helper. IPC, installation and running-scene hot
application are the next milestone. Do not show these stubs as working routing.

`internal/engine/attribution` reads the TCP/UDP `pcblist_n` sysctls and uses
`proc_info` for executable paths and unique identities. It preserves the
socket-owning `Process` while attributing `App` to the effective PID when
present. Nested `.app` bundles resolve to the outermost app. The decoder was
implemented from Apple XNU's [PCB headers](https://github.com/apple-oss-distributions/xnu/blob/main/bsd/netinet/in_pcb.h),
[socket headers](https://github.com/apple-oss-distributions/xnu/blob/main/bsd/sys/socketvar.h),
[record producer](https://github.com/apple-oss-distributions/xnu/blob/main/bsd/netinet/in_pcblist.c),
and [libproc wrappers](https://github.com/apple-oss-distributions/xnu/blob/main/libsyscall/wrappers/libproc/libproc.c),
without copying Apple implementation code. `pcblist_n` exports effective PID,
not the delegated executable UUID. Actual WebKit/nsurlsessiond delegation
still needs an integration test; synthetic delegated sockets are covered.

All ordinary tests use synthetic documentation addresses or temporary local
loopback sockets. Live PCB tests assert ownership without printing snapshots.
An optional single network probe is:

```sh
PITH_TEST_INTERFACE=en0 PITH_TEST_TARGET=example.com:443 go test ./internal/client -run TestLiveProbeInterface -v
```

Omit `PITH_TEST_TARGET` to test HTTPS/public addresses. The test logs success
and latency, not public addresses. No test installs a helper or changes routes.
