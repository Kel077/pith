package egress

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Kel077/pith/internal/api"
	"github.com/Kel077/pith/internal/netif"
	"github.com/Kel077/pith/internal/proxy"
	"golang.org/x/net/dns/dnsmessage"
)

func dnsName(name string) dnsmessage.Name { return dnsmessage.MustNewName(name) }
func addressRecord(name string, typ dnsmessage.Type) dnsmessage.Resource {
	r := dnsmessage.Resource{Header: dnsmessage.ResourceHeader{Name: dnsName(name), Type: typ, Class: dnsmessage.ClassINET, TTL: 60}}
	if typ == dnsmessage.TypeA {
		r.Body = &dnsmessage.AResource{A: [4]byte{203, 0, 113, 8}}
	} else {
		r.Body = &dnsmessage.AAAAResource{AAAA: netip.MustParseAddr("2001:db8::8").As16()}
	}
	return r
}
func cnameRecord(name, alias string) dnsmessage.Resource {
	return dnsmessage.Resource{Header: dnsmessage.ResourceHeader{Name: dnsName(name), Type: dnsmessage.TypeCNAME, Class: dnsmessage.ClassINET, TTL: 60}, Body: &dnsmessage.CNAMEResource{CNAME: dnsName(alias)}}
}

type respondFunc func(network, address string, query dnsmessage.Message) (dnsmessage.Message, error)

func useDNSResponder(f *testNetwork, respond respondFunc) {
	f.dial = func(ctx context.Context, name, network, address string) (net.Conn, error) {
		if err := netif.ValidateBinding(ctx, 7); err != nil {
			return nil, err
		}
		if name != "example0" {
			return nil, errors.New("unexpected interface")
		}
		target, err := netip.ParseAddrPort(address)
		if err != nil {
			return nil, err
		}
		if target.Port() != 53 {
			return newTestConn(nil), nil
		}
		c := newTestConn(nil)
		c.onWrite = func(data []byte) ([]byte, error) {
			if strings.HasPrefix(network, "tcp") {
				if len(data) < 2 || int(binary.BigEndian.Uint16(data[:2])) != len(data)-2 {
					return nil, errors.New("invalid TCP DNS frame")
				}
				data = data[2:]
			}
			var query dnsmessage.Message
			if err := query.Unpack(data); err != nil {
				return nil, err
			}
			if query.Response || !query.RecursionDesired || len(query.Questions) != 1 || !strings.HasSuffix(query.Questions[0].Name.String(), ".") {
				return nil, errors.New("invalid DNS request")
			}
			response, err := respond(network, address, query)
			if err != nil {
				return nil, err
			}
			packed, err := response.Pack()
			if err != nil {
				return nil, err
			}
			if strings.HasPrefix(network, "tcp") {
				frame := make([]byte, 2+len(packed))
				binary.BigEndian.PutUint16(frame, uint16(len(packed)))
				copy(frame[2:], packed)
				packed = frame
			}
			return packed, nil
		}
		return c, nil
	}
}

func answerFor(query dnsmessage.Message) dnsmessage.Message {
	return dnsmessage.Message{Header: dnsmessage.Header{ID: query.ID, Response: true, RecursionDesired: true, RecursionAvailable: true}, Questions: query.Questions}
}

