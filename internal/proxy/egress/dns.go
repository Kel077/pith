package egress

import (
	"context"
	"crypto/rand"
	"encoding/binary"
	"errors"
	"io"
	"net"
	"net/netip"
	"os"
	"slices"
	"strings"
	"time"

	"github.com/Kel077/pith/internal/api"
	"golang.org/x/net/dns/dnsmessage"
	"golang.org/x/net/idna"
)

const (
	maxDNSServers  = 3
	maxDNSBytes    = 16384
	maxDNSRecords  = 128
	maxCNAMEHops   = 8
	dnsTime        = 4 * time.Second
	dnsAttemptTime = 750 * time.Millisecond
	// An attempt sends its query once more after dnsResendTime: a hotspot
	// can drop the first query after a quiet while and answer the next at
	// once, as the M2000 does.
	dnsResendTime = 400 * time.Millisecond
)

var errTruncated = errors.New("DNS response requires TCP")

// SystemDNS lists the Mac's DNS servers, as its resolver configuration does:
// those the user set, or else the primary interface's.
func SystemDNS() []netip.Addr {
	data, err := os.ReadFile("/etc/resolv.conf")
	if err != nil {
		return nil
	}
	return parseResolvConf(data)
}

func parseResolvConf(data []byte) []netip.Addr {
	var servers []netip.Addr
	for _, line := range strings.Split(string(data), "\n") {
		fields := strings.Fields(line)
		if len(fields) < 2 || fields[0] != "nameserver" {
			continue
		}
		if addr, err := netip.ParseAddr(fields[1]); err == nil {
			servers = append(servers, addr)
		}
	}
	return servers
}

// FallbackDNS lists the DNS servers to ask through an interface when its own
// don't answer, as a hotspot restarting its DNS doesn't: the Mac's (system),
// leaving out those only their own network reaches: private, shared and
// link-local ones, and public IPv6 ones in an interface's /64, as a router's
// own address is. Queries to them still go through the interface.
func FallbackDNS(ifaces []api.Interface, system []netip.Addr) []netip.Addr {
	var result []netip.Addr
	for _, addr := range system {
		addr = addr.Unmap()
		if !public(addr) || onLink(ifaces, addr) || slices.Contains(result, addr) {
			continue
		}
		result = append(result, addr)
		if len(result) == maxDNSServers {
			break
		}
	}
	return result
}

// public tells an address any network reaches: not private, shared,
// link-local, loopback, multicast or unspecified.
func public(addr netip.Addr) bool {
	return addr.Zone() == "" && addr.IsGlobalUnicast() && !addr.IsPrivate() && !sharedAddresses.Contains(addr)
}

// sharedAddresses are carriers' NATs' and VPNs' (RFC 6598), Tailscale's DNS
// among them.
var sharedAddresses = netip.MustParsePrefix("100.64.0.0/10")

// onLink tells an IPv6 address in the /64 of one of the interfaces' own.
func onLink(ifaces []api.Interface, addr netip.Addr) bool {
	if !addr.Is6() {
		return false
	}
	for _, iface := range ifaces {
		for _, raw := range iface.IPv6 {
			own, err := netip.ParseAddr(raw)
			if err != nil {
				continue
			}
			if prefix, err := own.Prefix(64); err == nil && prefix.Contains(addr) {
				return true
			}
		}
	}
	return false
}

// resolvers are the DNS servers to ask through an interface, in turn: its
// own, then the Mac's (FallbackDNS).
func resolvers(ifaces []api.Interface, iface api.Interface, system []netip.Addr) []netip.Addr {
	servers := dnsServers(iface)
	for _, addr := range FallbackDNS(ifaces, system) {
		if !slices.Contains(servers, addr) {
			servers = append(servers, addr)
		}
	}
	return servers
}

func dnsServers(iface api.Interface) []netip.Addr {
	var result []netip.Addr
	seen := make(map[netip.Addr]bool)
	for _, raw := range iface.DNS {
		addr, err := netip.ParseAddr(raw)
		if err != nil {
			continue
		}
		addr, err = destination(addr, iface.Name)
		if err != nil || seen[addr] {
			continue
		}
		seen[addr] = true
		result = append(result, addr)
		if len(result) == maxDNSServers {
			break
		}
	}
	return result
}

