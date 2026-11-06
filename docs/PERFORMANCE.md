# Relay performance investigation

The initial investigation identified the per-I/O interface identity lookup.
The baseline `capture.Binding.Check` called the native driver's name lookup before
each guarded socket read/write. Go's BSD `InterfaceByName` reads the entire
interface table; `InterfaceByIndex` can request one index. Checking the returned
name and index could preserve the current identity validation at much lower cost.
The original investigation changed benchmarks only. The reviewed optimizations
are recorded separately below; the baseline tables are unchanged.

## Applied pinned-index validation

The native driver now queries an existing binding by its pinned index and verifies
both returned name and index. `Binding.Check` still rejects changed epochs and
pending system-route generations, propagates capture faults, and returns
`ErrEgressChanged` for lookup/identity failure. No result is cached. New bindings
still resolve by name, and rule references retain their name/MAC resolution.
Drivers without the optional indexed check retain the previous name-based path.

`BenchmarkBoundInterfaceCheck` compares the complete production Guard check with
that fallback, including the same mutex and epoch checks. Three 500 ms read-only
loopback runs measured these medians:

| Guard path | Time/check | Bytes/check | Allocations/check |
| --- | ---: | ---: | ---: |
| Previous name lookup | 105.3 us | 34,392 | 316 |
| Pinned index plus name/index validation | 3.72 us | 1,707 | 23 |

This is about 28 times faster per check, not a claimed throughput speedup.
Tests cover name/index reuse, missing or inconsistent lookup results, epoch and
generation changes, capture faults and fresh name resolution after reindexing.
The combined hardware rerun below passed; it does not isolate this change from
output-buffer reuse. MTU is unchanged.

## Applied output-buffer reuse

`WritePacket` now retains one output buffer and one single-packet slice descriptor
per Forwarder. The existing write mutex covers growth, header reset, payload copy
and the synchronous device write; reuse begins only after that write returns.
The four-byte native header is cleared each time, the exact current packet length
is passed, and the input packet is not modified. The buffer grows only when needed
and keeps the largest capacity until the Forwarder is released. MTU, packet
contents, one-packet writes and error propagation are unchanged.

A warm-buffer microbenchmark with a memory sink, three 300 ms samples, measured:

| Packet bytes | Previous ns/write | Reused ns/write | Previous bytes/allocations | Reused bytes/allocations |
| ---: | ---: | ---: | ---: | ---: |
| 64 | 20.33 | 6.00 | 104 / 2 | 0 / 0 |
| 1500 | 148.9 | 18.65 | 1,560 / 2 | 0 / 0 |
| 4096 | 363.7 | 40.69 | 4,888 / 2 | 0 / 0 |

The larger input is a buffer-growth test, not a changed link MTU. In the MTU-1500
memory relay, allocated bytes per 4 MiB transfer fell from approximately 10.4 MB
to 5.2 MB; fixture-side copies remain. End-to-end timing varied between runs, so
these observations do not establish a hardware CPU or throughput improvement.
Tests include concurrent TCP/UDP/ICMP-shaped packets, a device retaining the input
until its synchronous write returns, mutable native headers, growth followed by
shorter packets, immutable caller input and error propagation. The concurrency
test is also run under the race detector.

## Hardware comparison after both optimizations

The coordinator verified `20ae914` using the same privately frozen endpoint and
per-adapter budgets as `8a9094f`, with 32 MiB downloads and 4 MiB uploads per
sample. Both adapters' throughput stages passed. Installation and removal used
two delegated authorizations; independent cleanup verification passed and the
default route was unchanged. This is the combined result of pinned-index checks
and output-buffer reuse, not a measurement of either change in isolation.

All rates below are application Mbps, download / upload; each value is one
bounded hardware sample rather than an average of repeated runs.

