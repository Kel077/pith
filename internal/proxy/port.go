package proxy

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"io"
	"math"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/Kel077/pith/internal/api"
)

const (
	// Address changes may take minutes; their lower cap leaves room for
	// status reads and requests that can be answered without waiting.
	maxPortRequests   = 64
	maxChangeRequests = 48
	maxChangeBody     = 4 << 10
	changeIPTimeout   = 10 * time.Minute
)

var unsupportedChange = &api.Error{Code: api.ErrUnsupported, Detail: "the public address of this proxy's exit cannot be changed"}

// portFailure is an endpoint's HTTP status and its public error.
type portFailure struct {
	status  int
	problem *api.Error
}

func (f *portFailure) Error() string { return f.problem.Error() }

func portProblem(status int, code api.ErrorCode, detail string) *portFailure {
	return &portFailure{status: status, problem: &api.Error{Code: code, Detail: detail}}
}

// markStream leaves unclassified HTTP handshakes alone during Suspend: only
// after their target is known can they be told from requests to the port.
func (m *Manager) markStream(s *session) {
	m.mu.Lock()
	s.stream = true
	m.mu.Unlock()
}

// takeRequest moves a handshake out of the stream slots. Requests to a port
// have their own cap, and neither displace streams nor count as open ones.
func (m *Manager) takeRequest(e *listener, s *session) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.closed || e.retired || s.ctx.Err() != nil || m.requests >= maxPortRequests {
		return false
	}
	if _, ok := e.conns[s]; !ok {
		return false
	}
	delete(e.conns, s)
	m.total--
	e.requests[s] = struct{}{}
	m.requests++
	return true
}

// takeChangeRequest counts a POST only after its immediate checks pass.
// Its waiting slot is released with the request, including on cancellation.
func (m *Manager) takeChangeRequest(e *listener, s *session) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.closed || e.retired || s.ctx.Err() != nil || m.changeRequests >= maxChangeRequests {
		return false
	}
	if _, ok := e.requests[s]; !ok {
		return false
	}
	s.waitingChange = true
	m.changeRequests++
	return true
}

func (m *Manager) servePort(e *listener, s *session, req *http.Request, br *bufio.Reader, connectDeadline time.Time) {
	// Presence matters, including an empty Origin. Repeated fields cannot
	// hide a page's fetch behind an earlier "none".
	_, origin := req.Header["Origin"]
	page := origin
	for _, site := range req.Header.Values("Sec-Fetch-Site") {
		page = page || site != "none"
	}
	if page {
		writePortError(s.in, portProblem(http.StatusForbidden, api.ErrNotAllowed, "web pages cannot ask a proxy's port"))
		return
	}
	if !portHost(req.Host) {
		writePortError(s.in, portProblem(http.StatusForbidden, api.ErrNotAllowed, "the Host must name an address, localhost or a local network name"))
		return
	}
	if !m.takeRequest(e, s) {
		writePortError(s.in, portProblem(http.StatusServiceUnavailable, api.ErrInternal, "too many requests to proxy ports"))
		return
	}
	var method string
	switch req.URL.Path {
	case "/":
		method = http.MethodGet
	case "/change-ip":
		method = http.MethodPost
	default:
		writePortError(s.in, portProblem(http.StatusNotFound, api.ErrUnsupported, "no such proxy endpoint"))
		return
	}
	if req.Method != method {
		writePortJSON(s.in, http.StatusMethodNotAllowed, errorBody{&api.Error{Code: api.ErrUnsupported, Detail: "method not supported by this proxy endpoint"}}, method)
		return
	}
	maxAge, valid := requestMaxAge(req)
	hops, validHops := requestHops(req)
	if !valid || !validHops {
		writePortError(s.in, portProblem(http.StatusBadRequest, api.ErrInvalidConfig, "invalid max-age or Pith-Hops"))
		return
	}
	deadline := connectDeadline
	if method == http.MethodPost {
		if !e.allowChangeIP.Load() {
			writePortError(s.in, portProblem(http.StatusForbidden, api.ErrNotAllowed, "this proxy port does not allow address changes"))
			return
		}
	}
	if s.in.SetDeadline(deadline) != nil {
		return
	}
	if method == http.MethodPost {
		if req.ContentLength > maxChangeBody {
			writePortError(s.in, portProblem(http.StatusRequestEntityTooLarge, api.ErrInvalidConfig, "the request body exceeds 4 KiB"))
			return
		}
		n, err := io.Copy(io.Discard, io.LimitReader(req.Body, maxChangeBody+1))
		if n > maxChangeBody {
			writePortError(s.in, portProblem(http.StatusRequestEntityTooLarge, api.ErrInvalidConfig, "the request body exceeds 4 KiB"))
			return
		}
		if err != nil {
			writePortError(s.in, portProblem(http.StatusBadRequest, api.ErrInvalidConfig, "the request body is incomplete"))
			return
		}
		if !m.takeChangeRequest(e, s) {
			writePortError(s.in, portProblem(http.StatusServiceUnavailable, api.ErrInternal, "too many address change requests"))
			return
		}
		deadline = time.Now().Add(changeIPTimeout)
		if s.in.SetDeadline(deadline) != nil {
			return
		}
	}
	workDeadline := deadline
	if method == http.MethodGet {
		// Leave a little of the socket's deadline to send the known state
		// when an interface lookup or an upstream check runs out of time.
		workDeadline = deadline.Add(-min(100*time.Millisecond, m.opts.ConnectTimeout/10))
	}
	ctx, cancel := context.WithDeadline(s.ctx, workDeadline)
	defer cancel()
	// Nothing else is served on the connection. Notice a departed caller
	// while a hook waits, without canceling the change the client owns.
	gone := make(chan struct{})
	go func() {
		defer close(gone)
		_, _ = io.Copy(io.Discard, br)
		cancel()
	}()
	defer func() {
		_ = s.in.SetReadDeadline(time.Unix(1, 0))
		<-gone
	}()
	if method == http.MethodGet {
		info, failure := m.portInfo(ctx, e, maxAge, hops)
		if failure != nil {
			writePortError(s.in, failure)
		} else {
			writePortJSON(s.in, http.StatusOK, info, "")
		}
		return
	}
	result, failure := m.changeIP(ctx, e, hops)
	if failure != nil {
		writePortError(s.in, failure)
	} else {
		writePortJSON(s.in, http.StatusOK, result, "")
	}
}

