// Package proxy implements bounded local SOCKS5 CONNECT listeners. It has no
// resolver, and no default-route dialer but to reach a port's upstream proxy:
// every other outbound connection is supplied by Dial.
package proxy

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/netip"
	"slices"
	"strconv"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/Kel077/pith/internal/api"
)

// Target is a SOCKS destination. Domain names remain names until Dial resolves
// them through the selected interface; the protocol never uses net.DefaultResolver.
type Target struct {
	Host     string
	Port     uint16
	IsDomain bool
}

// DialFunc must honor cancellation and return only a connection through Via.
// A wrapper around a TCP connection should preserve CloseWrite for half-close.
type DialFunc func(context.Context, api.InterfaceRef, Target) (net.Conn, error)

// Options provides the strict outbound implementation and injectable loopback
// listener. Zero limits use the production defaults; positive limits may lower
// the connection caps for a caller or a test.
type Options struct {
	Dial   DialFunc
	Health func(context.Context, api.InterfaceRef) error
	Listen func(context.Context, string, string) (net.Listener, error)
	// Upstream opens the TCP connection to a port's upstream proxy, by its
	// "host:port". Nil connects as macOS routes and resolves names.
	Upstream func(ctx context.Context, network, address string) (net.Conn, error)

	HandshakeTimeout time.Duration
	ConnectTimeout   time.Duration
	MaxPerPort       int
	MaxTotal         int
}

// A browser opens a stream for every host a page loads from and keeps idle
// ones for reuse: a Chromium-based browser held about 120 through one port for
// a single news page. Each stream holds two sockets.
const (
	defaultHandshakeTimeout = 5 * time.Second
	defaultConnectTimeout   = 10 * time.Second
	defaultMaxPerPort       = 1024
	defaultMaxTotal         = 2048
	// A refused device is told so, a few at a time and briefly: one that
	// isn't listed can't hold the slots of those that are.
	maxRefusing   = 8
	refuseTimeout = 2 * time.Second
	// An upstream proxy has a few seconds to take a connection, of the ten a
	// CONNECT has. One that carried none for a while is asked whether it
	// answers, when Status is read.
	upstreamAnswer     = 3 * time.Second
	upstreamCheckEvery = 10 * time.Second
)

// Manager owns all listeners and workers for one App client. Apply and Close
// serialize configuration changes; established streams do not hold that lock.
type Manager struct {
	opts Options

	applyMu sync.Mutex
	mu      sync.Mutex
	closed  bool
	entries []*listener
	byPort  map[int]*listener
	total   int
	wg      sync.WaitGroup
	// suspended tells the exits whose listeners refuse connections for now.
	suspended func(api.InterfaceRef) bool
	// allow is the other devices that may use shared listeners; devices, the
	// ones that connected; refusing, the ones being told they may not.
	allow    []netip.Prefix
	devices  map[netip.Addr]*device
	refusing map[net.Conn]struct{}
}

// device is another device that connected to a shared listener. Manager.mu
// guards all but its byte counters.
type device struct {
	addr     netip.Addr
	allowed  bool
	seen     time.Time
	conns    int // its sessions, handshakes included
	open     int // its established streams
	sent     atomic.Uint64
	received atomic.Uint64
}

type listener struct {
	cfg      api.Proxy
	ctx      context.Context
	cancel   context.CancelFunc
	ln       net.Listener
	since    time.Time
	err      *api.Error
	retired  bool
	conns    map[*session]struct{}
	open     int
	sent     atomic.Uint64
	received atomic.Uint64
	// up tells how the upstream proxy last answered, a connection or a check:
	// guarded by Manager.mu.
	up struct {
		checking bool
		checked  time.Time
		err      *api.Error
	}
}

type session struct {
	ctx    context.Context
	cancel context.CancelFunc
	in     net.Conn
	start  time.Time
	mu     sync.Mutex
	out    net.Conn
	closed bool
	active bool // protected by Manager.mu
	// peer is the other device the stream is for, and dev what it moved;
	// neither is set for this Mac's own.
	peer netip.Addr
	dev  *device
	// last is when the stream last moved data, on clock. writing counts its
	// writes in progress: a client that stops reading stalls one for long.
	last    atomic.Int64
	writing atomic.Int32
}

