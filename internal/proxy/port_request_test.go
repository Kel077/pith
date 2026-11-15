package proxy

import (
	"context"
	"errors"
	"fmt"
	"net"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Kel077/pith/internal/api"
)

func TestPortIncompleteBodiesExpireBeforeChangeWait(t *testing.T) {
	var changes atomic.Int32
	m, f := newTestManager(t, Options{Dial: noDial(t), ConnectTimeout: 150 * time.Millisecond,
		ChangeIP: func(context.Context, api.InterfaceRef) (api.ProxyIPChange, error) {
			changes.Add(1)
			return api.ProxyIPChange{}, nil
		},
	})
	p := testProxy(18080)
	p.ChangeIP = true
	applyTest(t, m, p)
	for range maxPortRequests {
		c := f.dial(t, p.Port)
		fmt.Fprint(c, "POST /change-ip HTTP/1.1\r\nHost: 127.0.0.1\r\nContent-Length: 5\r\n\r\n")
	}
	waitFor(t, func() bool { m.mu.Lock(); defer m.mu.Unlock(); return m.requests > 0 })
	waitPortRequests(t, m, 0, 0)
	if changes.Load() != 0 {
		t.Fatal("an incomplete body reached the change hook")
	}
	portAnswer(t, portRequest(t, f, p.Port, "GET", "/", "", ""), 200, nil)
}

func TestPortHostAllowsOnlyAddressesAndLocalNames(t *testing.T) {
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
	for _, host := range []string{"127.0.0.1:18080", "192.0.2.5", "192.0.2.5.:18080", "[2001:db8::1]:18080",
		"[2001:db8::1]", "2001:db8::1", "localhost", "LOCALHOST.:18080", "example-mac:18080", "Example-Mac.LOCAL.:18080"} {
		c := f.dial(t, p.Port)
		fmt.Fprintf(c, "GET / HTTP/1.1\r\nHost: %s\r\n\r\n", host)
		portAnswer(t, c, 200, nil)
	}
	c := f.dial(t, p.Port)
	fmt.Fprint(c, "GET / HTTP/1.0\r\n\r\n")
	portAnswer(t, c, 200, nil)
	before := infoCalls.Load()
	for _, host := range []string{"example.com", "127.0.0.1.example.com:18080", "mac.example.com.", "app.localhost:18080", "app.LOCALHOST."} {
		for _, request := range []struct{ method, path string }{{"GET", "/?max-age=bad"}, {"POST", "/change-ip"}, {"OPTIONS", "/unknown"}} {
			c := f.dial(t, p.Port)
			// A same-origin HTTP fetch need send neither Origin nor Sec-Fetch.
			fmt.Fprintf(c, "%s %s HTTP/1.1\r\nHost: %s\r\nUser-Agent: Mozilla/5.0\r\nReferer: http://%s/page\r\n\r\n", request.method, request.path, host, host)
			var body errorBody
			portAnswer(t, c, 403, &body)
			if body.Error == nil || body.Error.Code != api.ErrNotAllowed || !strings.Contains(body.Error.Detail, "Host") {
				t.Fatalf("Host %s: %+v", host, body.Error)
			}
		}
	}
	if infoCalls.Load() != before || changeCalls.Load() != 0 {
		t.Fatal("a public Host reached a hook")
	}
}

func TestPortLocalAddressCannotBypassHostRule(t *testing.T) {
	m, f := newTestManager(t, Options{Dial: noDial(t)})
	shareTest(t, m, nil, sharedProxy(18080))
	for _, host := range []string{"192.0.2.5:18080", "example.com:18080"} {
		c := f.dialAs(t, 18080, &deviceConn{
			remote: &net.TCPAddr{IP: net.ParseIP("192.0.2.5"), Port: 50000},
			local:  &net.TCPAddr{IP: net.ParseIP("192.0.2.5"), Port: 18080},
		})
		fmt.Fprintf(c, "GET / HTTP/1.1\r\nHost: %s\r\n\r\n", host)
		if host == "example.com:18080" {
			portError(t, c, 403, api.ErrNotAllowed)
		} else {
			portAnswer(t, c, 200, nil)
		}
	}
	if len(m.Devices()) != 0 {
		t.Fatal("this Mac's own connection was treated as a device")
	}
}

func TestPortFailedInfoKeepsConfiguredInterface(t *testing.T) {
	for _, tc := range []struct{ label, want string }{
		{"", testVia.Name}, {"Saved adapter", "Saved adapter"},
	} {
		t.Run(tc.want, func(t *testing.T) {
			m, f := newTestManager(t, Options{Dial: noDial(t),
				Info: func(context.Context, api.InterfaceRef, time.Duration) (api.ProxyInfo, error) {
					return api.ProxyInfo{Online: false, Changing: true}, errors.New("not available")
				},
			})
			p := testProxy(18080)
			p.Via.Label = tc.label
			applyTest(t, m, p)
			var info api.ProxyInfo
			portAnswer(t, portRequest(t, f, p.Port, "GET", "/", "", ""), 200, &info)
			if info.Interface != tc.want || info.Online || !info.Changing {
				t.Fatalf("a partial failure erased the configured interface: %+v", info)
			}
		})
	}
}

func TestPortInterruptedInfoKeepsPartialState(t *testing.T) {
	for _, halfClose := range []bool{false, true} {
		t.Run(fmt.Sprintf("half-close=%v", halfClose), func(t *testing.T) {
			m, f := newTestManager(t, Options{Dial: noDial(t), ConnectTimeout: 200 * time.Millisecond,
				Info: func(ctx context.Context, _ api.InterfaceRef, _ time.Duration) (api.ProxyInfo, error) {
					<-ctx.Done()
					return api.ProxyInfo{Interface: "Known adapter", Online: false, Changing: true, PublicIPv4: "192.0.2.1", CanChangeIP: true}, ctx.Err()
				},
			})
			p := testProxy(18080)
			p.ChangeIP = true
			applyTest(t, m, p)
			c := portRequest(t, f, p.Port, "GET", "/", "", "")
			if halfClose {
				_ = c.CloseWrite()
			}
			var info api.ProxyInfo
			portAnswer(t, c, 200, &info)
			if info.Interface != "Known adapter" || info.Online || !info.Changing || info.PublicIPv4 != "" || info.CanChangeIP {
				t.Fatalf("partial state was lost or claimed addresses: %+v", info)
			}
		})
	}
}