func TestDNSNamesUseOnlyInterfaceSockets(t *testing.T) {
	for _, mode := range []string{"both", "ipv6-only", "cname-in-answer", "cname-next-query", "unrelated-address", "idna"} {
		t.Run(mode, func(t *testing.T) {
			f := newTestNetwork()
			f.metadata[0].DNS = []string{"192.0.2.53"}
			var mu sync.Mutex
			var names []string
			useDNSResponder(f, func(_, _ string, q dnsmessage.Message) (dnsmessage.Message, error) {
				question := q.Questions[0]
				name := question.Name.String()
				mu.Lock()
				names = append(names, name)
				mu.Unlock()
				r := answerFor(q)
				switch mode {
				case "ipv6-only":
					if question.Type == dnsmessage.TypeAAAA {
						r.Answers = []dnsmessage.Resource{addressRecord(name, question.Type)}
					}
				case "cname-in-answer":
					r.Answers = []dnsmessage.Resource{addressRecord("canonical.example.com.", question.Type), cnameRecord(name, "canonical.example.com.")}
				case "cname-next-query":
					if name == "example.com." {
						r.Answers = []dnsmessage.Resource{cnameRecord(name, "canonical.example.com.")}
					} else {
						r.Answers = []dnsmessage.Resource{addressRecord(name, question.Type)}
					}
				case "unrelated-address":
					r.Answers = []dnsmessage.Resource{addressRecord("unrelated.example.com.", question.Type)}
				default:
					r.Answers = []dnsmessage.Resource{addressRecord(name, question.Type)}
				}
				return r, nil
			})
			host := "example.com"
			if mode == "idna" {
				host = "b\u00fccher.example.com"
			}
			conn, err := New(f.options()).Dial(context.Background(), testRef(), proxy.Target{Host: host, Port: 443, IsDomain: true})
			if mode == "unrelated-address" {
				if err == nil || conn != nil {
					t.Fatal("unrelated DNS answer was used")
				}
			} else {
				if err != nil {
					t.Fatal(err)
				}
				conn.Close()
			}
			if mode == "idna" {
				for _, name := range names {
					if name != "xn--bcher-kva.example.com." {
						t.Fatalf("IDNA query=%q", name)
					}
				}
			}
			if mode == "cname-next-query" && len(names) != 4 {
				t.Fatalf("CNAME queries=%v", names)
			}
			for _, call := range f.dials {
				if call.name != "example0" {
					t.Fatal(call)
				}
				if strings.HasSuffix(call.address, ":53") && call.address != "192.0.2.53:53" {
					t.Fatalf("DNS escaped interface servers: %v", call)
				}
			}
			if mode == "ipv6-only" && f.dials[len(f.dials)-1].network != "tcp6" {
				t.Fatalf("AAAA used wrong family: %v", f.dials)
			}
		})
	}
}

func TestDNSTruncationUsesSameServerAndBinding(t *testing.T) {
	f := newTestNetwork()
	f.metadata[0].DNS = []string{"2001:db8::53"}
	var mu sync.Mutex
	ids := map[dnsmessage.Type]uint16{}
	useDNSResponder(f, func(network, address string, q dnsmessage.Message) (dnsmessage.Message, error) {
		if address != "[2001:db8::53]:53" {
			return dnsmessage.Message{}, errors.New("wrong DNS server")
		}
		r := answerFor(q)
		typ := q.Questions[0].Type
		mu.Lock()
		defer mu.Unlock()
		if network == "udp6" {
			ids[typ] = q.ID
			r.Truncated = true
		} else {
			if network != "tcp6" || ids[typ] != q.ID {
				return dnsmessage.Message{}, errors.New("TCP changed DNS query or transport family")
			}
			r.Answers = []dnsmessage.Resource{addressRecord(q.Questions[0].Name.String(), typ)}
		}
		return r, nil
	})
	conn, err := New(f.options()).Dial(context.Background(), testRef(), proxy.Target{Host: "example.com", Port: 443, IsDomain: true})
	if err != nil {
		t.Fatal(err)
	}
	conn.Close()
	counts := map[string]int{}
	for _, call := range f.dials {
		counts[call.network]++
	}
	if counts["udp6"] != 2 || counts["tcp6"] != 2 || counts["tcp4"] != 1 {
		t.Fatalf("dial sequence=%v", f.dials)
	}
}

