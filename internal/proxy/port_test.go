package proxy

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Kel077/pith/internal/api"
)

func portRequest(t *testing.T, f *listenerFixture, port int, method, path, fields, body string) *net.TCPConn {
	t.Helper()
	c := f.dial(t, port)
	wire := fmt.Sprintf("%s %s HTTP/1.1\r\nHost: example.com\r\n%sContent-Length: %d\r\n\r\n%s", method, path, fields, len(body), body)
	if _, err := io.WriteString(c, wire); err != nil {
		t.Fatal(err)
	}
	return c
}

func portAnswer(t *testing.T, c net.Conn, status int, value any) *http.Response {
	t.Helper()
	res := httpAnswer(t, c)
	defer res.Body.Close()
	body, err := io.ReadAll(res.Body)
	if err != nil {
		t.Fatal(err)
	}
	if res.StatusCode != status {
		t.Fatalf("status = %d, want %d: %s", res.StatusCode, status, body)
	}
	if !res.Close || res.ContentLength != int64(len(body)) || res.Header.Get("Content-Type") != "application/json" ||
		res.Header.Get("Cache-Control") != "no-store" || res.Header.Get("X-Content-Type-Options") != "nosniff" {
		t.Fatalf("unexpected response fields: %v, close %v, length %d, body %q", res.Header, res.Close, res.ContentLength, body)
	}
	for key := range res.Header {
		if strings.HasPrefix(key, "Access-Control-") {
			t.Fatalf("CORS field %s in %v", key, res.Header)
		}
	}
	if !json.Valid(body) {
		t.Fatalf("not JSON: %q", body)
	}
	if value != nil {
		if err := json.Unmarshal(body, value); err != nil {
			t.Fatal(err)
		}
	}
	return res
}

func portError(t *testing.T, c net.Conn, status int, code api.ErrorCode) {
	t.Helper()
	var body errorBody
	portAnswer(t, c, status, &body)
	if body.Error == nil || body.Error.Code != code {
		t.Fatalf("error = %+v, want %s", body.Error, code)
	}
}

func TestPortInfoAndMaxAge(t *testing.T) {
	at := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	ages := make(chan time.Duration, 10)
	m, f := newTestManager(t, Options{Dial: noDial(t), Info: func(ctx context.Context, via api.InterfaceRef, maxAge time.Duration) (api.ProxyInfo, error) {
		if via != testVia {
			t.Errorf("Info via = %+v", via)
		}
		if deadline, ok := ctx.Deadline(); !ok || time.Until(deadline) > defaultConnectTimeout {
			t.Error("Info has no bounded wait")
		}
		ages <- maxAge
		return api.ProxyInfo{Pith: 100, Port: 99, Exit: api.ExitScene, Interface: "Example adapter", Online: true,
			PublicIPv4: "192.0.2.1", PublicIPv6: "2001:db8::1", At: at, CanChangeIP: true, Changing: true}, nil
	}})
	p := testProxy(18080)
	applyTest(t, m, p)
	for path, age := range map[string]time.Duration{"/": -1, "/?max-age=0": 0, "/?max-age=60": time.Minute, "/?max-age=0002": 2 * time.Second} {
		var info api.ProxyInfo
		c := portRequest(t, f, p.Port, "GET", path, "", "")
		portAnswer(t, c, 200, &info)
		if got := <-ages; got != age {
			t.Fatalf("%s: maxAge = %s, want %s", path, got, age)
		}
		if info.Pith != 1 || info.Port != p.Port || info.Exit != api.ExitInterface || info.Interface != "Example adapter" ||
			!info.Online || info.CanChangeIP || !info.Changing || info.PublicIPv4 != "192.0.2.1" || info.PublicIPv6 != "2001:db8::1" || !info.At.Equal(at) || info.Upstream != nil {
			t.Fatalf("info = %+v", info)
		}
		readClosed(t, c)
	}
	for _, query := range []string{"", "-1", "+1", "1.5", "one", "9223372036854775807", "18446744073709551616", "1&max-age=2", "%zz", "1;other=2"} {
		portError(t, portRequest(t, f, p.Port, "GET", "/?max-age="+query, "", ""), 400, api.ErrInvalidConfig)
	}
	if len(ages) != 0 {
		t.Fatal("invalid max-age reached Info")
	}
	p.ChangeIP = true
	applyTest(t, m, p)
	var info api.ProxyInfo
	portAnswer(t, portRequest(t, f, p.Port, "GET", "/", "", ""), 200, &info)
	<-ages
	if !info.CanChangeIP {
		t.Fatal("the port's ChangeIP was not counted in")
	}
}