// portHost excludes public DNS names, which a web page can rebind to this
// Mac. Empty is an HTTP/1.0 caller; local names need no public DNS authority.
func portHost(host string) bool {
	if host == "" {
		return true
	}
	if name, _, err := net.SplitHostPort(host); err == nil {
		host = name
	}
	host = strings.ToLower(strings.TrimSuffix(host, "."))
	if strings.HasPrefix(host, "[") && strings.HasSuffix(host, "]") {
		host = host[1 : len(host)-1]
	}
	if _, err := netip.ParseAddr(host); err == nil {
		return true
	}
	return host != "" && !strings.ContainsAny(host, ":[]") &&
		(host == "localhost" || strings.HasSuffix(host, ".local") || !strings.Contains(host, "."))
}

func requestMaxAge(req *http.Request) (time.Duration, bool) {
	query, err := url.ParseQuery(req.URL.RawQuery)
	if err != nil {
		return 0, false
	}
	values, present := query["max-age"]
	if !present {
		return -1, true
	}
	if len(values) != 1 || !decimal(values[0]) {
		return 0, false
	}
	seconds, err := strconv.ParseUint(values[0], 10, 64)
	if err != nil || seconds > math.MaxInt64/uint64(time.Second) {
		return 0, false
	}
	return time.Duration(seconds) * time.Second, true
}

func requestHops(req *http.Request) (int, bool) {
	values := req.Header.Values("Pith-Hops")
	if len(values) == 0 {
		return 0, true
	}
	if len(values) != 1 || !decimal(values[0]) {
		return 0, false
	}
	// Any large count is already at the limit; do not let overflow reset it.
	n, err := strconv.ParseUint(values[0], 10, 64)
	if err != nil || n >= api.MaxProxyHops {
		return api.MaxProxyHops, true
	}
	return int(n), true
}

func decimal(s string) bool {
	if s == "" {
		return false
	}
	for _, c := range []byte(s) {
		if c < '0' || c > '9' {
			return false
		}
	}
	return true
}

