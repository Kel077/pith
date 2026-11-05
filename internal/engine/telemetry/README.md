# Connection history and traffic

The store owns no background processes, goroutines or timers. The engine feeds
payload byte counts and calls `Flush` on its one-second event tick. Byte events
are coalesced per connection; opening and terminal snapshots are preserved.
Open connections are checkpointed to JSONL at most every ten seconds, and closed
connections immediately at the next flush. A forced flush synchronizes files.

Connection snapshots include the application-side local tuple and, after a
successful dial, the outbound socket's pre-NAT local tuple. They add at most
125 JSON bytes per snapshot for unzoned IP endpoints, without new records,
packet logging or additional write frequency. They cannot reveal a carrier's
public NAT port. Missing fields in old JSONL records remain empty.

Daily `connections-YYYY-MM-DD.jsonl` files obey `LogDays`; zero deletes stored
connection logs and keeps new records in memory only. JSONL can contain several
snapshots for one ID. Recovery keeps the last snapshot, ignores incomplete JSON
lines, and marks interrupted connections closed. The recent query ring holds
5,000 closed connections plus active connections. A flood can use one additional
pending ring before oldest records are discarded; aggregate traffic is retained.
Clearing the log hides existing active entries while their traffic keeps counting.

Traffic is independent of connection-log retention: ten minutes of seconds in
memory, 24 hours of minutes, nine days of fifteen-minute buckets, and five years
of daily totals on disk, per app and interface. `traffic.json` is atomically
replaced and synchronized every fifteen
seconds while dirty and on a forced flush. Queries subtract overlapping finer
buckets, including after a restart. Samples start at `Since + k*Step` and sum
stored buckets whose timestamps fall in that interval. Explicit `Since` is
preserved. Week queries use sub-hour buckets; the UI groups hourly samples into
local calendar days, including 23- and 25-hour days at daylight-saving changes.

Persistent data requires a private directory owned by the helper's effective
UID. Data files are mode 0600 and opened without following symlinks; non-regular
files are rejected. Unit tests use temporary directories and synthetic addresses.