// epoch starts the monotonic clock that tells which stream is quietest.
var epoch = time.Now()

func clock() int64 { return int64(time.Since(epoch)) }

// New creates an idle manager. It does not bind a port or start a worker.
func New(opts Options) (*Manager, error) {
	if opts.Dial == nil {
		return nil, errors.New("proxy: an interface-bound dialer is required")
	}
	if opts.HandshakeTimeout < 0 || opts.ConnectTimeout < 0 || opts.MaxPerPort < 0 || opts.MaxTotal < 0 || opts.MaxPerPort > defaultMaxPerPort || opts.MaxTotal > defaultMaxTotal {
		return nil, errors.New("proxy: invalid timeout or connection limit")
	}
	if opts.HandshakeTimeout == 0 {
		opts.HandshakeTimeout = defaultHandshakeTimeout
	}
	if opts.ConnectTimeout == 0 {
		opts.ConnectTimeout = defaultConnectTimeout
	}
	if opts.MaxPerPort == 0 {
		opts.MaxPerPort = defaultMaxPerPort
	}
	if opts.MaxTotal == 0 {
		opts.MaxTotal = defaultMaxTotal
	}
	if opts.Listen == nil {
		opts.Listen = (&net.ListenConfig{}).Listen
	}
	if opts.Upstream == nil {
		opts.Upstream = (&net.Dialer{}).DialContext
	}
	return &Manager{opts: opts, byPort: make(map[int]*listener), devices: make(map[netip.Addr]*device), refusing: make(map[net.Conn]struct{})}, nil
}

// Apply keeps unchanged listeners, closes removed/rebound sessions, and retries
// failed binds. A per-port bind failure is retained in Status, not returned as a
// configuration failure. Label-only changes do not replace a listener. A shared
// listener takes the connections of devices too; a device no longer among
// them loses its streams.
func (m *Manager) Apply(ctx context.Context, proxies []api.Proxy, devices []api.Device) error {
	cfg := api.DefaultConfig()
	cfg.Proxies, cfg.Devices = proxies, devices
	if err := cfg.Validate(); err != nil {
		return err
	}
	allow := make([]netip.Prefix, 0, len(devices))
	for _, d := range devices {
		prefix, err := d.Prefix()
		if err != nil {
			return err
		}
		allow = append(allow, prefix)
	}
	m.applyMu.Lock()
	defer m.applyMu.Unlock()
	if err := ctx.Err(); err != nil {
		return err
	}
	m.mu.Lock()
	if m.closed {
		m.mu.Unlock()
		return net.ErrClosed
	}
	next := make([]*listener, 0, len(proxies))
	byPort := make(map[int]*listener, len(proxies))
	var start []*listener
	for _, p := range proxies {
		e := m.byPort[p.Port]
		if e != nil && !e.retired && sameProxy(e.cfg, p) && (!p.Enabled || e.ln != nil) {
			// Label is presentation-only; keep the worker's immutable reference.
		} else {
			life, cancel := context.WithCancel(context.Background())
			e = &listener{cfg: p, ctx: life, cancel: cancel, conns: make(map[*session]struct{})}
			if p.Enabled {
				start = append(start, e)
			}
		}
		next = append(next, e)
		byPort[p.Port] = e
	}
	var closing []closure
	for _, e := range m.entries {
		if byPort[e.cfg.Port] != e {
			closing = append(closing, m.retireLocked(e))
		}
	}
	m.entries, m.byPort = next, byPort
	unlisted := m.allowLocked(allow)
	m.mu.Unlock()
	for _, c := range closing {
		c.close()
	}
	for _, s := range unlisted {
		s.close(true)
	}
	for _, e := range start {
		// A shared listener takes every IPv4 address of this Mac: accept tells
		// which device a connection comes from.
		host := "127.0.0.1"
		if e.cfg.Shared {
			host = "0.0.0.0"
		}
		ln, err := m.opts.Listen(ctx, "tcp4", net.JoinHostPort(host, strconv.Itoa(e.cfg.Port)))
		m.mu.Lock()
		if err != nil {
			e.err = listenerError(err)
			m.mu.Unlock()
			if ln != nil {
				_ = ln.Close()
			}
			continue
		}
		if ln == nil {
			e.err = &api.Error{Code: api.ErrInternal, Detail: "local proxy listener was not created"}
			m.mu.Unlock()
			continue
		}
		e.ln, e.since = ln, time.Now().UTC()
		m.wg.Add(1)
		m.mu.Unlock()
		go m.accept(e, ln)
	}
	return nil
}