func domainName(raw string) (string, error) {
	if raw == "" || len(raw) > 1024 || strings.ContainsAny(raw, "\x00/\\:@[]% \t\r\n") {
		return "", errors.New("invalid proxy destination name")
	}
	name, err := idna.Lookup.ToASCII(strings.TrimSuffix(raw, "."))
	if err != nil || name == "" || len(name) > 253 {
		return "", errors.New("invalid proxy destination name")
	}
	for _, label := range strings.Split(name, ".") {
		if len(label) == 0 || len(label) > 63 || label[0] == '-' || label[len(label)-1] == '-' {
			return "", errors.New("invalid proxy destination name")
		}
		for _, c := range label {
			if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '-') {
				return "", errors.New("invalid proxy destination name")
			}
		}
	}
	return strings.ToLower(name) + ".", nil
}

func (n *Native) resolve(ctx context.Context, b binding, raw string) ([]netip.Addr, error) {
	name, err := domainName(raw)
	if err != nil {
		return nil, err
	}
	servers := b.dns
	if len(servers) == 0 {
		return nil, &api.Error{Code: api.ErrDNSUnavailable, Detail: "The selected interface has no usable DNS servers"}
	}
	ctx, cancel := context.WithTimeout(ctx, dnsTime)
	defer cancel()
	type answer struct {
		addrs []netip.Addr
		err   error
	}
	// Two independent queries avoid making IPv6 depend on an unanswered A query.
	// Both finish before returning, and every attempt shares the same binding.
	results := [2]chan answer{make(chan answer, 1), make(chan answer, 1)}
	for i, typ := range []dnsmessage.Type{dnsmessage.TypeA, dnsmessage.TypeAAAA} {
		go func() {
			addresses, err := n.lookupType(ctx, b, servers, name, typ)
			results[i] <- answer{addresses, err}
		}()
	}
	var answers [2][]netip.Addr
	var failures []error
	for i, result := range results {
		a := <-result
		if a.err != nil {
			failures = append(failures, a.err)
		}
		answers[i] = a.addrs
	}
	if err := b.check(); err != nil {
		return nil, err
	}
	// Interleave families before applying the attempt cap. A large A answer must
	// not exclude every AAAA candidate from an otherwise usable IPv6 interface.
	var all []netip.Addr
	seen := make(map[netip.Addr]bool)
	for i := 0; i < max(len(answers[0]), len(answers[1])) && len(all) < maxAddresses; i++ {
		for _, family := range answers {
			if i >= len(family) {
				continue
			}
			address, err := destination(family[i], b.iface.Name)
			if err == nil && !seen[address] && len(all) < maxAddresses {
				seen[address] = true
				all = append(all, address)
			}
		}
	}
	if len(all) != 0 {
		return all, nil
	}
	if len(failures) > 0 {
		return nil, errors.Join(failures...)
	}
	return nil, &net.DNSError{Err: "no usable address", Name: name, IsNotFound: true}
}

