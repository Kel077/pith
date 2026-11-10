// Package proxy implements bounded local SOCKS5 CONNECT listeners. It has no
// resolver or default-route dialer: every outbound connection is supplied by Dial.
package proxy

import (
	"context"
	"errors"
	"fmt"
	"net"
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
	return &Manager{opts: opts, byPort: make(map[int]*listener)}, nil
}

// Apply keeps unchanged listeners, closes removed/rebound sessions, and retries
// failed binds. A per-port bind failure is retained in Status, not returned as a
// configuration failure. Label-only changes do not replace a listener.
func (m *Manager) Apply(ctx context.Context, proxies []api.Proxy) error {
	cfg := api.DefaultConfig()
	cfg.Proxies = proxies
	if err := cfg.Validate(); err != nil {
		return err
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
	m.mu.Unlock()
	for _, c := range closing {
		c.close()
	}
	for _, e := range start {
		ln, err := m.opts.Listen(ctx, "tcp4", net.JoinHostPort("127.0.0.1", strconv.Itoa(e.cfg.Port)))
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

func sameProxy(a, b api.Proxy) bool {
	return a.Port == b.Port && a.Enabled == b.Enabled && a.Via.Name == b.Via.Name && a.Via.HardwareAddr == b.Via.HardwareAddr
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
		s := &session{ctx: life, cancel: cancel, in: conn, start: time.Now()}
		e.conns[s] = struct{}{}
		m.total++
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
			if states[i].Listening && states[i].Error == nil {
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
	m.mu.Unlock()
	for _, c := range closing {
		c.close()
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
	return true
}

func (t Target) String() string {
	return net.JoinHostPort(t.Host, fmt.Sprint(t.Port))
}