// errSuspended is why a suspended listener refuses a connection.
var errSuspended = &api.Error{Code: api.ErrInterfaceUnavailable, Detail: "the interface is offline while its router renews its address"}

// Suspend has the listeners whose exit match tells refuse connections, as
// when their interface is down, and closes their streams, until Resume. Other
// listeners go on, as those that go out through an upstream proxy do;
// nothing is unbound.
func (m *Manager) Suspend(match func(api.InterfaceRef) bool) {
	m.mu.Lock()
	m.suspended = match
	var closing []*session
	for _, e := range m.entries {
		if e.cfg.Enabled && e.cfg.Upstream == "" && match(e.cfg.Via) {
			for s := range e.conns {
				closing = append(closing, s)
			}
		}
	}
	m.mu.Unlock()
	for _, s := range closing {
		s.close(true)
	}
}

// Resume has the listeners Suspend stopped take connections again.
func (m *Manager) Resume() {
	m.mu.Lock()
	m.suspended = nil
	m.mu.Unlock()
}

// isSuspended tells whether connections through via are refused for now.
func (m *Manager) isSuspended(via api.InterfaceRef) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.suspended != nil && m.suspended(via)
}

func sameProxy(a, b api.Proxy) bool {
	return a.Port == b.Port && a.Enabled == b.Enabled && a.Via.Name == b.Via.Name && a.Via.HardwareAddr == b.Via.HardwareAddr &&
		a.Upstream == b.Upstream && a.Shared == b.Shared
}

// allowLocked makes allow the devices that may use shared listeners. It
// forgets the devices that may no longer, and every device once nothing is
// shared, and returns their streams for the caller to close.
func (m *Manager) allowLocked(allow []netip.Prefix) []*session {
	m.allow = allow
	shared := false
	for _, e := range m.entries {
		shared = shared || e.cfg.Enabled && e.cfg.Shared
	}
	var unlisted []*session
	for _, e := range m.entries {
		for s := range e.conns {
			if s.peer.IsValid() && !(shared && m.allowedLocked(s.peer)) {
				unlisted = append(unlisted, s)
			}
		}
	}
	for addr, d := range m.devices {
		switch {
		case !shared, d.allowed && !m.allowedLocked(addr):
			delete(m.devices, addr)
		default:
			d.allowed = m.allowedLocked(addr)
		}
	}
	return unlisted
}

func (m *Manager) allowedLocked(peer netip.Addr) bool {
	for _, prefix := range m.allow {
		if prefix.Contains(peer) {
			return true
		}
	}
	return false
}

// peerOf tells which other device conn comes from: none, and local, for a
// connection of this Mac's own, from a loopback address or from the very
// address it connected to.
func peerOf(conn net.Conn) (peer netip.Addr, local bool) {
	remote, ok := conn.RemoteAddr().(*net.TCPAddr)
	if !ok {
		return netip.Addr{}, false
	}
	peer, ok = netip.AddrFromSlice(remote.IP)
	if !ok {
		return netip.Addr{}, false
	}
	peer = peer.Unmap()
	if peer.IsLoopback() {
		return netip.Addr{}, true
	}
	if own, ok := conn.LocalAddr().(*net.TCPAddr); ok && own.IP.Equal(remote.IP) {
		return netip.Addr{}, true
	}
	return peer, false
}