func TestPortMethodsPathsAndOneRequest(t *testing.T) {
	m, f := newTestManager(t, Options{Dial: noDial(t)})
	applyTest(t, m, testProxy(18080))
	for _, tc := range []struct{ method, path, allow string }{
		{"POST", "/", "GET"}, {"HEAD", "/", "GET"}, {"OPTIONS", "/", "GET"},
		{"GET", "/change-ip", "POST"}, {"OPTIONS", "/change-ip", "POST"},
	} {
		res := portAnswer(t, portRequest(t, f, 18080, tc.method, tc.path, "", ""), 405, nil)
		if res.Header.Get("Allow") != tc.allow {
			t.Fatalf("%s %s: Allow = %q", tc.method, tc.path, res.Header.Get("Allow"))
		}
	}
	portError(t, portRequest(t, f, 18080, "GET", "/unknown", "", ""), 404, api.ErrUnsupported)
	c := f.dial(t, 18080)
	fmt.Fprint(c, "GET / HTTP/1.1\r\nHost: example.com\r\n\r\nPOST /change-ip HTTP/1.1\r\nHost: example.com\r\n\r\n")
	portAnswer(t, c, 200, nil)
	readClosed(t, c)
}

func TestPortRefusesWebPagesBeforeDispatch(t *testing.T) {
	var infoCalls, changeCalls atomic.Int32
	m, f := newTestManager(t, Options{Dial: noDial(t),
		Info: func(context.Context, api.InterfaceRef, time.Duration) (api.ProxyInfo, error) {
			infoCalls.Add(1)
			return api.ProxyInfo{Online: true}, nil
		},
		ChangeIP: func(context.Context, api.InterfaceRef) (api.ProxyIPChange, error) {
			changeCalls.Add(1)
			return api.ProxyIPChange{}, nil
		},
	})
	p := testProxy(18080)
	p.ChangeIP = true
	applyTest(t, m, p)
	for _, fields := range []string{
		"Origin: https://example.com\r\n", "Origin:\r\n", "Origin: null\r\n",
		"Sec-Fetch-Site: same-origin\r\n", "Sec-Fetch-Site: cross-site\r\n", "Sec-Fetch-Site:\r\n",
		"Sec-Fetch-Site: none\r\nSec-Fetch-Site: same-origin\r\n", "Sec-Fetch-Site: none, cross-site\r\n",
	} {
		for _, request := range []struct{ method, path string }{{"GET", "/"}, {"POST", "/change-ip"}, {"OPTIONS", "/unknown?max-age=bad"}} {
			portError(t, portRequest(t, f, p.Port, request.method, request.path, fields, ""), 403, api.ErrNotAllowed)
		}
	}
	if infoCalls.Load() != 0 || changeCalls.Load() != 0 {
		t.Fatal("a page's request reached a hook")
	}
	for _, fields := range []string{"", "Sec-Fetch-Site: none\r\n"} {
		portAnswer(t, portRequest(t, f, p.Port, "GET", "/", fields, ""), 200, nil)
		portAnswer(t, portRequest(t, f, p.Port, "POST", "/change-ip", fields, ""), 200, nil)
	}
	if infoCalls.Load() != 2 || changeCalls.Load() != 2 {
		t.Fatal("an allowed request did not reach its hook")
	}
}

