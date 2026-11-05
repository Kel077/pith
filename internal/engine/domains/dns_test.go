package domains

import (
	"encoding/binary"
	"net/netip"
	"testing"
	"time"

	"golang.org/x/net/dns/dnsmessage"
)

func dnsPair(t *testing.T, host string, ttl uint32) (query, response []byte) {
	t.Helper()
	name := dnsmessage.MustNewName(host + ".")
	q := dnsmessage.Message{Header: dnsmessage.Header{ID: 42}, Questions: []dnsmessage.Question{{Name: name, Type: dnsmessage.TypeA, Class: dnsmessage.ClassINET}}}
	query, err := q.Pack()
	if err != nil {
		t.Fatal(err)
	}
	q.Response = true
	q.Answers = []dnsmessage.Resource{{Header: dnsmessage.ResourceHeader{Name: name, Type: dnsmessage.TypeA, Class: dnsmessage.ClassINET, TTL: ttl}, Body: &dnsmessage.AResource{A: [4]byte{192, 0, 2, 10}}}}
	response, err = q.Pack()
	if err != nil {
		t.Fatal(err)
	}
	return
}

func TestDNSCorrelationAmbiguityAndTTL(t *testing.T) {
	c := NewCache(4)
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	addr := netip.MustParseAddr("192.0.2.10")
	q, r := dnsPair(t, "example.com", 5)
	corrupt := append([]byte(nil), r...)
	corrupt[1]++
	c.Observe(q, corrupt, now)
	if c.Lookup(addr, now) != "" {
		t.Fatal("unpaired response cached")
	}
	c.Observe(q, r, now)
	if c.Lookup(addr, now) != "example.com" {
		t.Fatal("paired response missing")
	}
	q2, r2 := dnsPair(t, "other.example.com", 20)
	c.Observe(q2, r2, now)
	if c.Lookup(addr, now) != "" {
		t.Fatal("ambiguous IP attributed to one domain")
	}
	if c.Lookup(addr, now.Add(6*time.Second)) != "other.example.com" {
		t.Fatal("expired mapping not removed")
	}
	if c.Lookup(addr, now.Add(21*time.Second)) != "" {
		t.Fatal("TTL ignored")
	}
}

func TestDNSCNAMEChainRejectsUnrelatedAnswers(t *testing.T) {
	c := NewCache(4)
	q, r := dnsPair(t, "example.com", 100)
	var message dnsmessage.Message
	if err := message.Unpack(r); err != nil {
		t.Fatal(err)
	}
	alias := dnsmessage.MustNewName("cdn.example.com.")
	message.Answers[0].Header.Name = alias
	message.Answers = append(message.Answers, dnsmessage.Resource{Header: dnsmessage.ResourceHeader{Name: message.Questions[0].Name, Type: dnsmessage.TypeCNAME, Class: dnsmessage.ClassINET, TTL: 2}, Body: &dnsmessage.CNAMEResource{CNAME: alias}})
	message.Answers = append(message.Answers, dnsmessage.Resource{Header: dnsmessage.ResourceHeader{Name: dnsmessage.MustNewName("unrelated.example.com."), Type: dnsmessage.TypeA, Class: dnsmessage.ClassINET, TTL: 100}, Body: &dnsmessage.AResource{A: [4]byte{192, 0, 2, 99}}})
	r, err := message.Pack()
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	c.Observe(q, r, now)
	if c.Lookup(netip.MustParseAddr("192.0.2.10"), now) != "example.com" {
		t.Fatal("CNAME answer missing")
	}
	if c.Lookup(netip.MustParseAddr("192.0.2.99"), now) != "" {
		t.Fatal("unrelated answer cached")
	}
	if c.Lookup(netip.MustParseAddr("192.0.2.10"), now.Add(3*time.Second)) != "" {
		t.Fatal("CNAME TTL ignored")
	}
}

func TestDNSTCPReassemblyAndPendingExpiry(t *testing.T) {
	c := NewCache(4)
	d := NewDNS(c, true)
	q, r := dnsPair(t, "example.com", 60)
	frame := func(p []byte) []byte { return append(binary.BigEndian.AppendUint16(nil, uint16(len(p))), p...) }
	q, r = frame(q), frame(r)
	now := time.Now()
	for _, b := range q {
		d.Query([]byte{b}, now)
	}
	for _, b := range r {
		d.Response([]byte{b}, now)
	}
	if c.Lookup(netip.MustParseAddr("192.0.2.10"), now) != "example.com" {
		t.Fatal("fragmented TCP DNS response lost")
	}
	c = NewCache(4)
	d = NewDNS(c, false)
	d.Query(q[2:], now)
	d.Response(r[2:], now.Add(11*time.Second))
	if c.Lookup(netip.MustParseAddr("192.0.2.10"), now.Add(11*time.Second)) != "" {
		t.Fatal("expired DNS request accepted")
	}
}

func FuzzDNS(f *testing.F) {
	f.Add([]byte{0, 42, 1, 0, 0, 0, 0, 0, 0, 0, 0, 0}, []byte{0, 42, 129, 128, 0, 0, 0, 0, 0, 0, 0, 0})
	f.Fuzz(func(t *testing.T, q, r []byte) {
		if len(q) > 65535 || len(r) > 65535 {
			t.Skip()
		}
		c := NewCache(4)
		c.Observe(q, r, time.Now())
		d := NewDNS(c, true)
		d.Query(q, time.Now())
		d.Response(r, time.Now())
	})
}