func TestDNSRetryStaysOnSelectedInterface(t *testing.T) {
	f := newTestNetwork()
	f.metadata[0].DNS = []string{"192.0.2.53", "198.51.100.53", "203.0.113.53", "192.0.2.54"}
	useDNSResponder(f, func(_, address string, q dnsmessage.Message) (dnsmessage.Message, error) {
		r := answerFor(q)
		if address == "192.0.2.53:53" {
			r.RCode = dnsmessage.RCodeServerFailure
		} else {
			r.Answers = []dnsmessage.Resource{addressRecord(q.Questions[0].Name.String(), q.Questions[0].Type)}
		}
		return r, nil
	})
	conn, err := New(f.options()).Dial(context.Background(), testRef(), proxy.Target{Host: "example.com", Port: 443, IsDomain: true})
	if err != nil {
		t.Fatal(err)
	}
	conn.Close()
	for _, call := range f.dials {
		if call.name != "example0" || call.address == "203.0.113.53:53" || call.address == "192.0.2.54:53" {
			t.Fatalf("unexpected retry=%v", call)
		}
	}
}

func TestDNSResponseValidation(t *testing.T) {
	q := dnsmessage.Question{Name: dnsName("example.com."), Type: dnsmessage.TypeA, Class: dnsmessage.ClassINET}
	base := dnsmessage.Message{Header: dnsmessage.Header{ID: 17, Response: true}, Questions: []dnsmessage.Question{q}, Answers: []dnsmessage.Resource{addressRecord("example.com.", dnsmessage.TypeA)}}
	for _, mode := range []string{"valid", "id", "query", "opcode", "name", "type", "class", "question-count", "rcode", "truncated", "record-limit", "short", "malformed", "oversized"} {
		t.Run(mode, func(t *testing.T) {
			message := base
			message.Questions = append([]dnsmessage.Question(nil), base.Questions...)
			switch mode {
			case "id":
				message.ID++
			case "query":
				message.Response = false
			case "opcode":
				message.OpCode = 1
			case "name":
				message.Questions[0].Name = dnsName("other.example.com.")
			case "type":
				message.Questions[0].Type = dnsmessage.TypeAAAA
			case "class":
				message.Questions[0].Class = dnsmessage.ClassCHAOS
			case "question-count":
				message.Questions = append(message.Questions, q)
			case "rcode":
				message.RCode = dnsmessage.RCodeNameError
			case "truncated":
				message.Truncated = true
			}
			payload, err := message.Pack()
			if err != nil {
				t.Fatal(err)
			}
			switch mode {
			case "record-limit":
				binary.BigEndian.PutUint16(payload[6:8], maxDNSRecords+1)
			case "short":
				payload = payload[:8]
			case "malformed":
				payload = payload[:len(payload)-1]
			case "oversized":
				payload = append(payload, make([]byte, maxDNSBytes)...)
			}
			_, err = parseResponse(payload, 17, q)
			if mode == "valid" {
				if err != nil {
					t.Fatal(err)
				}
			} else if err == nil {
				t.Fatal("malformed reply accepted")
			}
			if mode == "truncated" && !errors.Is(err, errTruncated) {
				t.Fatal(err)
			}
			if mode == "rcode" {
				var dnsErr *net.DNSError
				if !errors.As(err, &dnsErr) || !dnsErr.IsNotFound {
					t.Fatal(err)
				}
			}
		})
	}
}

func TestCNAMEBoundsAndConflicts(t *testing.T) {
	for _, mode := range []string{"cycle", "chain-limit", "conflicting-alias", "address-and-alias"} {
		t.Run(mode, func(t *testing.T) {
			f := newTestNetwork()
			f.metadata[0].DNS = []string{"192.0.2.53"}
			useDNSResponder(f, func(_, _ string, q dnsmessage.Message) (dnsmessage.Message, error) {
				r := answerFor(q)
				question := q.Questions[0]
				name := question.Name.String()
				switch mode {
				case "cycle":
					r.Answers = []dnsmessage.Resource{cnameRecord(name, "alias.example.com."), cnameRecord("alias.example.com.", name)}
				case "chain-limit":
					for i := 0; i < maxCNAMEHops+1; i++ {
						next := fmt.Sprintf("alias%d.example.com.", i)
						r.Answers = append(r.Answers, cnameRecord(name, next))
						name = next
					}
					r.Answers = append(r.Answers, addressRecord(name, question.Type))
				case "conflicting-alias":
					r.Answers = []dnsmessage.Resource{cnameRecord(name, "one.example.com."), cnameRecord(name, "two.example.com.")}
				case "address-and-alias":
					r.Answers = []dnsmessage.Resource{addressRecord(name, question.Type), cnameRecord(name, "alias.example.com.")}
				}
				return r, nil
			})
			conn, err := New(f.options()).Dial(context.Background(), testRef(), proxy.Target{Host: "example.com", Port: 443, IsDomain: true})
			if conn != nil || err == nil {
				t.Fatal("invalid CNAME chain was dialed")
			}
			for _, call := range f.dials {
				if !strings.HasSuffix(call.address, ":53") {
					t.Fatalf("target dialed after invalid CNAME: %v", call)
				}
			}
		})
	}
}