func TestPortInterfaceAndSceneFallbacks(t *testing.T) {
	for _, hook := range []bool{false, true} {
		opts := Options{Dial: noDial(t)}
		if hook {
			opts.Info = func(context.Context, api.InterfaceRef, time.Duration) (api.ProxyInfo, error) {
				return api.ProxyInfo{PublicIPv4: "192.0.2.7", CanChangeIP: true}, errors.New("not available")
			}
		}
		m, f := newTestManager(t, opts)
		applyTest(t, m, testProxy(18080), sceneProxy(18081, "example"))
		var info api.ProxyInfo
		portAnswer(t, portRequest(t, f, 18080, "GET", "/", "", ""), 200, &info)
		if info.Interface != testVia.Name || !info.Online || info.CanChangeIP || info.PublicIPv4 != "" {
			t.Fatalf("interface fallback = %+v", info)
		}
		info = api.ProxyInfo{}
		portAnswer(t, portRequest(t, f, 18081, "GET", "/", "", ""), 200, &info)
		if info.Exit != api.ExitScene || !info.Online || info.Interface != "" || info.CanChangeIP || info.PublicIPv4 != "" || !info.At.IsZero() {
			t.Fatalf("scene = %+v", info)
		}
		m.Suspend(func(api.InterfaceRef) bool { return true })
		info = api.ProxyInfo{}
		portAnswer(t, portRequest(t, f, 18080, "GET", "/", "", ""), 200, &info)
		if info.Online {
			t.Fatal("a suspended exit is online")
		}
	}
}

func TestPortChangeIPResultsErrorsAndBodies(t *testing.T) {
	var failure atomic.Pointer[error]
	var calls atomic.Int32
	m, f := newTestManager(t, Options{Dial: noDial(t), ChangeIP: func(ctx context.Context, via api.InterfaceRef) (api.ProxyIPChange, error) {
		calls.Add(1)
		if deadline, ok := ctx.Deadline(); via != testVia || !ok || time.Until(deadline) > changeIPTimeout || time.Until(deadline) < changeIPTimeout-time.Minute {
			t.Errorf("ChangeIP via/deadline = %+v, %v, %v", via, deadline, ok)
		}
		if err := failure.Load(); err != nil {
			return api.ProxyIPChange{}, *err
		}
		return api.ProxyIPChange{Port: 42, BeforeIPv4: "192.0.2.1", PublicIPv4: "192.0.2.2", PublicIPv6: "2001:db8::2", Seconds: 2.5}, nil
	}})
	p := testProxy(18080)
	applyTest(t, m, p)
	portError(t, portRequest(t, f, p.Port, "POST", "/change-ip", "", ""), 403, api.ErrNotAllowed)
	p.ChangeIP = true
	applyTest(t, m, p)
	for _, body := range []string{"", strings.Repeat("a", maxChangeBody)} {
		var changed api.ProxyIPChange
		portAnswer(t, portRequest(t, f, p.Port, "POST", "/change-ip", "", body), 200, &changed)
		if changed.Port != p.Port || changed.BeforeIPv4 != "192.0.2.1" || changed.PublicIPv4 != "192.0.2.2" || changed.PublicIPv6 != "2001:db8::2" || changed.Seconds != 2.5 {
			t.Fatalf("change = %+v", changed)
		}
	}
	portError(t, portRequest(t, f, p.Port, "POST", "/change-ip", "", strings.Repeat("a", maxChangeBody+1)), 413, api.ErrInvalidConfig)
	for _, body := range []string{"4\r\nbody\r\n0\r\n\r\n", fmt.Sprintf("%x\r\n%s\r\n0\r\n\r\n", maxChangeBody+1, strings.Repeat("b", maxChangeBody+1))} {
		c := f.dial(t, p.Port)
		fmt.Fprint(c, "POST /change-ip HTTP/1.1\r\nHost: example.com\r\nTransfer-Encoding: chunked\r\n\r\n"+body)
		if strings.HasPrefix(body, "4\r\n") {
			portAnswer(t, c, 200, nil)
		} else {
			portError(t, c, 413, api.ErrInvalidConfig)
		}
	}
	if calls.Load() != 3 {
		t.Fatalf("body validation made %d hook calls, want 3", calls.Load())
	}
	for _, tc := range []struct {
		err    error
		status int
		code   api.ErrorCode
	}{
		{&api.Error{Code: api.ErrUnsupported}, 501, api.ErrUnsupported},
		{fmt.Errorf("wrapped: %w", &api.Error{Code: api.ErrNoStandby, Detail: "no standby exit"}), 502, api.ErrNoStandby},
		{errors.New("private internal failure"), 502, api.ErrInternal},
	} {
		failure.Store(&tc.err)
		var answer errorBody
		portAnswer(t, portRequest(t, f, p.Port, "POST", "/change-ip", "", ""), tc.status, &answer)
		if answer.Error == nil || answer.Error.Code != tc.code || strings.Contains(answer.Error.Detail, "private") {
			t.Fatalf("error = %+v, want %s", answer.Error, tc.code)
		}
	}
}

