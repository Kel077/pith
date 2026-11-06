# Root-free relay performance investigation

The first optimization candidate is the per-I/O interface identity lookup.
Production `capture.Binding.Check` calls the native driver's name lookup before
each guarded socket read/write. Go's BSD `InterfaceByName` reads the entire
interface table; `InterfaceByIndex` can request one index. Checking the returned
name and index could preserve the current identity validation at much lower cost.
This investigation changes benchmarks only, not production behavior.

## Method and limits

Measurements used the module's Go toolchain on Apple silicon, without root,
capture, network requests, helper installation or any route/interface change.
Two gVisor TCP stacks communicate through the existing memory-TUN fixture and
`net.Pipe`. The real relay and traffic counters are exercised. Each operation
transfers 4 MiB on one warmed-up flow; setup, rule lookup, attribution and sniffing
are outside the measured steady transfer. The only OS network API calls are
read-only identity lookups of a dynamically selected loopback interface.

The native read/write wrapper models the existing name lookup, an index-plus-name
validation candidate, or a no-lookup control. The control is **not** a proposed
safety bypass. Test-only link MTUs are 1500 and 9000; production remains 1500.
The interface inventory and all raw profiles remain private. Lookup cost depends
on that inventory, so these numbers are measurements, not portable constants.

Tables report medians of three unprofiled runs, with 300 ms measurement windows
at the default runtime parallelism and 500 ms with one Go execution processor.
`getrusage(RUSAGE_SELF)` supplies user-plus-system CPU across all test threads;
`cpu-ns/B` counts both TCP stacks, fixture copies and runtime work. `cpu-Mbit/s`
normalizes bytes to one aggregate CPU-second; it is not observed line rate.
CPU and allocation profiles were collected in separate three-second runs.

This model does not measure native utun reads/writes, kernel TCP, TLS, full live
guard/observer work, disk flushes, or multiple applications. It has an extra TCP
stack and fixture copy absent from the helper. It therefore cannot determine the
native helper's throughput ceiling or prove sustained 500 Mbps on hardware.

## Measurements

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

In the index-mode allocation profile, the real `Forwarder.WritePacket` accounts
for about 46% of allocated bytes, and the benchmark-only `memoryTUN.Write` copy
accounts for another 45%. The latter must not be charged to the production helper.
`WritePacket` allocates and copies a prefixed buffer per packet; inbound packets
also copy into a netstack-owned buffer. Pooling or reusing the output buffer is a
second candidate, subject to packet-lifetime and concurrent-writer tests.
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

1. Add a native check for a **previously pinned index**, verify its returned name
   and index, and keep existing guard epochs/generations and failure behavior.
   Retain normal name/MAC resolution for new bindings and rule references. Never
   substitute a cached result for lifetime validation. Review before changing code.
2. Evaluate output-buffer reuse without changing MTU or packet ownership. Keep
   real counter updates enabled in comparisons and test concurrent ICMP/TCP/UDP writers.
3. Consider MTU and native I/O changes only after those low-risk candidates,
   with explicit IPv6/UDP/PMTU and VPN acceptance. Do not tune runtime parallelism
   from a single-flow memory model.

No candidate in this list has been applied to production.

## Reproduce without root or network traffic

Keep outputs in a fresh private temporary directory and run one measurement
process at a time. Every benchmark closes its sockets/stacks and joins its workers.
Bound individual runs; do not run these during an attended hardware/network test.

```sh
PITH_PROFILE_DIR="$(mktemp -d "${TMPDIR%/}/pith-profile-XXXXXX")"
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
