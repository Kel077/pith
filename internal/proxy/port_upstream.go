package proxy

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"io"
	"mime"
	"net/http"
	"net/url"
	"strconv"
	"time"

	"github.com/Kel077/pith/internal/api"
)

// maxUpstreamInfo bounds the whole answer, including its HTTP header.
const maxUpstreamInfo = 64 << 10

var errUpstreamHTTP = &api.Error{Code: api.ErrUpstreamUnreachable, Detail: "the upstream left its HTTP request unanswered"}

type upstreamResponse struct {
	status int
	json   []byte
}

// upstreamHTTP asks the upstream's port directly, without a SOCKS CONNECT,
// redirects, environment proxies or headers supplied by this port's caller.
func (m *Manager) upstreamHTTP(ctx context.Context, e *listener, method string, maxAge time.Duration, hops int) (upstreamResponse, error) {
	if hops >= api.MaxProxyHops {
		return upstreamResponse{}, portProblem(http.StatusLoopDetected, api.ErrInternal, "too many Pith proxy hops")
	}
	answer, cancel := context.WithTimeout(ctx, upstreamAnswer)
	conn, err := m.opts.Upstream(answer, "tcp", e.cfg.Upstream)
	cancel()
	if err != nil || conn == nil {
		if conn != nil {
			_ = conn.Close()
		}
		return upstreamResponse{}, errUpstreamUnreachable
	}
	defer conn.Close()
	if e.cfg.Shared && toThisMac(conn) {
		return upstreamResponse{}, errUpstreamOwn
	}
	stop := context.AfterFunc(ctx, func() { _ = conn.Close() })
	defer stop()
	if deadline, ok := ctx.Deadline(); ok {
		if conn.SetDeadline(deadline) != nil {
			return upstreamResponse{}, errUpstreamHTTP
		}
	}
	path := "/"
	if method == http.MethodPost {
		path = "/change-ip"
	} else if maxAge >= 0 {
		path += "?max-age=" + strconv.FormatInt(int64(maxAge/time.Second), 10)
	}
	req := &http.Request{
		Method: method, URL: &url.URL{Path: "/"}, Host: conn.RemoteAddr().String(),
		Header: make(http.Header), Close: true,
	}
	req.URL, _ = url.Parse(path)
	req.Header.Set("Pith-Hops", strconv.Itoa(hops+1))
	req.Header["User-Agent"] = []string{""}
	if err := req.Write(conn); err != nil {
		return upstreamResponse{}, errUpstreamHTTP
	}
	reader := &io.LimitedReader{R: conn, N: maxUpstreamInfo + 1}
	res, err := http.ReadResponse(bufio.NewReader(reader), req)
	if err != nil {
		return upstreamResponse{}, errUpstreamHTTP
	}
	defer res.Body.Close()
	response := upstreamResponse{status: res.StatusCode}
	data, err := io.ReadAll(res.Body)
	if err != nil || reader.N <= 0 {
		return upstreamResponse{}, errUpstreamHTTP
	}
	if media, _, err := mime.ParseMediaType(res.Header.Get("Content-Type")); err == nil && media == "application/json" {
		response.json = data
	}
	return response, nil
}