func TestPortNoChangeHook(t *testing.T) {
	m, f := newTestManager(t, Options{Dial: noDial(t)})
	p := testProxy(18080)
	p.ChangeIP = true
	applyTest(t, m, p)
	portError(t, portRequest(t, f, p.Port, "POST", "/change-ip", "", ""), 501, api.ErrUnsupported)
}

func TestPortRequestSurvivesSuspend(t *testing.T) {
	entered, release := make(chan struct{}), make(chan struct{})
	var changing atomic.Bool
	m, f := newTestManager(t, Options{
		Dial: echoPeer(t).dial,
		Info: func(context.Context, api.InterfaceRef, time.Duration) (api.ProxyInfo, error) {
			return api.ProxyInfo{Online: true, CanChangeIP: true}, nil
		},
		Changing: func(api.InterfaceRef) bool { return changing.Load() },
		ChangeIP: func(ctx context.Context, _ api.InterfaceRef) (api.ProxyIPChange, error) {
			changing.Store(true)
			close(entered)
			select {
			case <-release:
				changing.Store(false)
				return api.ProxyIPChange{PublicIPv4: "192.0.2.2"}, nil
			case <-ctx.Done():
				return api.ProxyIPChange{}, ctx.Err()
			}
		},
	})
	p := testProxy(18080)
	p.ChangeIP = true
	applyTest(t, m, p)
	stream := connectTest(t, f, p.Port)
	c := portRequest(t, f, p.Port, "POST", "/change-ip", "", "")
	<-entered
	m.Suspend(func(api.InterfaceRef) bool { return true })
	readClosed(t, stream)
	if st := m.Status(context.Background())[0]; !st.Changing {
		t.Fatal("Status doesn't report the interface's change")
	}
	var info api.ProxyInfo
	portAnswer(t, portRequest(t, f, p.Port, "GET", "/", "", ""), 200, &info)
	if info.Online || !info.Changing || !info.CanChangeIP {
		t.Fatalf("during Suspend: %+v", info)
	}
	waitFor(t, func() bool { m.mu.Lock(); defer m.mu.Unlock(); return m.requests == 1 })
	m.mu.Lock()
	open, requests, quiet := m.byPort[p.Port].open, m.requests, m.quietestLocked(m.byPort[p.Port], true)
	m.mu.Unlock()
	if open != 0 || requests != 1 || quiet != nil {
		t.Fatalf("control request became a stream: open %d, requests %d, quiet %v", open, requests, quiet)
	}
	close(release)
	portAnswer(t, c, 200, nil)
	m.Resume()
}

func TestPortRequestsHaveOneManagerLimit(t *testing.T) {
	entered, release := make(chan struct{}, maxPortRequests), make(chan struct{})
	m, f := newTestManager(t, Options{Dial: echoPeer(t).dial, MaxPerPort: 1, Info: func(ctx context.Context, _ api.InterfaceRef, _ time.Duration) (api.ProxyInfo, error) {
		entered <- struct{}{}
		select {
		case <-release:
			return api.ProxyInfo{Online: true}, nil
		case <-ctx.Done():
			return api.ProxyInfo{}, ctx.Err()
		}
	}})
	a, b := testProxy(18080), testProxy(18081)
	applyTest(t, m, a, b)
	var pending []net.Conn
	for i := range maxPortRequests {
		pending = append(pending, portRequest(t, f, 18080+i%2, "GET", "/", "", ""))
		<-entered
	}
	portError(t, portRequest(t, f, a.Port, "GET", "/", "", ""), 503, api.ErrInternal)
	waitFor(t, func() bool { m.mu.Lock(); defer m.mu.Unlock(); return m.total == 0 })
	stream := connectTest(t, f, a.Port)
	roundTrip(t, stream, "requests leave room for streams")
	m.mu.Lock()
	requests, total := m.requests, m.total
	m.mu.Unlock()
	if requests != maxPortRequests || total != 1 {
		t.Fatalf("slots = %d requests, %d streams", requests, total)
	}
	close(release)
	for _, c := range pending {
		portAnswer(t, c, 200, nil)
	}
	waitFor(t, func() bool { m.mu.Lock(); defer m.mu.Unlock(); return m.requests == 0 && m.changeRequests == 0 })
	roundTrip(t, stream, "still open")
}