| Interface class | Phase | Before (`8a9094f`) | After (`20ae914`) |
| --- | --- | ---: | ---: |
| Cellular hotspot | Direct | 138 / 23 | 152 / 19 |
| Cellular hotspot | One rule | 139 / 22 | 145 / 14 |
| Cellular hotspot | 1001 rules | 132 / 23 | 152 / 15 |
| Wired Ethernet | Direct | 672 / 154 | 710 / 45 |
| Wired Ethernet | One rule | 680 / 145 | 773 / 81 |
| Wired Ethernet | 1001 rules | 740 / 160 | 335 / 78 |

| Interface class | Helper CPU, one rule | Helper CPU, 1001 rules | RSS after | Idle CPU before -> after |
| --- | ---: | ---: | ---: | ---: |
| Cellular hotspot | 42% -> 23.6% | 35% -> 19.6% | 27--28 MiB | 0.5% -> 2.0% |
| Wired Ethernet | 90% -> 49.5% | 100% -> 48.4% | About 29 MiB | 1.0% -> 1.0% |

CPU percentages use one core as 100%. In the **one-rule** phase, CPU percentage
per download-Mbps fell from about 0.302 to 0.163 on the hotspot (46%), and from
0.132 to 0.064 on wired Ethernet (52%). These are coarse phase comparisons: CPU
includes setup and upload, so this ratio is not steady-state CPU time per byte.
The wired 1001-rule sample does not show the same normalized improvement and must
not be averaged away or described as a 52% improvement.

Upload varied substantially: wired direct upload fell from 154 to 45 Mbps.
The wired 1001-rule download was one 0.80-second sample at 335 Mbps, versus 740
Mbps previously. Network and short-sample variation are plausible explanations,
but the measurements cannot establish the cause of that outlier. They do not
prove sustained throughput or an improvement in idle CPU.

### Rule-count cost is at connection setup

Source inspection confirms that the user rule list is not scanned per packet or
per copied payload chunk. [TCP setup](../internal/engine/forward/relay.go) captures
one immutable policy, performs a pre-acceptance app/address block check, then
calls [DialFlow](../internal/engine/egress/egress.go) to match the route after
optional sniffing. Fake-address resolution can additionally check candidate real
addresses for blocks during dialing. The selected connection and route are then
retained: `copyTCP` reads/writes, updates counters and optionally observes DNS,
without calling the matcher. UDP similarly selects a route for each new session;
its subsequent datagram loop does not scan user rules.

The per-packet VPN-prefix gate and per-I/O binding validation remain necessary,
but neither loops over the user rule list. More rules can cost more to compile,
validate/save, inspect for warnings, or match when creating a new connection.
Thus there is no 1001-rule per-packet cost to explain the wired outlier; this is
not a claim that rule count has zero cost across the entire measured phase.

## In-memory benchmark method and limits

Measurements used the module's Go toolchain on Apple silicon, without root,
capture, network requests, helper installation or any route/interface change.
Two gVisor TCP stacks communicate through the existing memory-TUN fixture and
`net.Pipe`. The real relay and traffic counters are exercised. Each operation
transfers 4 MiB on one warmed-up flow; setup, rule lookup, attribution and sniffing
are outside the measured steady transfer. The only OS network API calls are
read-only identity lookups of a dynamically selected loopback interface.

The native read/write wrapper models the legacy name lookup, index-plus-name
validation, or a no-lookup control. The control is **not** a proposed
safety bypass. Test-only link MTUs are 1500 and 9000; production remains 1500.
The interface inventory and all raw profiles remain private. Lookup cost depends
on that inventory, so these numbers are measurements, not portable constants.

The in-memory tables report medians of three unprofiled runs, with 300 ms measurement windows
at the default runtime parallelism and 500 ms with one Go execution processor.
`getrusage(RUSAGE_SELF)` supplies user-plus-system CPU across all test threads;
`cpu-ns/B` counts both TCP stacks, fixture copies and runtime work. `cpu-Mbit/s`
normalizes bytes to one aggregate CPU-second; it is not observed line rate.
CPU and allocation profiles were collected in separate three-second runs.

This model does not measure native utun reads/writes, kernel TCP, TLS, full live
guard/observer work, disk flushes, or multiple applications. It has an extra TCP
stack and fixture copy absent from the helper. It therefore cannot determine the
native helper's throughput ceiling or prove sustained 500 Mbps on hardware.

