// Package egress opens strictly interface-bound sockets for local proxies.
// It never uses system name resolution or another interface as fallback.
package egress

import (
	"context"
	"errors"
	"net"
	"net/netip"
	"slices"
	"strconv"
	"sync"
	"time"

	"github.com/Kel077/pith/internal/api"
	"github.com/Kel077/pith/internal/netif"
	"github.com/Kel077/pith/internal/proxy"
)

const (
	maxDialTime  = 10 * time.Second
	metadataTime = time.Second
	maxAddresses = 8
)

// Options supplies discovery and native-socket dependencies. Nil fields use
// read-only discovery and IP_BOUND_IF/IPV6_BOUND_IF. DialContext always receives
// a nonempty interface name and a literal IP endpoint, including DNS requests.
// Dependencies must honor their contexts and be safe for concurrent calls.
type Options struct {
	Interfaces       func(context.Context) ([]api.Interface, error)
	InterfaceByName  func(string) (*net.Interface, error)
	InterfaceByIndex func(int) (*net.Interface, error)
	DialContext      func(context.Context, string, string, string) (net.Conn, error)
	// MetadataTTL is capped at one second. Zero uses that maximum; a negative
	// value disables caching. Native interface identity is never cached.
	MetadataTTL time.Duration
}

// Native is independent of the privileged helper and owns no background worker.
// Its small metadata cache coalesces simultaneous discovery requests, while every
// new socket and relay operation checks the selected native interface lifetime.
type Native struct {
	opts        Options
	mu          sync.Mutex
	metadata    []api.Interface
	metadataErr error
	expires     time.Time
	refresh     chan struct{}
}

func New(opts Options) *Native {
	if opts.Interfaces == nil {
		opts.Interfaces = netif.List
	}
	if opts.InterfaceByName == nil {
		opts.InterfaceByName = net.InterfaceByName
	}
	if opts.InterfaceByIndex == nil {
		opts.InterfaceByIndex = net.InterfaceByIndex
	}
	if opts.DialContext == nil {
		opts.DialContext = func(ctx context.Context, name, network, address string) (net.Conn, error) {
			// All callers have already parsed address as a literal. The resolver
			// attached by netif.Dialer is never entered, even for DNS transport.
			d, err := netif.Dialer(name, 0, nil)
			if err != nil {
				return nil, err
			}
			return d.DialContext(ctx, network, address)
		}
	}
	if opts.MetadataTTL == 0 || opts.MetadataTTL > metadataTime {
		opts.MetadataTTL = metadataTime
	}
	return &Native{opts: opts}
}

func cloneInterfaces(ifaces []api.Interface) []api.Interface {
	result := slices.Clone(ifaces)
	for i := range result {
		result[i].DNS = slices.Clone(result[i].DNS)
		result[i].IPv4 = slices.Clone(result[i].IPv4)
		result[i].IPv6 = slices.Clone(result[i].IPv6)
	}
	return result
}

func (n *Native) interfaces(ctx context.Context) ([]api.Interface, error) {
	for {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		n.mu.Lock()
		if time.Now().Before(n.expires) {
			ifaces, err := cloneInterfaces(n.metadata), n.metadataErr
			n.mu.Unlock()
			return ifaces, err
		}
		if wait := n.refresh; wait != nil {
			n.mu.Unlock()
			select {
			case <-ctx.Done():
				return nil, ctx.Err()
			case <-wait:
				continue
			}
		}
		wait := make(chan struct{})
		n.refresh = wait
		n.mu.Unlock()
		readCtx, cancel := context.WithTimeout(ctx, metadataTime)
		ifaces, err := n.opts.Interfaces(readCtx)
		if err == nil {
			err = readCtx.Err()
		}
		cancel()
		if err != nil {
			ifaces = nil
		}
		n.mu.Lock()
		if ownerErr := ctx.Err(); ownerErr != nil {
			// A canceled CONNECT/Health caller does not own the next caller's
			// discovery result. Wake waiters without caching that caller's error,
			// so a healthy waiter can take over the refresh immediately.
			ifaces, err = nil, ownerErr
		} else {
			// A discovery failure, including this reader's own bounded timeout,
			// may be cached briefly to avoid repeated failing metadata processes.
			n.metadata, n.metadataErr = cloneInterfaces(ifaces), err
			n.expires = time.Now().Add(n.opts.MetadataTTL)
		}
		n.refresh = nil
		close(wait)
		n.mu.Unlock()
		return cloneInterfaces(ifaces), err
	}
}

