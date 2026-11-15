package proxy

import (
	"context"
	"net"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Kel077/pith/internal/api"
)

func waitPortRequests(t *testing.T, m *Manager, requests, changes int) {
	t.Helper()
	waitFor(t, func() bool {
		m.mu.Lock()
		defer m.mu.Unlock()
		return m.requests == requests && m.changeRequests == changes && m.total == 0
	})
}

func TestPortChangeRequestsLeaveRoomForInfo(t *testing.T) {
	changeEntered := make(chan struct{}, maxChangeRequests)
	infoEntered := make(chan struct{}, maxPortRequests-maxChangeRequests)
	releaseChange, releaseInfo := make(chan struct{}), make(chan struct{})
	var holdInfo atomic.Bool
	want := api.ProxyIPChange{BeforeIPv4: "192.0.2.1", PublicIPv4: "192.0.2.2", Seconds: 3}
	m, f := newTestManager(t, Options{Dial: noDial(t), MaxPerPort: 1,
		ChangeIP: func(ctx context.Context, _ api.InterfaceRef) (api.ProxyIPChange, error) {
			changeEntered <- struct{}{}
			select {
			case <-releaseChange:
				return want, nil
			case <-ctx.Done():
				return api.ProxyIPChange{}, ctx.Err()
			}
		},
		Info: func(ctx context.Context, _ api.InterfaceRef, _ time.Duration) (api.ProxyInfo, error) {
			if holdInfo.Load() {
				infoEntered <- struct{}{}
				select {
				case <-releaseInfo:
				case <-ctx.Done():
					return api.ProxyInfo{}, ctx.Err()
				}
			}
			return api.ProxyInfo{Online: true, Changing: true, CanChangeIP: true}, nil
		},
	})
	a, b, off := testProxy(18080), testProxy(18081), testProxy(18082)
	a.ChangeIP, b.ChangeIP = true, true
	applyTest(t, m, a, b, off)
	var changes []net.Conn
	for i := range maxChangeRequests {
		changes = append(changes, portRequest(t, f, a.Port+i%2, "POST", "/change-ip", "", ""))
		<-changeEntered
	}
	portError(t, portRequest(t, f, a.Port, "POST", "/change-ip", "", ""), 503, api.ErrInternal)
	waitPortRequests(t, m, maxChangeRequests, maxChangeRequests)

	// A full change queue still answers requests that need no change slot.
	for _, tc := range []struct {
		port         int
		method, path string
		fields, body string
		status       int
		code         api.ErrorCode
	}{
		{a.Port, "POST", "/missing", "", "", 404, api.ErrUnsupported},
		{a.Port, "POST", "/", "", "", 405, api.ErrUnsupported},
		{a.Port, "POST", "/change-ip?max-age=bad", "", "", 400, api.ErrInvalidConfig},
		{off.Port, "POST", "/change-ip", "", "", 403, api.ErrNotAllowed},
		{a.Port, "POST", "/change-ip", "", strings.Repeat("a", maxChangeBody+1), 413, api.ErrInvalidConfig},
		{a.Port, "POST", "/change-ip", "Origin: https://example.com\r\n", "", 403, api.ErrNotAllowed},
	} {
		portError(t, portRequest(t, f, tc.port, tc.method, tc.path, tc.fields, tc.body), tc.status, tc.code)
		waitPortRequests(t, m, maxChangeRequests, maxChangeRequests)
	}
	var info api.ProxyInfo
	portAnswer(t, portRequest(t, f, a.Port, "GET", "/", "", ""), 200, &info)
	if !info.Changing || !info.Online {
		t.Fatalf("GET during a change = %+v", info)
	}
	waitPortRequests(t, m, maxChangeRequests, maxChangeRequests)

	// All sixteen remaining slots may hold GETs while the POSTs still wait.
	holdInfo.Store(true)
	var reads []net.Conn
	for i := range maxPortRequests - maxChangeRequests {
		reads = append(reads, portRequest(t, f, a.Port+i%2, "GET", "/", "", ""))
		<-infoEntered
	}
	waitPortRequests(t, m, maxPortRequests, maxChangeRequests)
	portError(t, portRequest(t, f, a.Port, "GET", "/", "", ""), 503, api.ErrInternal)
	close(releaseInfo)
	for _, c := range reads {
		portAnswer(t, c, 200, &info)
		if !info.Changing {
			t.Fatal("a reserved GET did not report the change")
		}
	}
	waitPortRequests(t, m, maxChangeRequests, maxChangeRequests)
	close(releaseChange)
	for i, c := range changes {
		var changed api.ProxyIPChange
		portAnswer(t, c, 200, &changed)
		expected := want
		expected.Port = a.Port + i%2
		if changed != expected {
			t.Fatalf("change waiter %d got %+v, want %+v", i, changed, expected)
		}
	}
	waitPortRequests(t, m, 0, 0)
}

func TestPortChangeRequestSlotsReturnOnDeparture(t *testing.T) {
	entered, release := make(chan struct{}, maxChangeRequests), make(chan struct{})
	want := api.ProxyIPChange{PublicIPv4: "192.0.2.2", Seconds: 3}
	m, f := newTestManager(t, Options{Dial: noDial(t), ChangeIP: func(ctx context.Context, _ api.InterfaceRef) (api.ProxyIPChange, error) {
		entered <- struct{}{}
		select {
		case <-release:
			return want, nil
		case <-ctx.Done():
			return api.ProxyIPChange{}, ctx.Err()
		}
	}})
	p := testProxy(18080)
	p.ChangeIP = true
	applyTest(t, m, p)
	var waiting []net.Conn
	for range maxChangeRequests {
		waiting = append(waiting, portRequest(t, f, p.Port, "POST", "/change-ip", "", ""))
		<-entered
	}
	leaving := maxChangeRequests / 2
	for _, c := range waiting[:leaving] {
		_ = c.Close()
	}
	waitPortRequests(t, m, maxChangeRequests-leaving, maxChangeRequests-leaving)
	waiting = waiting[leaving:]
	for range leaving {
		waiting = append(waiting, portRequest(t, f, p.Port, "POST", "/change-ip", "", ""))
		<-entered
	}
	waitPortRequests(t, m, maxChangeRequests, maxChangeRequests)
	portError(t, portRequest(t, f, p.Port, "POST", "/change-ip", "", ""), 503, api.ErrInternal)
	close(release)
	expected := want
	expected.Port = p.Port
	for _, c := range waiting {
		var changed api.ProxyIPChange
		portAnswer(t, c, 200, &changed)
		if changed != expected {
			t.Fatalf("remaining change waiter got %+v, want %+v", changed, expected)
		}
	}
	waitPortRequests(t, m, 0, 0)
}