func TestPortCallerDepartureEndsOnlyItsWait(t *testing.T) {
	entered, exited, release := make(chan struct{}, 2), make(chan struct{}, 2), make(chan struct{})
	m, f := newTestManager(t, Options{Dial: noDial(t), ChangeIP: func(ctx context.Context, _ api.InterfaceRef) (api.ProxyIPChange, error) {
		entered <- struct{}{}
		defer func() { exited <- struct{}{} }()
		select {
		case <-release:
			return api.ProxyIPChange{}, nil
		case <-ctx.Done():
			return api.ProxyIPChange{}, ctx.Err()
		}
	}})
	p := testProxy(18080)
	p.ChangeIP = true
	applyTest(t, m, p)
	first := portRequest(t, f, p.Port, "POST", "/change-ip", "", "")
	<-entered
	second := portRequest(t, f, p.Port, "POST", "/change-ip", "", "")
	<-entered
	_ = first.Close()
	select {
	case <-exited:
	case <-time.After(time.Second):
		t.Fatal("departed caller kept waiting")
	}
	waitFor(t, func() bool { m.mu.Lock(); defer m.mu.Unlock(); return m.requests == 1 })
	close(release)
	portAnswer(t, second, 200, nil)
	<-exited
}

func TestPortRequestsEndWithManager(t *testing.T) {
	entered, exited := make(chan struct{}), make(chan struct{})
	m, f := newTestManager(t, Options{Dial: noDial(t), ChangeIP: func(ctx context.Context, _ api.InterfaceRef) (api.ProxyIPChange, error) {
		close(entered)
		<-ctx.Done()
		close(exited)
		return api.ProxyIPChange{}, ctx.Err()
	}})
	p := testProxy(18080)
	p.ChangeIP = true
	applyTest(t, m, p)
	c := portRequest(t, f, p.Port, "POST", "/change-ip", "", "")
	<-entered
	if err := m.Close(); err != nil {
		t.Fatal(err)
	}
	<-exited
	// Cancellation may race the hook's failure reply. Any buffered reply
	// ends with the socket; no worker or request slot survives Close.
	_, _ = io.ReadAll(c)
	m.mu.Lock()
	requests := m.requests
	m.mu.Unlock()
	if requests != 0 {
		t.Fatalf("%d requests survived Close", requests)
	}
}

func TestPortHooksMayCallManager(t *testing.T) {
	entered, release := make(chan struct{}), make(chan struct{})
	var m *Manager
	m, f := newTestManager(t, Options{Dial: noDial(t),
		Info: func(_ context.Context, via api.InterfaceRef, _ time.Duration) (api.ProxyInfo, error) {
			return api.ProxyInfo{Online: !m.Suspended(via), CanChangeIP: true}, nil
		},
		Changing: func(via api.InterfaceRef) bool { return m.Suspended(via) },
		ChangeIP: func(ctx context.Context, via api.InterfaceRef) (api.ProxyIPChange, error) {
			m.Suspend(func(ref api.InterfaceRef) bool { return ref == via })
			defer m.Resume()
			close(entered)
			select {
			case <-release:
				return api.ProxyIPChange{PublicIPv4: "192.0.2.2"}, nil
			case <-ctx.Done():
				return api.ProxyIPChange{}, ctx.Err()
			}
		},
	})
	p := testProxy(18080)
	p.ChangeIP = true
	applyTest(t, m, p)
	c := portRequest(t, f, p.Port, "POST", "/change-ip", "", "")
	select {
	case <-entered:
	case <-time.After(time.Second):
		t.Fatal("ChangeIP could not call Suspend")
	}
	done := make(chan struct{})
	go func() {
		defer close(done)
		for range 20 {
			_ = m.Status(context.Background())
		}
	}()
	var info api.ProxyInfo
	portAnswer(t, portRequest(t, f, p.Port, "GET", "/", "", ""), 200, &info)
	if info.Online || !info.Changing {
		t.Fatalf("hooks didn't observe Suspend: %+v", info)
	}
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("Status deadlocked with hooks calling the manager")
	}
	close(release)
	portAnswer(t, c, 200, nil)
	if m.Suspended(testVia) {
		t.Fatal("ChangeIP could not call Resume")
	}
}