func (n *Native) lookupType(ctx context.Context, b binding, servers []netip.Addr, name string, typ dnsmessage.Type) ([]netip.Addr, error) {
	seen := map[string]bool{name: true}
	hops := 0
	for {
		msg, err := n.query(ctx, b, servers, name, typ)
		if err != nil {
			return nil, err
		}
		for {
			var addresses []netip.Addr
			alias := ""
			for _, record := range msg.Answers {
				if record.Header.Class != dnsmessage.ClassINET || !strings.EqualFold(record.Header.Name.String(), name) {
					continue
				}
				switch body := record.Body.(type) {
				case *dnsmessage.AResource:
					if typ == dnsmessage.TypeA {
						addresses = append(addresses, netip.AddrFrom4(body.A))
					}
				case *dnsmessage.AAAAResource:
					if typ == dnsmessage.TypeAAAA {
						addresses = append(addresses, netip.AddrFrom16(body.AAAA))
					}
				case *dnsmessage.CNAMEResource:
					canonical, err := domainName(body.CNAME.String())
					if err != nil || alias != "" && alias != canonical {
						return nil, &net.DNSError{Err: "invalid CNAME answer", Name: name}
					}
					alias = canonical
				}
			}
			if alias == "" {
				return addresses, nil
			}
			if len(addresses) != 0 || hops == maxCNAMEHops || seen[alias] {
				return nil, &net.DNSError{Err: "conflicting or cyclic CNAME answer", Name: name}
			}
			hops++
			seen[alias] = true
			name = alias
			// Recursive servers commonly include the final address in the same
			// answer. Otherwise query only the selected interface's servers again.
			hasAnswer := false
			for _, record := range msg.Answers {
				if record.Header.Class == dnsmessage.ClassINET && strings.EqualFold(record.Header.Name.String(), name) &&
					(record.Header.Type == typ || record.Header.Type == dnsmessage.TypeCNAME) {
					hasAnswer = true
					break
				}
			}
			if !hasAnswer {
				break
			}
		}
	}
}

func (n *Native) query(ctx context.Context, b binding, servers []netip.Addr, name string, typ dnsmessage.Type) (dnsmessage.Message, error) {
	questionName, err := dnsmessage.NewName(name)
	if err != nil {
		return dnsmessage.Message{}, err
	}
	question := dnsmessage.Question{Name: questionName, Type: typ, Class: dnsmessage.ClassINET}
	var failures []error
	for _, server := range servers {
		if err := ctx.Err(); err != nil {
			return dnsmessage.Message{}, dnsError(name, err)
		}
		var random [2]byte
		if _, err := rand.Read(random[:]); err != nil {
			return dnsmessage.Message{}, err
		}
		id := binary.BigEndian.Uint16(random[:])
		query := dnsmessage.Message{Header: dnsmessage.Header{ID: id, RecursionDesired: true}, Questions: []dnsmessage.Question{question}}
		payload, err := query.Pack()
		if err != nil {
			return dnsmessage.Message{}, err
		}
		attempt, cancel := context.WithTimeout(ctx, dnsAttemptTime)
		response, err := n.exchange(attempt, b, server, "udp", payload)
		var message dnsmessage.Message
		if err == nil {
			message, err = parseResponse(response, id, question)
		}
		if errors.Is(err, errTruncated) {
			response, err = n.exchange(attempt, b, server, "tcp", payload)
			if err == nil {
				message, err = parseResponse(response, id, question)
			}
		}
		cancel()
		if err == nil {
			return message, nil
		}
		var dnsErr *net.DNSError
		if errors.As(err, &dnsErr) && dnsErr.IsNotFound || api.Code(err) == api.ErrInterfaceUnavailable {
			return dnsmessage.Message{}, err
		}
		failures = append(failures, dnsError(name, err))
	}
	return dnsmessage.Message{}, errors.Join(failures...)
}

func dnsError(name string, err error) error {
	var dnserr *net.DNSError
	if errors.As(err, &dnserr) || api.Code(err) == api.ErrInterfaceUnavailable {
		return err
	}
	var timeout net.Error
	return &net.DNSError{Err: err.Error(), Name: name, IsTimeout: errors.Is(err, context.DeadlineExceeded) || errors.As(err, &timeout) && timeout.Timeout()}
}