## In-memory measurements

| Identity query | Time per query | Allocated bytes | Allocations |
| --- | ---: | ---: | ---: |
| Name | 104.9 us | 34,392 | 316 |
| Index, then validate name and index | 4.08 us | 1,707 | 23 |

The index lookup was about 26 times faster in this read-only microbenchmark.
Default-parallelism download measurements were:

| Native read cap | Link MTU | Identity mode | Wall MB/s | CPU ns/byte | CPU-normalized Mbit/s |
| --- | ---: | --- | ---: | ---: | ---: |
| 32 KiB | 1500 | Name | 227 | 8.405 | 952 |
| 32 KiB | 1500 | Index | 776 | 4.576 | 1,748 |
| 32 KiB | 1500 | None, control only | 776 | 4.538 | 1,763 |
| 32 KiB | 9000 | Name | 268 | 5.401 | 1,481 |
| 32 KiB | 9000 | Index | 1,976 | 1.718 | 4,656 |
| 1460 B | 1500 | Name | 12.9 | 93.51 | 85.6 |
| 1460 B | 1500 | Index | 161 | 15.30 | 523 |
| 1460 B | 1500 | None, control only | 372 | 8.577 | 933 |

The name-to-index variant reduced CPU per byte by about 46% with full 32 KiB
reads and 84% with 1460-byte reads. The latter performs 2,874 checks per 4 MiB,
versus 129 with full reads. It demonstrates sensitivity to native read sizes;
it does not establish the actual read-size distribution of a cellular interface.
This is a plausible contributor to the hardware observation that CPU per Mbps
was higher on the slower link, not a measured explanation of that run.

Uploads at MTU 1500 measured 265 MB/s and 9.347 CPU ns/B with name checks, versus
478 MB/s and 6.005 CPU ns/B with index checks. The faster path received smaller
app-side chunks and made more checks, so a lookup-only speedup cannot be directly
multiplied into end-to-end throughput.

With `-cpu=1`, 32 KiB download reads and MTU 1500, the name/index/no-lookup models
measured 189/473/515 MB/s respectively: about 1.51/3.78/4.12 Gbit/s of synthetic
single-flow payload. Their CPU costs were 5.398/2.111/1.938 ns/B. These are the
observed model ceilings under that constraint, not native utun ceilings. Changing
runtime parallelism altered scheduler overhead significantly in this two-stack
fixture; no production `GOMAXPROCS` change is justified by this single-flow test.

The real `Store.AddBytes` path measured about 160 ns/update with zero allocations.
At 129 updates per 4 MiB that is approximately 21 us, versus tens of milliseconds
for the complete relay model. Connection-log persistence is absent here; production
already coalesces it rather than writing on every packet. There is no evidence
from these measurements that weakening counters or log retention is useful.

## Profiles and source findings