func TestPortInfoWaitIsBounded(t *testing.T) {
	exited := make(chan struct{})
	m, f := newTestManager(t, Options{Dial: noDial(t), ConnectTimeout: 30 * time.Millisecond,
		Info: func(ctx context.Context, _ api.InterfaceRef, _ time.Duration) (api.ProxyInfo, error) {
			<-ctx.Done()
			close(exited)
			return api.ProxyInfo{}, ctx.Err()
		},
	})
	applyTest(t, m, testProxy(18080))
	c := portRequest(t, f, 18080, "GET", "/", "", "")
	<-exited
	// The reply socket shares the expired deadline, as a CONNECT's does.
	_, _ = io.ReadAll(c)
	waitFor(t, func() bool { m.mu.Lock(); defer m.mu.Unlock(); return m.requests == 0 })
}

func TestPortRefusesUnlistedDevice(t *testing.T) {
	m, f := newTestManager(t, Options{Dial: noDial(t)})
	shareTest(t, m, []string{"192.0.2.10"}, sharedProxy(18080))
	c := f.dialFrom(t, 18080, "192.0.2.11")
	fmt.Fprint(c, "GET / HTTP/1.1\r\nHost: example.com\r\n\r\n")
	if res := httpAnswer(t, c); res.StatusCode != 403 {
		t.Fatalf("an unlisted device got %d", res.StatusCode)
	}
	readClosed(t, c)
	c = f.dialFrom(t, 18080, "192.0.2.10")
	fmt.Fprint(c, "GET / HTTP/1.1\r\nHost: example.com\r\n\r\n")
	portAnswer(t, c, 200, nil)
	if device, ok := deviceOf(m, "192.0.2.10"); !ok || device.Open != 0 || device.Sent != 0 || device.Received != 0 {
		t.Fatalf("device's control request counted as a stream: %+v, %v", device, ok)
	}
}

func TestManagerChangeIP(t *testing.T) {
	m, _ := newTestManager(t, Options{Dial: noDial(t), ChangeIP: func(context.Context, api.InterfaceRef) (api.ProxyIPChange, error) {
		return api.ProxyIPChange{PublicIPv4: "192.0.2.2"}, nil
	}})
	off := testProxy(18081)
	off.Enabled = false
	applyTest(t, m, testProxy(18080), off, sceneProxy(18082, "example"))
	if result, err := m.ChangeIP(context.Background(), 18080); err != nil || result.Port != 18080 || result.PublicIPv4 != "192.0.2.2" {
		t.Fatalf("local user with ChangeIP off: %+v, %v", result, err)
	}
	for _, port := range []int{18081, 18082, 18083} {
		if _, err := m.ChangeIP(context.Background(), port); api.Code(err) != api.ErrUnsupported {
			t.Fatalf("port %d: %v", port, err)
		} else if _, ok := err.(*api.Error); !ok {
			t.Fatalf("not an api.Error: %T", err)
		}
	}
	noHook, _ := newTestManager(t, Options{Dial: noDial(t)})
	applyTest(t, noHook, testProxy(18080))
	if _, err := noHook.ChangeIP(context.Background(), 18080); api.Code(err) != api.ErrUnsupported {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := m.ChangeIP(ctx, 18080); api.Code(err) != api.ErrInternal {
		t.Fatal(err)
	}
}
