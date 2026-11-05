package domains

import (
	"encoding/binary"
	"net/netip"
	"strings"
	"sync"
	"time"

	"golang.org/x/net/dns/dnsmessage"
)

type dnsEntry struct {
	names map[string]time.Time
	at    time.Time
}

// Cache holds only answers paired with a query observed on the same DNS flow.
// A shared address with several live names is ambiguous: Lookup returns no
// name rather than applying a potentially unrelated domain rule.
type Cache struct {
	mu      sync.Mutex
	entries map[netip.Addr]dnsEntry
	limit   int
}

func NewCache(limit int) *Cache {
	if limit <= 0 {
		limit = 8192
	}
	return &Cache{entries: make(map[netip.Addr]dnsEntry), limit: limit}
}

func (c *Cache) Lookup(addr netip.Addr, now time.Time) string {
	c.mu.Lock()
	defer c.mu.Unlock()
	addr = addr.Unmap().WithZone("")
	entry, ok := c.entries[addr]
	if !ok {
		return ""
	}
	var found string
	count := 0
	for name, until := range entry.names {
		if !now.Before(until) {
			delete(entry.names, name)
			continue
		}
		found, count = name, count+1
	}
	if count == 0 {
		delete(c.entries, addr)
	}
	if count != 1 {
		return ""
	}
	return found
}

func (c *Cache) Observe(query, response []byte, now time.Time) {
	if len(query) > 65535 || len(response) > 65535 {
		return
	}
	var q, r dnsmessage.Message
	if q.Unpack(query) != nil || r.Unpack(response) != nil || q.Response || !r.Response || r.Truncated || q.ID != r.ID || r.RCode != dnsmessage.RCodeSuccess || q.OpCode != 0 || r.OpCode != 0 || len(q.Questions) != 1 || len(r.Questions) != 1 {
		return
	}
	question := q.Questions[0]
	answer := r.Questions[0]
	if !strings.EqualFold(question.Name.String(), answer.Name.String()) || question.Type != answer.Type || question.Class != dnsmessage.ClassINET || answer.Class != dnsmessage.ClassINET || question.Type != dnsmessage.TypeA && question.Type != dnsmessage.TypeAAAA {
		return
	}
	name := normalize(question.Name.String())
	if name == "" || len(r.Answers) > 512 {
		return
	}
	// Resolve only the CNAME chain reachable from this question. Additional
	// records and unrelated answers must not poison the IP-to-name cache.
	reachable := map[string]uint32{name: 3600}
	for range 16 {
		changed := false
		for _, rr := range r.Answers {
			alias, ok := rr.Body.(*dnsmessage.CNAMEResource)
			ttl, known := reachable[normalize(rr.Header.Name.String())]
			if !ok || !known || rr.Header.Class != dnsmessage.ClassINET {
				continue
			}
			next := normalize(alias.CNAME.String())
			if next == "" {
				continue
			}
			ttl = min(ttl, rr.Header.TTL)
			if old, exists := reachable[next]; !exists || ttl < old {
				reachable[next], changed = ttl, true
			}
		}
		if !changed {
			break
		}
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, rr := range r.Answers {
		ttl, known := reachable[normalize(rr.Header.Name.String())]
		if !known || rr.Header.Class != dnsmessage.ClassINET {
			continue
		}
		ttl = min(ttl, rr.Header.TTL)
		if ttl == 0 {
			continue
		}
		var addr netip.Addr
		switch body := rr.Body.(type) {
		case *dnsmessage.AResource:
			if question.Type == dnsmessage.TypeA {
				addr = netip.AddrFrom4(body.A)
			}
		case *dnsmessage.AAAAResource:
			if question.Type == dnsmessage.TypeAAAA {
				addr = netip.AddrFrom16(body.AAAA)
			}
		}
		if !addr.IsValid() || addr.IsUnspecified() || addr.IsMulticast() {
			continue
		}
		addr = addr.Unmap()
		entry, exists := c.entries[addr]
		if !exists {
			if len(c.entries) >= c.limit {
				// Evict the oldest insertion, with no timer or idle maintenance.
				var oldest netip.Addr
				var at time.Time
				for candidate, e := range c.entries {
					if !oldest.IsValid() || e.at.Before(at) {
						oldest, at = candidate, e.at
					}
				}
				delete(c.entries, oldest)
			}
			entry = dnsEntry{names: make(map[string]time.Time), at: now}
		}
		for host, until := range entry.names {
			if !now.Before(until) {
				delete(entry.names, host)
			}
		}
		if len(entry.names) < 8 || !entry.names[name].IsZero() {
			entry.names[name] = now.Add(time.Duration(ttl) * time.Second)
		}
		c.entries[addr] = entry
	}
}

type dnsQuery struct {
	data []byte
	at   time.Time
}

// DNS observes one UDP or TCP DNS session. Call Query for outbound data and
// Response for inbound data. The two directions may be concurrent.
type DNS struct {
	mu      sync.Mutex
	cache   *Cache
	tcp     bool
	queries map[uint16]dnsQuery
	out, in []byte
}

func NewDNS(cache *Cache, tcp bool) *DNS {
	return &DNS{cache: cache, tcp: tcp, queries: make(map[uint16]dnsQuery)}
}

func (d *DNS) Query(data []byte, now time.Time)    { d.feed(data, false, now) }
func (d *DNS) Response(data []byte, now time.Time) { d.feed(data, true, now) }

func (d *DNS) feed(data []byte, response bool, now time.Time) {
	d.mu.Lock()
	defer d.mu.Unlock()
	consume := func(data []byte) {
		if len(data) < 12 || len(data) > 65535 {
			return
		}
		id := binary.BigEndian.Uint16(data)
		if !response {
			if len(d.queries) >= 128 {
				clear(d.queries)
			}
			d.queries[id] = dnsQuery{data: append([]byte(nil), data...), at: now}
			return
		}
		query, ok := d.queries[id]
		delete(d.queries, id)
		if ok && now.Sub(query.at) <= 10*time.Second {
			d.cache.Observe(query.data, data, now)
		}
	}
	if !d.tcp {
		consume(data)
		return
	}
	buffer := &d.out
	if response {
		buffer = &d.in
	}
	if len(*buffer)+len(data) > 2*(65535+2) {
		*buffer = nil
		return
	}
	*buffer = append(*buffer, data...)
	for len(*buffer) >= 2 {
		n := int(binary.BigEndian.Uint16(*buffer))
		if len(*buffer) < n+2 {
			break
		}
		consume((*buffer)[2 : n+2])
		*buffer = (*buffer)[n+2:]
	}
	if len(*buffer) == 0 {
		*buffer = nil
	}
}
