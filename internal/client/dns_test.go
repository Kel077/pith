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

// fakeDNS answers every A query with 192.0.2.1, or answer, after delay, but
// the first drop queries, which it leaves unanswered as some hotspots do.
// With nxdomain, it says no name exists.
type fakeDNS struct {
	conn     *net.UDPConn
	drop     int
	delay    time.Duration
	answer   [4]byte
	nxdomain bool
	mu       sync.Mutex
	queries  int
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

// with changes how it answers.
func (f *fakeDNS) with(change func(f *fakeDNS)) *fakeDNS {
	f.mu.Lock()
	defer f.mu.Unlock()
	change(f)
	return f
}

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
		answer, nxdomain := f.answer, f.nxdomain
		f.mu.Unlock()
		if dropped {
			continue
		}
		q := query.Questions[0]
		if answer == ([4]byte{}) {
			answer = [4]byte{192, 0, 2, 1}
		}
		reply := dnsmessage.Message{
			Header:    dnsmessage.Header{ID: query.ID, Response: true, RecursionDesired: query.RecursionDesired, RecursionAvailable: true},
			Questions: []dnsmessage.Question{q},
		}
		if nxdomain {
			reply.RCode = dnsmessage.RCodeNameError
		} else {
			reply.Answers = []dnsmessage.Resource{{
				Header: dnsmessage.ResourceHeader{Name: q.Name, Type: dnsmessage.TypeA, Class: dnsmessage.ClassINET, TTL: 60},
				Body:   &dnsmessage.AResource{A: answer},
			}}
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

// refusingAddress is a loopback port where no one listens: a query to it is
// refused at once, as the M2000's are while its DNS restarts.
func refusingAddress(t *testing.T) string {
	t.Helper()
	gone, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatal(err)
	}
	gone.Close()
	return gone.LocalAddr().String()
}

func testProbeDNS(own, fallback string) probeDNS {
	return probeDNS{own: resolverFor(&net.Dialer{}, []string{own}), fallback: resolverFor(&net.Dialer{}, []string{fallback})}
}

func TestProbeDNSAsksTheMacsServersWhenItsOwnDont(t *testing.T) {
	quiet := newFakeDNS(t, 1<<30, 0)
	for _, tt := range []struct {
		name, own string
		least     time.Duration // before it asks the Mac's
	}{
		{"refused", refusingAddress(t), 0},
		{"unanswered", quiet.addr(), dnsOwnTime},
	} {
		t.Run(tt.name, func(t *testing.T) {
			macs := newFakeDNS(t, 0, 0)
			p := testProbeDNS(tt.own, macs.addr())
			start := time.Now()
			r := p.resolver(context.Background(), "tcp4", "fallback.example.com.")
			if took := time.Since(start); r != p.fallback || took < tt.least || took > tt.least+time.Second {
				t.Fatalf("took the Mac's: %v, after %v", r == p.fallback, took)
			}
		})
	}
}

func TestProbeDNSKeepsToItsOwnServers(t *testing.T) {
	for _, nxdomain := range []bool{false, true} {
		own := newFakeDNS(t, 0, 0).with(func(f *fakeDNS) { f.nxdomain = nxdomain })
		macs := newFakeDNS(t, 0, 0)
		p := testProbeDNS(own.addr(), macs.addr())
		if r := p.resolver(context.Background(), "tcp4", "own.example.com."); r != p.own {
			t.Fatalf("no such name %v: took the Mac's servers", nxdomain)
		}
		if n := macs.count(); n != 0 {
			t.Fatalf("no such name %v: asked the Mac's servers %d times", nxdomain, n)
		}
	}
}

func TestProbeDNSDialsTheNamedHost(t *testing.T) {
	listener, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	go func() {
		if conn, err := listener.Accept(); err == nil {
			conn.Close()
		}
	}()
	macs := newFakeDNS(t, 0, 0).with(func(f *fakeDNS) { f.answer = [4]byte{127, 0, 0, 1} })
	_, port, _ := net.SplitHostPort(listener.Addr().String())
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	conn, err := testProbeDNS(refusingAddress(t), macs.addr()).dial(ctx, &net.Dialer{}, "tcp4", net.JoinHostPort("dial.example.com.", port))
	if err != nil {
		t.Fatal(err)
	}
	conn.Close()
}