func TestDNSScopeAndServerBound(t *testing.T) {
	iface := testInterface()
	iface.DNS = []string{"::1", "fe80::53%wrong", "fe80::53", "192.0.2.53", "192.0.2.53", "2001:db8::53", "198.51.100.53"}
	servers := dnsServers(iface)
	if len(servers) != 3 || servers[0].String() != "fe80::53%example0" || servers[1].String() != "192.0.2.53" || servers[2].String() != "2001:db8::53" {
		t.Fatalf("servers=%v", servers)
	}
}

func TestDNSRealLoopbackDeadlineWithoutSystemLookup(t *testing.T) {
	server, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatal(err)
	}
	defer server.Close()
	f := newTestNetwork()
	f.metadata[0].DNS = []string{"192.0.2.53"}
	f.dial = func(ctx context.Context, name, network, address string) (net.Conn, error) {
		if name != "example0" || network != "udp4" || address != "192.0.2.53:53" {
			return nil, errors.New("unexpected unbound DNS transport")
		}
		if err := netif.ValidateBinding(ctx, 7); err != nil {
			return nil, err
		}
		var dialer net.Dialer
		return dialer.DialContext(ctx, "udp4", server.LocalAddr().String())
	}
	ctx, cancel := context.WithTimeout(context.Background(), 40*time.Millisecond)
	defer cancel()
	started := time.Now()
	conn, err := New(f.options()).Dial(ctx, testRef(), proxy.Target{Host: "example.com", Port: 443, IsDomain: true})
	if conn != nil || err == nil {
		t.Fatal("unanswered DNS was accepted")
	}
	var dnsErr *net.DNSError
	if !errors.Is(err, context.DeadlineExceeded) && (!errors.As(err, &dnsErr) || !dnsErr.Timeout()) {
		t.Fatalf("timeout error=%v", err)
	}
	if time.Since(started) > time.Second {
		t.Fatal("DNS exceeded caller deadline")
	}
	if len(f.dials) != 2 {
		t.Fatalf("unexpected queries=%v", f.dials)
	}
}

func TestDNSRejectsChangedInterfaceDuringResolution(t *testing.T) {
	f := newTestNetwork()
	f.metadata[0].DNS = []string{"192.0.2.53"}
	useDNSResponder(f, func(_, _ string, q dnsmessage.Message) (dnsmessage.Message, error) {
		f.mu.Lock()
		link := f.links[7]
		link.Name = "unrelated"
		f.links[7] = link
		f.mu.Unlock()
		r := answerFor(q)
		r.Answers = []dnsmessage.Resource{addressRecord(q.Questions[0].Name.String(), q.Questions[0].Type)}
		return r, nil
	})
	conn, err := New(f.options()).Dial(context.Background(), testRef(), proxy.Target{Host: "example.com", Port: 443, IsDomain: true})
	if conn != nil || api.Code(err) != api.ErrInterfaceUnavailable {
		t.Fatalf("DNS binding change: %v %v", conn, err)
	}
}