// upstreamInfo tries Pith's answer first, then the greeting an ordinary SOCKS5
// proxy knows. A poll and a caller both replace the last answer, including nil
// when an upstream no longer speaks as Pith does.
func (m *Manager) upstreamInfo(ctx context.Context, e *listener, maxAge time.Duration, hops int) (*api.ProxyInfo, error) {
	ctx, cancel := context.WithTimeout(ctx, m.opts.ConnectTimeout)
	defer cancel()
	answer, answered := context.WithTimeout(ctx, min(upstreamAnswer, m.opts.ConnectTimeout/2))
	response, err := m.upstreamHTTP(answer, e, http.MethodGet, maxAge, hops)
	answered()
	if response.status == http.StatusLoopDetected {
		err = portProblem(http.StatusLoopDetected, api.ErrInternal, "too many Pith proxy hops")
	}
	var info *api.ProxyInfo
	if err == nil && response.status == http.StatusOK {
		var found api.ProxyInfo
		if json.Unmarshal(response.json, &found) == nil && found.Pith >= api.ProxyInfoVersion {
			info = &found
		}
	}
	var failure *portFailure
	if info == nil && !errors.As(err, &failure) && (err == nil || err == errUpstreamHTTP) && ctx.Err() == nil {
		// An HTTP refusal with no JSON may be a shared port refusing this
		// Mac as a device. Its SOCKS greeting tells it from ChangeIP being off.
		conn, _, fallback := connectUpstream(ctx, m.opts.Upstream, e.cfg.Upstream, nil, e.cfg.Shared)
		if conn != nil {
			_ = conn.Close()
		}
		err = fallback
	}
	if errors.Is(ctx.Err(), context.Canceled) {
		return cloneInfo(info), ctx.Err()
	}
	m.mu.Lock()
	e.up.info, e.up.checked = info, time.Now()
	m.mu.Unlock()
	m.noteUpstream(e, err)
	return cloneInfo(info), err
}

func (m *Manager) changeUpstreamIP(ctx context.Context, e *listener, hops int) (api.ProxyIPChange, *portFailure) {
	result := api.ProxyIPChange{Port: e.cfg.Port}
	up, err := m.upstreamInfo(ctx, e, -1, hops)
	var failure *portFailure
	if errors.As(err, &failure) {
		return result, failure
	}
	if err != nil {
		return result, &portFailure{http.StatusBadGateway, changeError(err)}
	}
	if up == nil {
		return result, &portFailure{http.StatusNotImplemented, unsupportedChange}
	}
	m.mu.Lock()
	e.up.changing++
	m.mu.Unlock()
	defer func() {
		if ctx.Err() == nil {
			_, _ = m.upstreamInfo(ctx, e, -1, hops)
		}
		m.mu.Lock()
		if ctx.Err() != nil {
			// The change may continue upstream after this wait ends. Let
			// the next Status poll learn that without extending the wait.
			e.up.checked = time.Time{}
		}
		e.up.changing--
		m.mu.Unlock()
	}()
	response, err := m.upstreamHTTP(ctx, e, http.MethodPost, -1, hops)
	if errors.As(err, &failure) {
		return result, failure
	}
	if err != nil {
		if ctx.Err() == nil {
			m.noteUpstream(e, err)
		}
		return result, &portFailure{http.StatusBadGateway, changeError(err)}
	}
	var body errorBody
	_ = json.Unmarshal(response.json, &body)
	switch response.status {
	case http.StatusOK:
		var changed api.ProxyIPChange
		if json.Unmarshal(response.json, &changed) == nil && changed.Port > 0 {
			changed.Port = e.cfg.Port
			return changed, nil
		}
	case http.StatusForbidden:
		if body.Error != nil && body.Error.Code == api.ErrNotAllowed {
			return result, portProblem(http.StatusForbidden, api.ErrNotAllowed, "the upstream port does not allow address changes")
		}
		m.noteUpstream(e, errUpstreamRefused)
		return result, &portFailure{http.StatusBadGateway, errUpstreamRefused}
	case http.StatusNotImplemented:
		return result, &portFailure{http.StatusNotImplemented, unsupportedChange}
	case http.StatusBadGateway:
		if body.Error != nil && body.Error.Code != "" {
			return result, &portFailure{http.StatusBadGateway, body.Error}
		}
	case http.StatusLoopDetected:
		return result, portProblem(http.StatusLoopDetected, api.ErrInternal, "too many Pith proxy hops")
	case http.StatusServiceUnavailable:
		return result, portProblem(http.StatusServiceUnavailable, api.ErrInternal, "the upstream has too many requests in progress")
	default:
		return result, &portFailure{http.StatusNotImplemented, unsupportedChange}
	}
	return result, portProblem(http.StatusBadGateway, api.ErrInternal, "the upstream returned an invalid address change")
}