func (m *Manager) portInfo(ctx context.Context, e *listener, maxAge time.Duration, hops int) (api.ProxyInfo, *portFailure) {
	info := api.ProxyInfo{Pith: api.ProxyInfoVersion, Port: e.cfg.Port}
	switch {
	case e.cfg.Upstream != "":
		info.Exit = api.ExitUpstream
		up, err := m.upstreamInfo(ctx, e, maxAge, hops)
		var failure *portFailure
		if errors.As(err, &failure) {
			return info, failure
		}
		info.Online = err == nil
		if up != nil {
			info.Upstream = up
			info.Online, info.PublicIPv4, info.PublicIPv6 = up.Online, up.PublicIPv4, up.PublicIPv6
			info.At, info.Changing = up.At, up.Changing
			info.CanChangeIP = e.allowChangeIP.Load() && up.CanChangeIP
		}
	case e.cfg.Scene != "":
		info.Exit, info.Online = api.ExitScene, true
	default:
		info.Exit, info.Interface, info.Online = api.ExitInterface, e.cfg.Via.Name, true
		if e.cfg.Via.Label != "" {
			info.Interface = e.cfg.Via.Label
		}
		if m.opts.Info != nil {
			if found, err := m.opts.Info(ctx, e.cfg.Via, maxAge); err == nil {
				info.Interface, info.Online = found.Interface, found.Online
				// In UTC: an answer doesn't tell this Mac's time zone.
				info.PublicIPv4, info.PublicIPv6, info.At = found.PublicIPv4, found.PublicIPv6, found.At.UTC()
				info.CanChangeIP, info.Changing = e.allowChangeIP.Load() && found.CanChangeIP, found.Changing
			} else {
				info.Interface, info.Online, info.Changing = found.Interface, found.Online, found.Changing
			}
		}
		if m.opts.Changing != nil {
			info.Changing = info.Changing || m.opts.Changing(e.cfg.Via)
		}
	}
	m.mu.Lock()
	listening := !m.closed && !e.retired && e.ln != nil
	if e.cfg.Via.Name != "" && m.suspended != nil && m.suspended(e.cfg.Via) {
		listening = false
	}
	if e.cfg.Upstream != "" {
		info.Changing = info.Changing || e.up.changing > 0
	}
	m.mu.Unlock()
	info.Online = info.Online && listening
	return info, nil
}

func (m *Manager) changeIP(ctx context.Context, e *listener, hops int) (api.ProxyIPChange, *portFailure) {
	result := api.ProxyIPChange{Port: e.cfg.Port}
	if changeWaitOver(ctx) {
		return result, changeWaitEnded()
	}
	switch {
	case e.cfg.Upstream != "":
		return m.changeUpstreamIP(ctx, e, hops)
	case e.cfg.Scene != "" || m.opts.ChangeIP == nil:
		return result, &portFailure{http.StatusNotImplemented, unsupportedChange}
	default:
		changed, err := m.opts.ChangeIP(ctx, e.cfg.Via)
		if err != nil {
			if changeWaitOver(ctx) || errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
				return result, changeWaitEnded()
			}
			problem := changeError(err)
			status := http.StatusBadGateway
			if problem.Code == api.ErrUnsupported {
				status = http.StatusNotImplemented
			} else if problem.Code == api.ErrBusy {
				status = http.StatusServiceUnavailable
			}
			return result, &portFailure{status, problem}
		}
		changed.Port, changed.At = e.cfg.Port, changed.At.UTC()
		return changed, nil
	}
}

func changeWaitEnded() *portFailure {
	return portProblem(http.StatusBadGateway, api.ErrInternal, "the wait for an address change ended; the change may still be in progress")
}

func changeWaitOver(ctx context.Context) bool {
	if ctx.Err() != nil {
		return true
	}
	// A socket's deadline may fire before context cancellation has propagated.
	deadline, ok := ctx.Deadline()
	return ok && !time.Now().Before(deadline)
}

func changeError(err error) *api.Error {
	var problem *api.Error
	if errors.As(err, &problem) && problem != nil && problem.Code != "" {
		copy := *problem
		return &copy
	}
	return &api.Error{Code: api.ErrInternal, Detail: "the address change failed"}
}

type errorBody struct {
	Error *api.Error `json:"error"`
}

func writePortError(conn net.Conn, failure *portFailure) {
	writePortJSON(conn, failure.status, errorBody{failure.problem}, "")
}

func writePortJSON(conn net.Conn, status int, value any, allow string) {
	body, err := json.Marshal(value)
	if err != nil {
		status = http.StatusInternalServerError
		body = []byte("{\"error\":{\"code\":\"internal\"}}")
	}
	head := "HTTP/1.1 " + strconv.Itoa(status) + " " + http.StatusText(status) + "\r\n" +
		"Content-Type: application/json\r\nCache-Control: no-store\r\nX-Content-Type-Options: nosniff\r\nConnection: close\r\n" +
		"Content-Length: " + strconv.Itoa(len(body)) + "\r\n"
	if allow != "" {
		head += "Allow: " + allow + "\r\n"
	}
	_ = writeAll(conn, append([]byte(head+"\r\n"), body...))
}

func cloneInfo(info *api.ProxyInfo) *api.ProxyInfo {
	if info == nil {
		return nil
	}
	copy := *info
	copy.Upstream = cloneInfo(info.Upstream)
	return &copy
}