func TestDNSCandidateLimitIncludesBothFamilies(t *testing.T) {
	f := newTestNetwork()
	f.metadata[0].DNS = []string{"192.0.2.53"}
	useDNSResponder(f, func(_, _ string, q dnsmessage.Message) (dnsmessage.Message, error) {
		r := answerFor(q)
		for i := range maxAddresses + 4 {
			record := addressRecord(q.Questions[0].Name.String(), q.Questions[0].Type)
			if body, ok := record.Body.(*dnsmessage.AResource); ok {
				body.A[3] = byte(i + 1)
			} else {
				record.Body.(*dnsmessage.AAAAResource).AAAA[15] = byte(i + 1)
			}
			r.Answers = append(r.Answers, record)
		}
		return r, nil
	})
	n := New(f.options())
	b, err := n.bind(context.Background(), testRef())
	if err != nil {
		t.Fatal(err)
	}
	addresses, err := n.resolve(context.Background(), b, "example.com")
	if err != nil || len(addresses) != maxAddresses {
		t.Fatalf("addresses=%v error=%v", addresses, err)
	}
	for i, address := range addresses {
		if address.Is4() != (i%2 == 0) {
			t.Fatalf("families not interleaved: %v", addresses)
		}
	}
}

func TestDNSNoAddressFromAuthorityOrAdditional(t *testing.T) {
	f := newTestNetwork()
	useDNSResponder(f, func(_, _ string, q dnsmessage.Message) (dnsmessage.Message, error) {
		r := answerFor(q)
		record := addressRecord(q.Questions[0].Name.String(), q.Questions[0].Type)
		r.Authorities = []dnsmessage.Resource{record}
		r.Additionals = []dnsmessage.Resource{record}
		return r, nil
	})
	conn, err := New(f.options()).Dial(context.Background(), testRef(), proxy.Target{Host: "example.com", Port: 443, IsDomain: true})
	if conn != nil || err == nil {
		t.Fatal("non-answer address was accepted")
	}
}

// dropFirstServer answers DNS on loopback, but the first query, which it
// leaves unanswered as some hotspots do after a quiet while.
func dropFirstServer(t *testing.T) (*net.UDPConn, func() int) {
	t.Helper()
	server, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { server.Close() })
	var mu sync.Mutex
	queries := 0
	go func() {
		buf := make([]byte, 1500)
		for {
			n, from, err := server.ReadFromUDP(buf)
			if err != nil {
				return
			}
			var q dnsmessage.Message
			if q.Unpack(buf[:n]) != nil || len(q.Questions) != 1 {
				continue
			}
			mu.Lock()
			queries++
			first := queries == 1
			mu.Unlock()
			if first {
				continue
			}
			r := answerFor(q)
			r.Answers = []dnsmessage.Resource{addressRecord(q.Questions[0].Name.String(), q.Questions[0].Type)}
			if b, err := r.Pack(); err == nil {
				server.WriteToUDP(b, from)
			}
		}
	}()
	return server, func() int {
		mu.Lock()
		defer mu.Unlock()
		return queries
	}
}

func TestDNSSendsAQueryAgainBeforeGivingUpOnItsServer(t *testing.T) {
	server, queries := dropFirstServer(t)
	f := newTestNetwork()
	f.metadata[0].DNS = []string{"192.0.2.53"}
	f.dial = func(ctx context.Context, name, network, address string) (net.Conn, error) {
		if address != "192.0.2.53:53" {
			return newTestConn(nil), nil
		}
		var dialer net.Dialer
		return dialer.DialContext(ctx, "udp4", server.LocalAddr().String())
	}
	started := time.Now()
	conn, err := New(f.options()).Dial(context.Background(), testRef(), proxy.Target{Host: "example.com", Port: 443, IsDomain: true})
	if err != nil {
		t.Fatal(err)
	}
	conn.Close()
	// One server, so without the second copy the query left unanswered would
	// have failed after dnsAttemptTime.
	if took := time.Since(started); took >= dnsAttemptTime {
		t.Fatalf("resolved after %v", took)
	}
	if n := queries(); n != 3 {
		t.Fatalf("%d queries, want 3: A, AAAA and one sent again", n)
	}
}