CPU profiles identify the `copyTCP` -> checked connection -> `InterfaceByName`
-> interface-table/sysctl path as hot. Native-wrapper and scheduler labels also
dominate some samples. Their precise percentages did not reconcile with the
independent lookup and process-CPU A/B measurements, especially after switching
to index lookup. Do not interpret those percentages as a hardware CPU ledger, or
sleep/wait labels as proof of busy waiting. Darwin syscall stack attribution has
known limitations tracked in [Go issue 80921](https://github.com/golang/go/issues/80921);
that issue does not by itself establish the cause of this particular discrepancy.
The optimization ranking rests on unprofiled CPU-per-byte comparisons.

In the baseline index-mode allocation profile, the real `Forwarder.WritePacket` accounts
for about 46% of allocated bytes, and the benchmark-only `memoryTUN.Write` copy
accounts for another 45%. The latter must not be charged to the production helper.
The former `WritePacket` allocated and copied a prefixed buffer per packet;
inbound packets also copy into a netstack-owned buffer. The applied output-buffer
reuse removes that per-write allocation while retaining the payload copy.
Checksum work appeared in the profile but did not explain the dominant measured
name/index delta. This study cannot precisely separate native utun syscall cost
from netstack, checksum or copy cost because it does not open a real utun.

Larger test MTU reduced index-mode CPU per byte by about 62%. Fewer packets reduce
packet construction, copies and scheduling, but changing real MTU also affects
UDP, PMTU, fragmentation, ICMP and compatibility with other tunnels. It needs its
own reviewed behavior change and hardware acceptance.

The pinned WireGuard-Go Darwin implementation reports `BatchSize() == 1`, reads
one packet, and loops over writes with one file write per packet. Passing more
buffers alone is not syscall batching on this backend. A different native I/O
strategy needs separate platform research; no batching gain is claimed here.

## Recommended order

1. Implemented: native checks for a **previously pinned index** verify its returned name
   and index, and keep existing guard epochs/generations and failure behavior.
   Retain normal name/MAC resolution for new bindings and rule references. Never
   substitute a cached result for lifetime validation. The combined hardware
   comparison above is recorded with its short-sample limitations.
2. Implemented: output-buffer reuse without changing MTU or packet ownership.
   Real counter updates remain enabled in relay comparisons; concurrent
   ICMP/TCP/UDP-shaped writer tests preserve lengths, headers and data lifetime.
3. Consider MTU and native I/O changes only after those low-risk candidates,
   with explicit IPv6/UDP/PMTU and VPN acceptance. Do not tune runtime parallelism
   from a single-flow memory model.

The first two candidates have been applied to production and the combined bounded
hardware rerun is recorded above. Native I/O and MTU changes remain separate work;
sustained throughput and isolated steady-state CPU cost remain unproven.

## Reproduce without root or network traffic

Keep outputs in a fresh private temporary directory and run one measurement
process at a time. Every benchmark closes its sockets/stacks and joins its workers.
Bound individual runs; do not run these during an attended hardware/network test.

```sh
PITH_PROFILE_DIR="$(mktemp -d "${TMPDIR%/}/pith-profile-XXXXXX")"
CGO_ENABLED=0 go test ./internal/engine/capture -run '^$' \
  -bench '^BenchmarkBoundInterfaceCheck$' -benchtime=500ms -count=3 -timeout=30s
CGO_ENABLED=0 go test ./internal/engine/forward -run '^$' \
  -bench '^BenchmarkWritePacket$' -benchtime=300ms -count=3 -timeout=30s
CGO_ENABLED=0 go test -c -o "$PITH_PROFILE_DIR/forward.test" ./internal/engine/forward
"$PITH_PROFILE_DIR/forward.test" -test.run '^$' \
  -test.bench '^Benchmark(InterfaceIdentityLookup|MemoryTCPRelay|MemorySmallReads)$' \
  -test.benchtime=300ms -test.count=3 -test.timeout=120s
"$PITH_PROFILE_DIR/forward.test" -test.run '^$' \
  -test.bench '^BenchmarkMemoryTCPRelay/download/(name|index|none)/mtu1500$' \
  -test.benchtime=500ms -test.count=3 -test.cpu=1 -test.timeout=60s
"$PITH_PROFILE_DIR/forward.test" -test.run '^$' \
  -test.bench '^BenchmarkMemoryTCPRelay/download/name/mtu1500$' \
  -test.benchtime=3s -test.count=1 -test.timeout=60s \
  -test.cpuprofile="$PITH_PROFILE_DIR/name.cpu" \
  -test.memprofile="$PITH_PROFILE_DIR/name.mem"
go tool pprof -top "$PITH_PROFILE_DIR/forward.test" "$PITH_PROFILE_DIR/name.cpu"
go tool pprof -top -alloc_space "$PITH_PROFILE_DIR/forward.test" "$PITH_PROFILE_DIR/name.mem"
CGO_ENABLED=0 go test ./internal/engine/telemetry -run '^$' \
  -bench '^BenchmarkTrafficAddBytes$' -benchtime=500ms -count=3 -timeout=30s
```
