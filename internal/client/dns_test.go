package client

import (
	"context"
	"net"
	"net/netip"
	"slices"
	"sync"
	"testing"
	"time"

	"golang.org/x/net/dns/dnsmessage"
)

// fakeDNS answers every A query with 192.0.2.1, after delay, but the first
// drop queries, which it leaves unanswered as some hotspots do.
type fakeDNS struct {
	conn    *net.UDPConn
	drop    int
	delay   time.Duration
	mu      sync.Mutex
	queries int
}

func newFakeDNS(t *testing.T, drop int, delay time.Duration) *fakeDNS {
	t.Helper()
	conn, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { conn.Close() })
	f := &fakeDNS{conn: conn, drop: drop, delay: delay}
	go f.serve()
	return f
}

func (f *fakeDNS) addr() string { return f.conn.LocalAddr().String() }

func (f *fakeDNS) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.queries
}

func (f *fakeDNS) serve() {
	buf := make([]byte, 1500)
	for {
		n, from, err := f.conn.ReadFromUDP(buf)
		if err != nil {
			return
		}
		var query dnsmessage.Message
		if query.Unpack(buf[:n]) != nil || len(query.Questions) != 1 {
			continue
		}
		f.mu.Lock()
		f.queries++
		dropped := f.queries <= f.drop
		f.mu.Unlock()
		if dropped {
			continue
		}
		q := query.Questions[0]
		reply := dnsmessage.Message{
			Header:    dnsmessage.Header{ID: query.ID, Response: true, RecursionDesired: query.RecursionDesired, RecursionAvailable: true},
			Questions: []dnsmessage.Question{q},
			Answers: []dnsmessage.Resource{{
				Header: dnsmessage.ResourceHeader{Name: q.Name, Type: dnsmessage.TypeA, Class: dnsmessage.ClassINET, TTL: 60},
				Body:   &dnsmessage.AResource{A: [4]byte{192, 0, 2, 1}},
			}},
		}
		b, err := reply.Pack()
		if err != nil {
			continue
		}
		time.AfterFunc(f.delay, func() { f.conn.WriteToUDP(b, from) })
	}
}

func lookupA(t *testing.T, addresses []string, name string) time.Duration {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()
	start := time.Now()
	addrs, err := resolverFor(&net.Dialer{}, addresses).LookupNetIP(ctx, "ip4", name)
	if err != nil || len(addrs) != 1 || addrs[0] != netip.MustParseAddr("192.0.2.1") {
		t.Fatalf("%v %v", addrs, err)
	}
	return time.Since(start)
}

func TestProbeResolverSendsAQueryAgain(t *testing.T) {
	f := newFakeDNS(t, 1, 0)
	if took := lookupA(t, []string{f.addr()}, "again.example.com."); took >= dnsTry {
		t.Fatalf("answered after %v", took)
	}
	if n := f.count(); n != 2 {
		t.Fatalf("%d queries, want 2", n)
	}
}

func TestProbeResolverTakesALateAnswer(t *testing.T) {
	// The answer to the first query comes after the second is sent, and
	// before a third would be.
	f := newFakeDNS(t, 0, 700*time.Millisecond)
	lookupA(t, []string{f.addr()}, "late.example.com.")
	if n := f.count(); n != 2 {
		t.Fatalf("%d queries, want 2", n)
	}
}

func TestProbeResolverTriesTheNextServer(t *testing.T) {
	gone, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatal(err)
	}
	gone.Close()
	f := newFakeDNS(t, 0, 0)
	if took := lookupA(t, []string{gone.LocalAddr().String(), f.addr()}, "next.example.com."); took > dnsTry+time.Second {
		t.Fatalf("answered after %v", took)
	}
}

func TestDNSAddresses(t *testing.T) {
	got := dnsAddresses("en9", []string{"192.0.2.53", "fe80::1", "fe80::2%en3", "2001:db8::53", "not an address"})
	want := []string{"192.0.2.53:53", "[fe80::1%en9]:53", "[fe80::2%en3]:53", "[2001:db8::53]:53"}
	if !slices.Equal(got, want) {
		t.Fatalf("%v, want %v", got, want)
	}
}