// deviceLocked notes that peer connected just now, allowed or not. It returns
// nil when the devices it remembers are all connected and peer isn't one.
func (m *Manager) deviceLocked(peer netip.Addr, allowed bool) *device {
	d := m.devices[peer]
	if d == nil {
		if len(m.devices) >= api.MaxDevices && !m.forgetDeviceLocked() {
			return nil
		}
		d = &device{addr: peer}
		m.devices[peer] = d
	}
	d.allowed, d.seen = allowed, time.Now().UTC()
	return d
}

// forgetDeviceLocked makes room for a device: it forgets the one that
// connected the longest ago and has no connection now, a refused one first.
func (m *Manager) forgetDeviceLocked() bool {
	var oldest *device
	for _, d := range m.devices {
		if d.conns != 0 {
			continue
		}
		if oldest == nil || d.allowed != oldest.allowed && !d.allowed || d.allowed == oldest.allowed && d.seen.Before(oldest.seen) {
			oldest = d
		}
	}
	if oldest == nil {
		return false
	}
	delete(m.devices, oldest.addr)
	return true
}

// Devices lists the other devices that connected to shared listeners, the
// latest first.
func (m *Manager) Devices() []api.DeviceStatus {
	m.mu.Lock()
	defer m.mu.Unlock()
	if len(m.devices) == 0 {
		return nil
	}
	out := make([]api.DeviceStatus, 0, len(m.devices))
	for _, d := range m.devices {
		out = append(out, api.DeviceStatus{Address: d.addr.String(), Allowed: d.allowed, Seen: d.seen, Open: d.open, Sent: d.sent.Load(), Received: d.received.Load()})
	}
	slices.SortFunc(out, func(a, b api.DeviceStatus) int {
		return cmp.Or(b.Seen.Compare(a.Seen), cmp.Compare(a.Address, b.Address))
	})
	return out
}

// refuse tells a device that isn't listed that no way of signing in is
// accepted, as RFC 1928 has a proxy refuse, so that it can tell a proxy that
// refuses it from one that is gone.
func (m *Manager) refuse(conn net.Conn) {
	defer m.wg.Done()
	defer func() {
		m.mu.Lock()
		delete(m.refusing, conn)
		m.mu.Unlock()
		_ = conn.Close()
	}()
	if conn.SetDeadline(time.Now().Add(refuseTimeout)) != nil {
		return
	}
	var header [2]byte
	if _, err := io.ReadFull(conn, header[:]); err != nil || header[0] != 5 {
		return
	}
	var methods [255]byte
	if _, err := io.ReadFull(conn, methods[:int(header[1])]); err != nil {
		return
	}
	_ = writeAll(conn, []byte{5, 0xff})
}

type closure struct {
	ln     net.Listener
	cancel context.CancelFunc
	conns  []*session
}

// retireLocked prevents accepts and late dial publication before any socket is
// closed. No network operation runs while holding Manager.mu.
func (m *Manager) retireLocked(e *listener) closure {
	e.retired = true
	c := closure{ln: e.ln, cancel: e.cancel}
	e.ln = nil
	for s := range e.conns {
		c.conns = append(c.conns, s)
	}
	return c
}

func (c closure) close() {
	c.cancel()
	if c.ln != nil {
		_ = c.ln.Close()
	}
	for _, s := range c.conns {
		s.close(true)
	}
}

