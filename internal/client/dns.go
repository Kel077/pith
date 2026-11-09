package client

import (
	"bytes"
	"context"
	"errors"
	"net"
	"net/netip"
	"sync/atomic"
	"time"
)

// A probe's DNS sends a query again when no answer has come after dnsResend,
// then after twice as long, and so on, and tries the next server after
// dnsTry. Go's resolver alone waits 5 seconds for each answer, while some
// hotspots, the M2000 among them, drop the first query after a while without
// any and answer the next one at once.
const (
	dnsResend = 400 * time.Millisecond
	dnsTry    = 2 * time.Second
)

// probeResolver resolves names through an interface's own DNS servers, on
// sockets dialer binds to the interface. With none, it asks the system's,
// through the interface.
func probeResolver(dialer *net.Dialer, iface string, servers []string) *net.Resolver {
	return resolverFor(dialer, dnsAddresses(iface, servers))
}

// dnsAddresses are the addresses of an interface's DNS servers. Link-local
// ones take the interface as their zone, which the system's list of them
// leaves out: without it, a query can't leave the Mac.
func dnsAddresses(iface string, servers []string) []string {
	var addresses []string
	for _, server := range servers {
		addr, err := netip.ParseAddr(server)
		if err != nil {
			continue
		}
		if addr.Is6() && addr.IsLinkLocalUnicast() && addr.Zone() == "" {
			addr = addr.WithZone(iface)
		}
		addresses = append(addresses, net.JoinHostPort(addr.String(), "53"))
	}
	return addresses
}

// resolverFor resolves names through the servers at addresses, in turn, or
// through those the resolver is given when there are none.
func resolverFor(dialer *net.Dialer, addresses []string) *net.Resolver {
	bound := *dialer
	bound.Resolver = nil // it dials addresses only
	var next atomic.Uint32
	return &net.Resolver{PreferGo: true, Dial: func(ctx context.Context, network, address string) (net.Conn, error) {
		if len(addresses) > 0 {
			address = addresses[int(next.Add(1)-1)%len(addresses)]
		}
		conn, err := bound.DialContext(ctx, network, address)
		if err != nil {
			return nil, err
		}
		if udp, ok := conn.(*net.UDPConn); ok {
			giveUp := time.Now().Add(dnsTry)
			return &resendingConn{UDPConn: udp, giveUp: giveUp, deadline: giveUp}, nil
		}
		return conn, nil
	}}
}

// resendingConn is a resolver's exchange over UDP. It sends the query again
// until an answer comes, on the one socket, so that a late answer to an
// earlier copy still counts, and gives up at giveUp, for the resolver to try
// the next server.
type resendingConn struct {
	*net.UDPConn
	giveUp   time.Time
	deadline time.Time // the resolver's, or giveUp if sooner
	query    []byte
	resend   time.Time // when to send it again
	wait     time.Duration
}

func (c *resendingConn) SetDeadline(t time.Time) error {
	c.deadline = earlier(t, c.giveUp)
	return c.UDPConn.SetWriteDeadline(c.deadline)
}

func (c *resendingConn) SetReadDeadline(t time.Time) error {
	c.deadline = earlier(t, c.giveUp)
	return nil
}

func (c *resendingConn) Write(b []byte) (int, error) {
	c.query = bytes.Clone(b)
	c.wait = dnsResend
	c.resend = time.Now().Add(c.wait)
	return c.UDPConn.Write(b)
}

func (c *resendingConn) Read(b []byte) (int, error) {
	for {
		until := c.deadline
		again := c.query != nil && c.resend.Before(until)
		if again {
			until = c.resend
		}
		if err := c.UDPConn.SetReadDeadline(until); err != nil {
			return 0, err
		}
		n, err := c.UDPConn.Read(b)
		var timeout net.Error
		if !again || !errors.As(err, &timeout) || !timeout.Timeout() {
			return n, err
		}
		if _, err := c.UDPConn.Write(c.query); err != nil {
			return 0, err
		}
		c.wait *= 2
		c.resend = time.Now().Add(c.wait)
	}
}

// earlier is t, or limit if t is later or none.
func earlier(t, limit time.Time) time.Time {
	if t.IsZero() || limit.Before(t) {
		return limit
	}
	return t
}