func unavailable(detail string) error {
	return &api.Error{Code: api.ErrInterfaceUnavailable, Detail: detail}
}

type binding struct {
	native *Native
	iface  api.Interface
	index  int
	mac    string
}

func (n *Native) bind(ctx context.Context, via api.InterfaceRef) (binding, error) {
	if via.Name == "" {
		return binding{}, unavailable("The selected interface reference has no name")
	}
	ifaces, err := n.interfaces(ctx)
	if err != nil {
		if ctx.Err() != nil {
			return binding{}, ctx.Err()
		}
		return binding{}, unavailable("Cannot read the selected interface")
	}
	iface := via.Resolve(ifaces)
	if iface == nil {
		return binding{}, unavailable("The selected physical interface is unavailable")
	}
	if !iface.CanProxy() {
		return binding{}, &api.Error{Code: api.ErrUnsupported, Detail: "The selected interface cannot be a local proxy exit"}
	}
	if !iface.Up {
		return binding{}, unavailable("The selected physical interface is unavailable")
	}
	link, err := n.opts.InterfaceByName(iface.Name)
	if err != nil || link == nil || link.Index <= 0 || link.Name != iface.Name ||
		(iface.HardwareAddr != "" && iface.HardwareAddr != link.HardwareAddr.String()) {
		return binding{}, unavailable("The selected interface identity changed")
	}
	b := binding{native: n, iface: *iface, index: link.Index, mac: link.HardwareAddr.String()}
	if err := b.check(); err != nil {
		return binding{}, err
	}
	return b, nil
}

func (b binding) check() error {
	link, err := b.native.opts.InterfaceByIndex(b.index)
	if err != nil || link == nil || link.Index != b.index || link.Name != b.iface.Name ||
		link.HardwareAddr.String() != b.mac || link.Flags&net.FlagLoopback != 0 ||
		link.Flags&(net.FlagUp|net.FlagRunning) != net.FlagUp|net.FlagRunning {
		return unavailable("The bound physical interface is no longer available")
	}
	return nil
}

// Health reports local availability, not internet connectivity. Literal-IP
// requests can still succeed when Health reports only ErrDNSUnavailable.
func (n *Native) Health(ctx context.Context, via api.InterfaceRef) error {
	b, err := n.bind(ctx, via)
	if err != nil {
		return err
	}
	if len(dnsServers(b.iface)) == 0 {
		return &api.Error{Code: api.ErrDNSUnavailable, Detail: "The selected interface has no usable DNS servers"}
	}
	return nil
}

// Dial connects through exactly one currently resolved physical interface.
// Domain queries and all candidate attempts retain that same native binding.
func (n *Native) Dial(ctx context.Context, via api.InterfaceRef, target proxy.Target) (net.Conn, error) {
	ctx, cancel := context.WithTimeout(ctx, maxDialTime)
	defer cancel()
	if target.Port == 0 {
		return nil, errors.New("invalid proxy destination port")
	}
	b, err := n.bind(ctx, via)
	if err != nil {
		return nil, err
	}
	var addresses []netip.Addr
	if target.IsDomain {
		addresses, err = n.resolve(ctx, b, target.Host)
		if err != nil {
			return nil, err
		}
	} else {
		addr, parseErr := netip.ParseAddr(target.Host)
		if parseErr != nil {
			return nil, errors.New("invalid proxy destination address")
		}
		addr, err = destination(addr, b.iface.Name)
		if err != nil {
			return nil, err
		}
		addresses = []netip.Addr{addr}
	}
	var last error
	for i, address := range addresses {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		// One unreachable answer cannot consume the complete CONNECT budget.
		deadline, _ := ctx.Deadline()
		attempt, done := context.WithTimeout(ctx, time.Until(deadline)/time.Duration(len(addresses)-i))
		conn, err := n.socket(attempt, b, "tcp", netip.AddrPortFrom(address, target.Port))
		done()
		if err == nil {
			return conn, nil
		}
		if api.Code(err) == api.ErrInterfaceUnavailable {
			return nil, err
		}
		last = err
	}
	if last == nil {
		last = &net.DNSError{Err: "no usable address", Name: target.Host, IsNotFound: true}
	}
	return nil, last
}