func addrs(raw ...string) []netip.Addr {
	var result []netip.Addr
	for _, r := range raw {
		result = append(result, netip.MustParseAddr(r))
	}
	return result
}

func TestSystemDNSReadsTheResolverConfiguration(t *testing.T) {
	got := parseResolvConf([]byte("# Example\nsearch example.com\nnameserver 198.51.100.53\nnameserver  2001:db8::53 \n;nameserver 203.0.113.9\nnameserver resolver.example.com\n"))
	if want := addrs("198.51.100.53", "2001:db8::53"); !slices.Equal(got, want) {
		t.Fatalf("%v, want %v", got, want)
	}
}

func TestFallbackDNS(t *testing.T) {
	ifaces := []api.Interface{testInterface(), {Name: "example1", Up: true, IPv6: []string{"2001:db8:1::2"}}}
	// Private, shared, link-local, a router's public address on example1's
	// network, then public ones.
	system := addrs("10.0.0.53", "100.64.0.53", "fe80::53", "2001:db8:1::1", "198.51.100.53", "::ffff:203.0.113.53", "198.51.100.53", "2001:db8:2::53", "192.0.2.99")
	if got, want := FallbackDNS(ifaces, system), addrs("198.51.100.53", "203.0.113.53", "2001:db8:2::53"); !slices.Equal(got, want) {
		t.Fatalf("%v, want %v", got, want)
	}
	if got := FallbackDNS(ifaces, nil); got != nil {
		t.Fatalf("without servers: %v", got)
	}
}

func TestDNSFallsBackToTheMacsServersThroughTheInterface(t *testing.T) {
	for _, mode := range []string{"refused", "unreachable", "answers", "no-such-name"} {
		t.Run(mode, func(t *testing.T) {
			f := newTestNetwork()
			f.metadata[0].DNS = []string{"192.0.2.53"}
			f.system = addrs("198.51.100.53")
			useDNSResponder(f, func(_, address string, q dnsmessage.Message) (dnsmessage.Message, error) {
				r := answerFor(q)
				if address == "192.0.2.53:53" {
					switch mode {
					case "refused":
						r.RCode = dnsmessage.RCodeRefused
						return r, nil
					case "unreachable":
						return r, errors.New("connection refused")
					case "no-such-name":
						r.RCode = dnsmessage.RCodeNameError
						return r, nil
					}
				}
				r.Answers = []dnsmessage.Resource{addressRecord(q.Questions[0].Name.String(), q.Questions[0].Type)}
				return r, nil
			})
			conn, err := New(f.options()).Dial(context.Background(), testRef(), proxy.Target{Host: "example.com", Port: 443, IsDomain: true})
			var dnsErr *net.DNSError
			if mode == "no-such-name" {
				if conn != nil || !errors.As(err, &dnsErr) || !dnsErr.IsNotFound {
					t.Fatalf("conn=%v err=%v", conn, err)
				}
			} else if err != nil {
				t.Fatal(err)
			} else {
				conn.Close()
			}
			// useDNSResponder refuses any interface but the selected one.
			asked := false
			for _, call := range f.dials {
				asked = asked || call.address == "198.51.100.53:53"
			}
			if want := mode == "refused" || mode == "unreachable"; asked != want {
				t.Fatalf("asked the Mac's server: %v, want %v: %v", asked, want, f.dials)
			}
		})
	}
}

func TestHealthCountsTheMacsDNS(t *testing.T) {
	f := newTestNetwork()
	f.metadata[0].DNS = nil
	f.system = addrs("198.51.100.53")
	if err := New(f.options()).Health(context.Background(), testRef()); err != nil {
		t.Fatal(err)
	}
	f.system = addrs("10.0.0.53")
	if err := New(f.options()).Health(context.Background(), testRef()); api.Code(err) != api.ErrDNSUnavailable {
		t.Fatalf("a private server of another network counted: %v", err)
	}
}