func (n *Native) exchange(ctx context.Context, b binding, server netip.Addr, protocol string, request []byte) ([]byte, error) {
	conn, err := n.socket(ctx, b, protocol, netip.AddrPortFrom(server, 53))
	if err != nil {
		return nil, err
	}
	defer conn.Close()
	stop := context.AfterFunc(ctx, func() { conn.Close() })
	defer stop()
	deadline, ok := ctx.Deadline()
	if !ok {
		return nil, errors.New("DNS query has no deadline")
	}
	if err := conn.SetDeadline(deadline); err != nil {
		return nil, err
	}
	if protocol == "tcp" {
		framed := make([]byte, 2+len(request))
		binary.BigEndian.PutUint16(framed, uint16(len(request)))
		copy(framed[2:], request)
		if err := writeAll(conn, framed); err != nil {
			return nil, normalizeDeadline(ctx, err)
		}
		var length [2]byte
		if _, err := io.ReadFull(conn, length[:]); err != nil {
			return nil, normalizeDeadline(ctx, err)
		}
		size := int(binary.BigEndian.Uint16(length[:]))
		if size < 12 || size > maxDNSBytes {
			return nil, errors.New("DNS response size is outside the limit")
		}
		response := make([]byte, size)
		if _, err := io.ReadFull(conn, response); err != nil {
			return nil, normalizeDeadline(ctx, err)
		}
		return response, nil
	}
	send := func() error {
		if count, err := conn.Write(request); err != nil {
			return normalizeDeadline(ctx, err)
		} else if count != len(request) {
			return io.ErrShortWrite
		}
		return nil
	}
	if err := send(); err != nil {
		return nil, err
	}
	response := make([]byte, maxDNSBytes+1)
	// Listen until the query goes again, then until the deadline: a late
	// answer to the first copy still counts.
	resend := time.Now().Add(dnsResendTime)
	for {
		again := resend.Before(deadline)
		until := deadline
		if again {
			until = resend
		}
		if err := conn.SetReadDeadline(until); err != nil {
			return nil, err
		}
		size, err := conn.Read(response)
		var timeout net.Error
		if again && errors.As(err, &timeout) && timeout.Timeout() && ctx.Err() == nil {
			if err := send(); err != nil {
				return nil, err
			}
			resend = deadline
			continue
		}
		if err != nil {
			return nil, normalizeDeadline(ctx, err)
		}
		if size > maxDNSBytes {
			return nil, errors.New("DNS response exceeds the size limit")
		}
		return response[:size], nil
	}
}

func normalizeDeadline(ctx context.Context, err error) error {
	if ctx.Err() != nil {
		return ctx.Err()
	}
	return err
}

func writeAll(conn net.Conn, data []byte) error {
	for len(data) > 0 {
		n, err := conn.Write(data)
		if err != nil {
			return err
		}
		if n <= 0 {
			return io.ErrShortWrite
		}
		data = data[n:]
	}
	return nil
}

func parseResponse(payload []byte, id uint16, question dnsmessage.Question) (dnsmessage.Message, error) {
	if len(payload) < 12 || len(payload) > maxDNSBytes {
		return dnsmessage.Message{}, errors.New("invalid DNS message size")
	}
	if binary.BigEndian.Uint16(payload[4:6]) != 1 {
		return dnsmessage.Message{}, errors.New("unexpected DNS question count")
	}
	records := int(binary.BigEndian.Uint16(payload[6:8])) + int(binary.BigEndian.Uint16(payload[8:10])) + int(binary.BigEndian.Uint16(payload[10:12]))
	if records > maxDNSRecords {
		return dnsmessage.Message{}, errors.New("DNS response has too many records")
	}
	var parser dnsmessage.Parser
	header, err := parser.Start(payload)
	if err != nil || header.ID != id || !header.Response || header.OpCode != 0 {
		return dnsmessage.Message{}, errors.New("DNS response does not match the query")
	}
	got, err := parser.Question()
	if err != nil || got.Type != question.Type || got.Class != question.Class || !strings.EqualFold(got.Name.String(), question.Name.String()) {
		return dnsmessage.Message{}, errors.New("DNS response question does not match")
	}
	if header.Truncated {
		return dnsmessage.Message{}, errTruncated
	}
	if header.RCode != dnsmessage.RCodeSuccess {
		return dnsmessage.Message{}, &net.DNSError{Err: "DNS server returned " + header.RCode.String(), Name: question.Name.String(), IsNotFound: header.RCode == dnsmessage.RCodeNameError}
	}
	var message dnsmessage.Message
	if err := message.Unpack(payload); err != nil {
		return dnsmessage.Message{}, err
	}
	return message, nil
}