func destination(addr netip.Addr, name string) (netip.Addr, error) {
	addr = addr.Unmap()
	if !addr.IsValid() || addr.IsUnspecified() || addr.IsLoopback() || addr.IsMulticast() ||
		(addr.Is4() && addr == netip.AddrFrom4([4]byte{255, 255, 255, 255})) {
		return netip.Addr{}, errors.New("destination cannot be sent through a physical interface")
	}
	if addr.Is6() && addr.IsLinkLocalUnicast() {
		if addr.Zone() != "" && addr.Zone() != name {
			return netip.Addr{}, errors.New("destination scope differs from the selected interface")
		}
		return addr.WithZone(name), nil
	}
	if addr.Zone() != "" {
		return netip.Addr{}, errors.New("unexpected destination scope")
	}
	return addr, nil
}

func (n *Native) socket(ctx context.Context, b binding, protocol string, target netip.AddrPort) (net.Conn, error) {
	if !target.IsValid() || target.Port() == 0 || (protocol != "tcp" && protocol != "udp") || b.index <= 0 || b.iface.Name == "" {
		return nil, errors.New("refusing an unbound or nonliteral proxy socket")
	}
	ctx = netif.WithBinding(ctx, b.index, b.check)
	if err := netif.ValidateBinding(ctx, b.index); err != nil {
		return nil, err
	}
	network := protocol + "4"
	if target.Addr().Is6() {
		network = protocol + "6"
	}
	conn, err := n.opts.DialContext(ctx, b.iface.Name, network, net.JoinHostPort(target.Addr().String(), strconv.Itoa(int(target.Port()))))
	if checkErr := netif.ValidateBinding(ctx, b.index); checkErr != nil {
		if conn != nil {
			conn.Close()
		}
		return nil, checkErr
	}
	if err != nil {
		if conn != nil {
			conn.Close()
		}
		return nil, err
	}
	if conn == nil {
		return nil, errors.New("proxy socket dial returned no connection")
	}
	return &boundConn{Conn: conn, binding: b}, nil
}

type boundConn struct {
	net.Conn
	binding binding
}

func (c *boundConn) check() error {
	if err := c.binding.check(); err != nil {
		c.Conn.Close()
		return err
	}
	return nil
}

func (c *boundConn) Read(p []byte) (int, error) {
	if err := c.check(); err != nil {
		return 0, err
	}
	n, err := c.Conn.Read(p)
	if checkErr := c.check(); checkErr != nil {
		return 0, checkErr
	}
	return n, err
}

func (c *boundConn) Write(p []byte) (int, error) {
	if err := c.check(); err != nil {
		return 0, err
	}
	n, err := c.Conn.Write(p)
	if checkErr := c.check(); checkErr != nil {
		return n, checkErr
	}
	return n, err
}

func (c *boundConn) CloseWrite() error {
	if err := c.check(); err != nil {
		return err
	}
	if closer, ok := c.Conn.(interface{ CloseWrite() error }); ok {
		return closer.CloseWrite()
	}
	return c.Conn.Close()
}

func (c *boundConn) CloseRead() error {
	if err := c.check(); err != nil {
		return err
	}
	if closer, ok := c.Conn.(interface{ CloseRead() error }); ok {
		return closer.CloseRead()
	}
	return c.Conn.Close()
}

// SetLinger remains available to force-close an already invalidated socket.
func (c *boundConn) SetLinger(seconds int) error {
	if setter, ok := c.Conn.(interface{ SetLinger(int) error }); ok {
		return setter.SetLinger(seconds)
	}
	return nil
}
