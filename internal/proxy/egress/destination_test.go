package egress

import (
	"context"
	"errors"
	"net"
	"net/netip"
	"sync"
	"syscall"
	"testing"

	"github.com/Kel077/pith/internal/proxy"
	"golang.org/x/net/dns/dnsmessage"
)

func TestLiteralAndResolvedOwnAddressesNeverReachInterfaceSockets(t *testing.T) {
	for _, host := range []string{"192.0.2.7", "198.51.100.7", "2001:db8::7", "2001:db8::8", "fe80::7", "::ffff:192.0.2.7", "0.1.2.3", "::ffff:0.1.2.3", "127.0.0.1", "::1"} {
		for _, domain := range []bool{false, true} {
			t.Run(host+map[bool]string{false: "/literal", true: "/resolved"}[domain], func(t *testing.T) {
				f := newTestNetwork()
				addr := netip.MustParseAddr(host)
				ctx := proxy.WithInterfaceAddrs(context.Background(), func() ([]net.Addr, error) {
					// None of these addresses is in f.metadata: another physical
					// interface, a tunnel, and secondary/temporary IPv6 are included.
					return []net.Addr{&net.IPAddr{IP: addr.AsSlice(), Zone: "example1"}}, nil
				})
				useDNSResponder(f, func(_, _ string, q dnsmessage.Message) (dnsmessage.Message, error) {
					r := answerFor(q)
					question := q.Questions[0]
					var body dnsmessage.ResourceBody
					if question.Type == dnsmessage.TypeA && addr.Is4() {
						body = &dnsmessage.AResource{A: addr.As4()}
					} else if question.Type == dnsmessage.TypeAAAA && addr.Is6() {
						body = &dnsmessage.AAAAResource{AAAA: addr.As16()}
					}
					if body != nil {
						r.Answers = []dnsmessage.Resource{{Header: dnsmessage.ResourceHeader{Name: question.Name, Type: question.Type, Class: dnsmessage.ClassINET}, Body: body}}
					}
					return r, nil
				})
				target := proxy.Target{Host: host, Port: 443}
				if domain {
					target = proxy.Target{Host: "example.com", Port: 443, IsDomain: true}
				}
				conn, err := New(f.options()).Dial(ctx, testRef(), target)
				if conn != nil {
					conn.Close()
				}
				if conn != nil || !errors.Is(err, syscall.EACCES) {
					t.Fatalf("conn=%v err=%v, want not allowed", conn, err)
				}
				for _, call := range f.dials {
					if endpoint := netip.MustParseAddrPort(call.address); endpoint.Port() != 53 {
						t.Fatal("a forbidden target reached a socket", call)
					}
				}
			})
		}
	}
}

func TestInterfaceChecksTheAnswerActuallyDialed(t *testing.T) {
	f := newTestNetwork()
	ctx := proxy.WithInterfaceAddrs(context.Background(), func() ([]net.Addr, error) {
		return []net.Addr{&net.IPAddr{IP: net.ParseIP("198.51.100.7")}}, nil
	})
	var queries, ownQueries int
	var mu sync.Mutex
	useDNSResponder(f, func(_, _ string, q dnsmessage.Message) (dnsmessage.Message, error) {
		mu.Lock()
		defer mu.Unlock()
		queries++
		r := answerFor(q)
		if q.Questions[0].Type == dnsmessage.TypeA {
			ownQueries++
			ip := [4]byte{203, 0, 113, 8}
			if ownQueries > 1 {
				ip = [4]byte{198, 51, 100, 7}
			}
			r.Answers = []dnsmessage.Resource{{Header: dnsmessage.ResourceHeader{Name: q.Questions[0].Name, Type: dnsmessage.TypeA, Class: dnsmessage.ClassINET}, Body: &dnsmessage.AResource{A: ip}}}
		}
		return r, nil
	})
	n := New(f.options())
	target := proxy.Target{Host: "rebind.example.com", Port: 443, IsDomain: true}
	conn, err := n.Dial(ctx, testRef(), target)
	if err != nil {
		t.Fatal(err)
	}
	conn.Close()
	if conn, err := n.Dial(ctx, testRef(), target); conn != nil || !errors.Is(err, syscall.EACCES) {
		t.Fatalf("changed answer: conn=%v err=%v", conn, err)
	}
	var targets []string
	for _, call := range f.dials {
		if netip.MustParseAddrPort(call.address).Port() != 53 {
			targets = append(targets, call.address)
		}
	}
	if queries != 4 || len(targets) != 1 || targets[0] != "203.0.113.8:443" {
		t.Fatalf("queries=%d targets=%v, want exactly one A/AAAA lookup per request and only the first answer dialed", queries, targets)
	}
}