func (m *Manager) accept(e *listener, ln net.Listener) {
	defer m.wg.Done()
	var backoff time.Duration
	for {
		conn, err := ln.Accept()
		if err != nil {
			if e.ctx.Err() != nil {
				return
			}
			if temporary, ok := err.(interface{ Temporary() bool }); ok && temporary.Temporary() {
				if backoff == 0 {
					backoff = 5 * time.Millisecond
				} else {
					backoff = min(backoff*2, time.Second)
				}
				timer := time.NewTimer(backoff)
				select {
				case <-e.ctx.Done():
					timer.Stop()
					return
				case <-timer.C:
				}
				continue
			}
			m.mu.Lock()
			if m.byPort[e.cfg.Port] == e && !e.retired {
				e.err = listenerError(err)
				c := m.retireLocked(e)
				m.mu.Unlock()
				c.close()
			} else {
				m.mu.Unlock()
			}
			return
		}
		backoff = 0
		m.mu.Lock()
		if m.closed || e.retired || m.byPort[e.cfg.Port] != e {
			m.mu.Unlock()
			_ = conn.Close()
			continue
		}
		// A listener that isn't shared is bound to 127.0.0.1, which only this
		// Mac reaches. A shared one takes this Mac's connections and those of
		// the devices listed; any other device is told it may not, and holds
		// no slot.
		var peer netip.Addr
		var dev *device
		if e.cfg.Shared {
			var local bool
			if peer, local = peerOf(conn); !local {
				allowed := peer.IsValid() && m.allowedLocked(peer)
				if peer.IsValid() {
					dev = m.deviceLocked(peer, allowed)
				}
				if !allowed {
					telling := len(m.refusing) < maxRefusing
					if telling {
						m.refusing[conn] = struct{}{}
						m.wg.Add(1)
					}
					m.mu.Unlock()
					if telling {
						go m.refuse(conn)
					} else {
						closeConn(conn, true)
					}
					continue
				}
			}
		}
		// A browser keeps idle streams for reuse and opens one again when it
		// finds it closed, while a refused connection fails its request. So a
		// full port, or a full manager, closes its quietest stream for this one.
		var quietest *session
		if full := len(e.conns) >= m.opts.MaxPerPort; full || m.total >= m.opts.MaxTotal {
			if quietest = m.quietestLocked(e, full); quietest == nil {
				m.mu.Unlock()
				_ = conn.Close()
				continue
			}
		}
		life, cancel := context.WithCancel(e.ctx)
		s := &session{ctx: life, cancel: cancel, in: conn, start: time.Now(), peer: peer, dev: dev}
		e.conns[s] = struct{}{}
		m.total++
		if dev != nil {
			dev.conns++
		}
		m.wg.Add(1)
		m.mu.Unlock()
		if quietest != nil {
			quietest.close(false)
		}
		go m.serve(e, s)
	}
}

// Status takes a coherent listener snapshot. Health is read-only diagnostics and
// never gates a literal-IP CONNECT merely because the interface has no DNS.
func (m *Manager) Status(ctx context.Context) []api.ProxyStatus {
	m.mu.Lock()
	states := make([]api.ProxyStatus, len(m.entries))
	refs := make([]api.InterfaceRef, len(m.entries))
	for i, e := range m.entries {
		st := api.ProxyStatus{Port: e.cfg.Port}
		if e.cfg.Enabled {
			st.Listening, st.Since = e.ln != nil && !e.retired, e.since
			st.Open, st.Sent, st.Received = e.open, e.sent.Load(), e.received.Load()
			if e.err != nil {
				copy := *e.err
				st.Error = &copy
			} else if e.cfg.Upstream != "" {
				if st.Listening {
					if e.up.err != nil {
						copy := *e.up.err
						st.Error = &copy
					}
					if !m.closed && !e.up.checking && time.Since(e.up.checked) >= upstreamCheckEvery {
						e.up.checking = true
						m.wg.Add(1)
						go m.checkUpstream(e)
					}
				}
			} else if m.suspended != nil && m.suspended(e.cfg.Via) {
				copy := *errSuspended
				st.Error = &copy
			}
			refs[i] = e.cfg.Via
		}
		states[i] = st
	}
	m.mu.Unlock()
	if m.opts.Health != nil {
		ctx, cancel := context.WithTimeout(ctx, 2*time.Second)
		defer cancel()
		for i := range states {
			if states[i].Listening && states[i].Error == nil && refs[i].Name != "" {
				if err := m.opts.Health(ctx, refs[i]); err != nil {
					states[i].Error = healthError(err)
				}
			}
		}
	}
	return states
}

// Close stops accepting, cancels pending dials, closes all streams and joins all
// manager workers. It is idempotent and does not permit a subsequent Apply.
func (m *Manager) Close() error {
	m.applyMu.Lock()
	defer m.applyMu.Unlock()
	m.mu.Lock()
	if m.closed {
		m.mu.Unlock()
		return nil
	}
	m.closed = true
	closing := make([]closure, 0, len(m.entries))
	for _, e := range m.entries {
		closing = append(closing, m.retireLocked(e))
	}
	refusing := make([]net.Conn, 0, len(m.refusing))
	for conn := range m.refusing {
		refusing = append(refusing, conn)
	}
	m.mu.Unlock()
	for _, c := range closing {
		c.close()
	}
	for _, conn := range refusing {
		closeConn(conn, true)
	}
	m.wg.Wait()
	return nil
}

func listenerError(err error) *api.Error {
	if errors.Is(err, syscall.EADDRINUSE) {
		return &api.Error{Code: api.ErrPortInUse, Detail: "local proxy port is already in use"}
	}
	return &api.Error{Code: api.ErrInternal, Detail: "local proxy listener is unavailable"}
}

func healthError(err error) *api.Error {
	var problem *api.Error
	if errors.As(err, &problem) {
		copy := *problem
		return &copy
	}
	return &api.Error{Code: api.ErrInternal, Detail: "local proxy exit availability could not be checked"}
}

func (s *session) setOutbound(out net.Conn) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed || s.ctx.Err() != nil {
		return false
	}
	s.out = out
	return true
}

func (s *session) close(reset bool) {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return
	}
	s.closed = true
	out := s.out
	s.cancel()
	s.mu.Unlock()
	closeConn(s.in, reset)
	if out != nil {
		closeConn(out, reset)
	}
}

func closeConn(conn net.Conn, reset bool) {
	if reset {
		if tcp, ok := conn.(interface{ SetLinger(int) error }); ok {
			_ = tcp.SetLinger(0)
		}
	}
	_ = conn.Close()
}

func (m *Manager) finish(e *listener, s *session) {
	s.close(false)
	m.mu.Lock()
	m.releaseLocked(e, s)
	m.mu.Unlock()
	m.wg.Done()
}

// releaseLocked frees the slot of s once: a stream that made room for a new
// one gave its slot up before its worker finishes.
func (m *Manager) releaseLocked(e *listener, s *session) {
	if _, ok := e.conns[s]; !ok {
		return
	}
	delete(e.conns, s)
	m.total--
	if s.active {
		e.open--
	}
	if s.dev != nil {
		s.dev.conns--
		if s.active {
			s.dev.open--
		}
	}
}

// quietestLocked releases and returns the established stream that has moved
// no data the longest, of e alone when e is full, else of every port. Streams
// still being set up and streams writing are left alone; nil means none is
// left. The caller closes the stream once Manager.mu is released.
func (m *Manager) quietestLocked(e *listener, full bool) *session {
	var owner *listener
	var quietest *session
	var since int64
	consider := func(l *listener) {
		for s := range l.conns {
			if !s.active || s.writing.Load() != 0 {
				continue
			}
			if last := s.last.Load(); quietest == nil || last < since {
				owner, quietest, since = l, s, last
			}
		}
	}
	if full {
		consider(e)
	} else {
		for _, l := range m.entries {
			consider(l)
		}
	}
	if quietest != nil {
		m.releaseLocked(owner, quietest)
	}
	return quietest
}

func (m *Manager) activate(e *listener, s *session) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.closed || e.retired || m.byPort[e.cfg.Port] != e || s.ctx.Err() != nil {
		return false
	}
	s.active = true
	s.last.Store(clock())
	e.open++
	if s.dev != nil {
		s.dev.open++
	}
	return true
}

func (t Target) String() string {
	return net.JoinHostPort(t.Host, fmt.Sprint(t.Port))
}
