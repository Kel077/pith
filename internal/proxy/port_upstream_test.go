package proxy

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/Kel077/pith/internal/api"
)

func portUpstream(t *testing.T, handle func(net.Conn, *http.Request)) *loopbackPeer {
	t.Helper()
	return newPeer(t, func(c *net.TCPConn) {
		req, err := http.ReadRequest(bufio.NewReader(c))
		if err != nil {
			t.Errorf("upstream could not read request: %v", err)
			return
		}
		handle(c, req)
	})
}

func TestPortUpstreamChainAndBothFlags(t *testing.T) {
	for _, farAllowed := range []bool{false, true} {
		for _, nearAllowed := range []bool{false, true} {
			t.Run(fmt.Sprintf("near=%v/far=%v", nearAllowed, farAllowed), func(t *testing.T) {
				ages := make(chan time.Duration, 16)
				var changed atomic.Bool
				far, farPorts := newTestManager(t, Options{Dial: noDial(t),
					Info: func(_ context.Context, _ api.InterfaceRef, age time.Duration) (api.ProxyInfo, error) {
						ages <- age
						ip := "192.0.2.1"
						if changed.Load() {
							ip = "192.0.2.2"
						}
						return api.ProxyInfo{Interface: "Example adapter", Online: true, PublicIPv4: ip,
							PublicIPv6: "2001:db8::1", At: time.Unix(100, 0).UTC(), CanChangeIP: true}, nil
					},
					ChangeIP: func(context.Context, api.InterfaceRef) (api.ProxyIPChange, error) {
						changed.Store(true)
						return api.ProxyIPChange{BeforeIPv4: "192.0.2.1", PublicIPv4: "192.0.2.2", Seconds: 2}, nil
					},
				})
				fp := testProxy(18081)
				fp.ChangeIP = farAllowed
				applyTest(t, far, fp)
				near, nearPorts := newTestManager(t, Options{Dial: noDial(t), Upstream: farPorts.upstream(t, fp.Port, "192.0.2.7:18081", "")})
				np := upstreamProxy(18080, "192.0.2.7:18081")
				np.ChangeIP = nearAllowed
				applyTest(t, near, np)
				var info api.ProxyInfo
				portAnswer(t, portRequest(t, nearPorts, np.Port, "GET", "/?max-age=60", "", ""), 200, &info)
				if age := <-ages; age != time.Minute {
					t.Fatalf("upstream got max-age %v", age)
				}
				if info.Pith != 1 || info.Port != np.Port || info.Exit != api.ExitUpstream || info.Interface != "" ||
					!info.Online || info.PublicIPv4 != "192.0.2.1" || info.PublicIPv6 != "2001:db8::1" ||
					!info.At.Equal(time.Unix(100, 0)) || info.CanChangeIP != (nearAllowed && farAllowed) ||
					info.Upstream == nil || info.Upstream.Port != fp.Port || info.Upstream.CanChangeIP != farAllowed {
					t.Fatalf("chain info = %+v, upstream %+v", info, info.Upstream)
				}
				c := portRequest(t, nearPorts, np.Port, "POST", "/change-ip", "", "")
				if nearAllowed && farAllowed {
					var result api.ProxyIPChange
					portAnswer(t, c, 200, &result)
					if result.Port != np.Port || result.PublicIPv4 != "192.0.2.2" || result.BeforeIPv4 != "192.0.2.1" || result.Seconds != 2 {
						t.Fatalf("chain change = %+v", result)
					}
					if st := near.Status(context.Background())[0]; st.Upstream == nil || st.Upstream.PublicIPv4 != "192.0.2.2" {
						t.Fatalf("upstream was not refreshed after change: %+v", st)
					}
				} else {
					portError(t, c, 403, api.ErrNotAllowed)
				}
				// This Mac's user bypasses only this port's flag.
				result, err := near.ChangeIP(context.Background(), np.Port)
				if farAllowed {
					if err != nil || result.Port != np.Port || result.PublicIPv4 != "192.0.2.2" {
						t.Fatalf("local chain change = %+v, %v", result, err)
					}
				} else if api.Code(err) != api.ErrNotAllowed {
					t.Fatalf("local change bypassed upstream flag: %v", err)
				}
				st := near.Status(context.Background())[0]
				if st.Upstream == nil {
					t.Fatal("Status has no upstream info")
				}
				st.Upstream.Interface = "changed by reader"
				if next := near.Status(context.Background())[0]; next.Upstream.Interface == st.Upstream.Interface {
					t.Fatal("Status exposed mutable upstream info")
				}
			})
		}
	}
}

func TestPortUpstreamChangingAndRefresh(t *testing.T) {
	entered, release := make(chan struct{}), make(chan struct{})
	var changing, changed atomic.Bool
	far, farPorts := newTestManager(t, Options{Dial: noDial(t),
		Info: func(context.Context, api.InterfaceRef, time.Duration) (api.ProxyInfo, error) {
			ip := "192.0.2.1"
			if changed.Load() {
				ip = "192.0.2.2"
			}
			return api.ProxyInfo{Online: true, PublicIPv4: ip, CanChangeIP: true, Changing: changing.Load()}, nil
		},
		Changing: func(api.InterfaceRef) bool { return changing.Load() },
		ChangeIP: func(ctx context.Context, _ api.InterfaceRef) (api.ProxyIPChange, error) {
			changing.Store(true)
			close(entered)
			select {
			case <-release:
				changed.Store(true)
				changing.Store(false)
				return api.ProxyIPChange{PublicIPv4: "192.0.2.2"}, nil
			case <-ctx.Done():
				return api.ProxyIPChange{}, ctx.Err()
			}
		},
	})
	fp := testProxy(18081)
	fp.ChangeIP = true
	applyTest(t, far, fp)
	near, nearPorts := newTestManager(t, Options{Dial: noDial(t), Upstream: farPorts.upstream(t, fp.Port, "192.0.2.7:18081", "")})
	np := upstreamProxy(18080, "192.0.2.7:18081")
	np.ChangeIP = true
	applyTest(t, near, np)
	c := portRequest(t, nearPorts, np.Port, "POST", "/change-ip", "", "")
	<-entered
	if !near.Status(context.Background())[0].Changing || !far.Status(context.Background())[0].Changing {
		t.Fatal("Status doesn't report a change being passed on")
	}
	var info api.ProxyInfo
	portAnswer(t, portRequest(t, nearPorts, np.Port, "GET", "/", "", ""), 200, &info)
	if !info.Changing || info.Upstream == nil || !info.Upstream.Changing {
		t.Fatalf("changing info = %+v, upstream %+v", info, info.Upstream)
	}
	close(release)
	portAnswer(t, c, 200, nil)
	if st := near.Status(context.Background())[0]; st.Changing || st.Upstream == nil || st.Upstream.Changing || st.Upstream.PublicIPv4 != "192.0.2.2" {
		t.Fatalf("after change = %+v, upstream %+v", st, st.Upstream)
	}
}

func TestPortUpstreamNonPithFallback(t *testing.T) {
	for _, greeting := range []bool{true, false} {
		peer := socksPeer(t, greeting, nil)
		m, f := newTestManager(t, Options{Dial: noDial(t), Upstream: peer.reach, ConnectTimeout: 80 * time.Millisecond})
		p := upstreamProxy(18080, "192.0.2.7:18081")
		p.ChangeIP = true
		applyTest(t, m, p)
		var info api.ProxyInfo
		portAnswer(t, portRequest(t, f, p.Port, "GET", "/", "", ""), 200, &info)
		if info.Online != greeting || info.Upstream != nil || info.CanChangeIP || info.PublicIPv4 != "" || !info.At.IsZero() {
			t.Fatalf("non-Pith info = %+v", info)
		}
		if greeting {
			portError(t, portRequest(t, f, p.Port, "POST", "/change-ip", "", ""), 501, api.ErrUnsupported)
			if _, err := m.ChangeIP(context.Background(), p.Port); api.Code(err) != api.ErrUnsupported {
				t.Fatal(err)
			}
		} else {
			portError(t, portRequest(t, f, p.Port, "POST", "/change-ip", "", ""), 502, api.ErrUpstreamUnreachable)
		}
		if st := m.Status(context.Background())[0]; st.Upstream != nil {
			t.Fatalf("ordinary proxy exposed Pith info: %+v", st.Upstream)
		}
	}
}

func TestPortUpstreamUnreachableAndRefused(t *testing.T) {
	far, farPorts := newTestManager(t, Options{Dial: noDial(t)})
	shareTest(t, far, nil, sharedProxy(18081))
	for _, refused := range []bool{true, false} {
		reach := farPorts.upstream(t, 18081, "192.0.2.7:18081", "192.0.2.30")
		m, f := newTestManager(t, Options{Dial: noDial(t), Upstream: func(ctx context.Context, network, address string) (net.Conn, error) {
			if !refused {
				return nil, syscall.ECONNREFUSED
			}
			return reach(ctx, network, address)
		}})
		p := upstreamProxy(18080, "192.0.2.7:18081")
		p.ChangeIP = true
		applyTest(t, m, p)
		code := api.ErrUpstreamUnreachable
		if refused {
			code = api.ErrUpstreamRefused
		}
		var info api.ProxyInfo
		portAnswer(t, portRequest(t, f, p.Port, "GET", "/", "", ""), 200, &info)
		if info.Online || info.Upstream != nil || info.CanChangeIP {
			t.Fatalf("unavailable upstream: %+v", info)
		}
		portError(t, portRequest(t, f, p.Port, "POST", "/change-ip", "", ""), 502, code)
		if _, err := m.ChangeIP(context.Background(), p.Port); api.Code(err) != code {
			t.Fatalf("local change = %v, want %s", err, code)
		}
	}
}

func TestPortUpstreamRelaysChangeFailures(t *testing.T) {
	for _, tc := range []struct {
		name, body string
		status     int
		want       int
		code       api.ErrorCode
	}{
		{"flag off", "{\"error\":{\"code\":\"not_allowed\"}}", 403, 403, api.ErrNotAllowed},
		{"device refused", "", 403, 502, api.ErrUpstreamRefused},
		{"unsupported", "{\"error\":{\"code\":\"unsupported\"}}", 501, 501, api.ErrUnsupported},
		{"failed", "{\"error\":{\"code\":\"no_standby\",\"detail\":\"no standby exit\"}}", 502, 502, api.ErrNoStandby},
		{"malformed failure", "not json", 502, 502, api.ErrInternal},
		{"invalid success", "{}", 200, 502, api.ErrInternal},
		{"loop", "", 508, 508, api.ErrInternal},
		{"full", "", 503, 503, api.ErrInternal},
		{"not Pith anymore", "", 404, 501, api.ErrUnsupported},
		{"left unanswered", "", 0, 502, api.ErrUpstreamUnreachable},
	} {
		t.Run(tc.name, func(t *testing.T) {
			up := portUpstream(t, func(c net.Conn, req *http.Request) {
				if req.Method == "GET" {
					writePortJSON(c, 200, api.ProxyInfo{Pith: 1, Port: 18081, Online: true, CanChangeIP: true}, "")
					return
				}
				if tc.status == 0 {
					return
				}
				fmt.Fprintf(c, "HTTP/1.1 %d %s\r\nContent-Type: application/json\r\nContent-Length: %d\r\nConnection: close\r\n\r\n%s", tc.status, http.StatusText(tc.status), len(tc.body), tc.body)
			})
			m, f := newTestManager(t, Options{Dial: noDial(t), Upstream: up.reach})
			p := upstreamProxy(18080, "192.0.2.7:18081")
			p.ChangeIP = true
			applyTest(t, m, p)
			portError(t, portRequest(t, f, p.Port, "POST", "/change-ip", "", ""), tc.want, tc.code)
			if _, err := m.ChangeIP(context.Background(), p.Port); api.Code(err) != tc.code {
				t.Fatalf("local failure = %v, want %s", err, tc.code)
			} else if _, ok := err.(*api.Error); !ok {
				t.Fatalf("not an api.Error: %T", err)
			}
		})
	}
}

func TestPortUpstreamHeadersAndBounds(t *testing.T) {
	for _, tc := range []struct{ name, answer string }{
		{"not JSON", "HTTP/1.1 200 OK\r\nContent-Type: text/plain\r\nContent-Length: 10\r\n\r\n{\"pith\":1}"},
		{"version zero", "HTTP/1.1 200 OK\r\nContent-Type: application/json\r\nContent-Length: 10\r\n\r\n{\"pith\":0}"},
		{"oversized body", "HTTP/1.1 200 OK\r\nContent-Type: application/json\r\nContent-Length: 1000000\r\n\r\n" + strings.Repeat("a", maxUpstreamInfo+1)},
		{"oversized header", "HTTP/1.1 200 OK\r\nX-Long: " + strings.Repeat("a", maxUpstreamInfo+1) + "\r\nContent-Length: 0\r\n\r\n"},
		{"never answers", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			// One port speaks HTTP or SOCKS as its first byte requests.
			peer := newPeer(t, func(c *net.TCPConn) {
				br := bufio.NewReader(c)
				first, _ := br.Peek(1)
				if len(first) != 0 && first[0] == 5 {
					var greeting [3]byte
					_, _ = io.ReadFull(br, greeting[:])
					_, _ = c.Write([]byte{5, 0})
					return
				}
				if _, err := http.ReadRequest(br); err != nil {
					return
				}
				_, _ = io.WriteString(c, tc.answer)
				if tc.answer == "" {
					_, _ = io.Copy(io.Discard, br)
				}
			})
			m, f := newTestManager(t, Options{Dial: noDial(t), Upstream: peer.reach, ConnectTimeout: 60 * time.Millisecond})
			p := upstreamProxy(18080, "192.0.2.7:18081")
			applyTest(t, m, p)
			var info api.ProxyInfo
			portAnswer(t, portRequest(t, f, p.Port, "GET", "/", "", ""), 200, &info)
			if !info.Online || info.Upstream != nil || info.CanChangeIP {
				t.Fatalf("did not fall back to SOCKS: %+v", info)
			}
		})
	}
	seen := make(chan *http.Request, 1)
	peer := portUpstream(t, func(c net.Conn, req *http.Request) {
		seen <- req
		writePortJSON(c, 200, api.ProxyInfo{Pith: 2, Port: 18081, Online: true}, "")
	})
	m, f := newTestManager(t, Options{Dial: noDial(t), Upstream: peer.reach})
	applyTest(t, m, upstreamProxy(18080, "example.com:18081"))
	var info api.ProxyInfo
	portAnswer(t, portRequest(t, f, 18080, "GET", "/?max-age=12", "Pith-Hops: 2\r\nX-Private: keep here\r\n", ""), 200, &info)
	req := <-seen
	if req.RequestURI != "/?max-age=12" || req.Host != "example.com:18081" || req.Header.Get("Pith-Hops") != "3" ||
		req.Header.Get("X-Private") != "" || req.Header.Get("User-Agent") != "" || !req.Close || info.Upstream == nil || info.Upstream.Pith != 2 {
		t.Fatalf("forwarded request = %+v, response %+v", req, info)
	}
}

func TestPortUpstreamHopLimitAndLoop(t *testing.T) {
	var firstPorts, secondPorts *listenerFixture
	var forwarded atomic.Int32
	first, firstPorts := newTestManager(t, Options{Dial: noDial(t), Upstream: func(ctx context.Context, network, address string) (net.Conn, error) {
		forwarded.Add(1)
		return secondPorts.upstream(t, 18081, "192.0.2.8:18081", "")(ctx, network, address)
	}})
	second, secondPorts := newTestManager(t, Options{Dial: noDial(t), Upstream: func(ctx context.Context, network, address string) (net.Conn, error) {
		forwarded.Add(1)
		return firstPorts.upstream(t, 18080, "192.0.2.7:18080", "")(ctx, network, address)
	}})
	a, b := upstreamProxy(18080, "192.0.2.8:18081"), upstreamProxy(18081, "192.0.2.7:18080")
	a.ChangeIP, b.ChangeIP = true, true
	applyTest(t, first, a)
	applyTest(t, second, b)
	for _, request := range []struct{ method, path string }{{"GET", "/"}, {"POST", "/change-ip"}} {
		before := forwarded.Load()
		portError(t, portRequest(t, firstPorts, a.Port, request.method, request.path, "", ""), 508, api.ErrInternal)
		if got := forwarded.Load() - before; got != api.MaxProxyHops {
			t.Fatalf("loop forwarded %d requests, want %d", got, api.MaxProxyHops)
		}
	}
	for _, count := range []string{"4", "5", "18446744073709551616"} {
		before := forwarded.Load()
		portError(t, portRequest(t, firstPorts, a.Port, "GET", "/", "Pith-Hops: "+count+"\r\n", ""), 508, api.ErrInternal)
		if forwarded.Load() != before {
			t.Fatal("hop limit still passed the request on")
		}
	}
	for _, count := range []string{"", "-1", "one", "1, 2", "1\r\nPith-Hops: 2"} {
		portError(t, portRequest(t, firstPorts, a.Port, "GET", "/", "Pith-Hops: "+count+"\r\n", ""), 400, api.ErrInvalidConfig)
	}
	if _, err := first.ChangeIP(context.Background(), a.Port); api.Code(err) != api.ErrInternal {
		t.Fatalf("local loop change = %v", err)
	}
	waitFor(t, func() bool {
		first.mu.Lock()
		defer first.mu.Unlock()
		second.mu.Lock()
		defer second.mu.Unlock()
		return first.requests == 0 && second.requests == 0
	})
}

func TestPortSharedUpstreamOnThisMacGetsNoRequest(t *testing.T) {
	var told atomic.Int32
	peer := newPeer(t, func(c *net.TCPConn) {
		var b [1]byte
		if n, _ := c.Read(b[:]); n > 0 {
			told.Add(1)
		}
	})
	m, f := newTestManager(t, Options{Dial: noDial(t), Upstream: peer.reach})
	p := upstreamProxy(18080, "example.com:18081")
	p.Shared, p.ChangeIP = true, true
	shareTest(t, m, []string{"192.0.2.10"}, p)
	var info api.ProxyInfo
	portAnswer(t, portRequest(t, f, p.Port, "GET", "/", "", ""), 200, &info)
	if info.Online || info.Upstream != nil || info.CanChangeIP {
		t.Fatalf("shared port went through this Mac: %+v", info)
	}
	portError(t, portRequest(t, f, p.Port, "POST", "/change-ip", "", ""), 502, api.ErrUpstreamRefused)
	if _, err := m.ChangeIP(context.Background(), p.Port); api.Code(err) != api.ErrUpstreamRefused {
		t.Fatal(err)
	}
	if told.Load() != 0 {
		t.Fatal("an upstream on this Mac was sent a request")
	}
}

func TestPortUpstreamStatusRefreshesDuringTraffic(t *testing.T) {
	var revision atomic.Int32
	var ages atomic.Int32
	far, farPorts := newTestManager(t, Options{Dial: echoPeer(t).dial, Info: func(_ context.Context, _ api.InterfaceRef, age time.Duration) (api.ProxyInfo, error) {
		if age >= 0 {
			t.Errorf("Status poll supplied max-age %s", age)
		}
		ages.Add(1)
		return api.ProxyInfo{Online: true, PublicIPv4: fmt.Sprintf("192.0.2.%d", revision.Load()+1), Changing: revision.Load() == 1}, nil
	}})
	applyTest(t, far, testProxy(18081))
	reach := farPorts.upstream(t, 18081, "192.0.2.7:18081", "")
	socks := socksPeer(t, true, nil)
	var ordinary atomic.Bool
	near, nearPorts := newTestManager(t, Options{Dial: noDial(t), ConnectTimeout: 60 * time.Millisecond,
		Upstream: func(ctx context.Context, network, address string) (net.Conn, error) {
			if ordinary.Load() {
				return socks.reach(ctx, network, address)
			}
			return reach(ctx, network, address)
		},
	})
	p := upstreamProxy(18080, "192.0.2.7:18081")
	applyTest(t, near, p)
	waitFor(t, func() bool { return near.Status(context.Background())[0].Upstream != nil })
	if ages.Load() != 1 {
		t.Fatalf("Status polled %d times in one interval", ages.Load())
	}
	revision.Store(1)
	near.mu.Lock()
	near.byPort[p.Port].up.checked = time.Now().Add(-upstreamCheckEvery)
	near.mu.Unlock()
	for range 3 {
		c := connectTest(t, nearPorts, p.Port)
		roundTrip(t, c, "flowing")
	}
	waitFor(t, func() bool {
		st := near.Status(context.Background())[0]
		return st.Upstream != nil && st.Upstream.PublicIPv4 == "192.0.2.2" && st.Changing
	})
	if ages.Load() != 2 {
		t.Fatalf("active traffic suppressed or repeated polls: %d", ages.Load())
	}
	ordinary.Store(true)
	near.mu.Lock()
	near.byPort[p.Port].up.checked = time.Now().Add(-upstreamCheckEvery)
	near.mu.Unlock()
	waitFor(t, func() bool {
		st := near.Status(context.Background())[0]
		return st.Upstream == nil && !st.Changing && st.Error == nil
	})
	ordinary.Store(false)
	p.Upstream = "192.0.2.8:18081"
	// Reconfiguration clears the old snapshot before its next poll.
	applyTest(t, near, p)
	near.mu.Lock()
	stored := near.byPort[p.Port].up.info
	near.mu.Unlock()
	if stored != nil {
		t.Fatalf("reconfigured port kept info: %+v", stored)
	}
}

func TestPortUpstreamCanceledCaller(t *testing.T) {
	peer := portUpstream(t, func(c net.Conn, req *http.Request) {
		if req.Method == "GET" {
			writePortJSON(c, 200, api.ProxyInfo{Pith: 1, Port: 18081, Online: true, CanChangeIP: true}, "")
			return
		}
		_, _ = io.Copy(io.Discard, c)
	})
	m, _ := newTestManager(t, Options{Dial: noDial(t), Upstream: peer.reach})
	applyTest(t, m, upstreamProxy(18080, "192.0.2.7:18081"))
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()
	if _, err := m.ChangeIP(ctx, 18080); api.Code(err) != api.ErrUpstreamUnreachable {
		t.Fatalf("canceled upstream change = %v", err)
	}
	if err := m.Close(); err != nil && !errors.Is(err, net.ErrClosed) {
		t.Fatal(err)
	}
}

func TestPortUpstreamDepartureKeepsLastInfo(t *testing.T) {
	entered, canceled := make(chan struct{}), make(chan struct{})
	far, farPorts := newTestManager(t, Options{Dial: noDial(t),
		Info: func(context.Context, api.InterfaceRef, time.Duration) (api.ProxyInfo, error) {
			return api.ProxyInfo{Online: true, CanChangeIP: true, PublicIPv4: "192.0.2.1"}, nil
		},
		ChangeIP: func(ctx context.Context, _ api.InterfaceRef) (api.ProxyIPChange, error) {
			close(entered)
			<-ctx.Done()
			close(canceled)
			return api.ProxyIPChange{}, ctx.Err()
		},
	})
	fp := testProxy(18081)
	fp.ChangeIP = true
	applyTest(t, far, fp)
	near, nearPorts := newTestManager(t, Options{Dial: noDial(t), Upstream: farPorts.upstream(t, fp.Port, "192.0.2.7:18081", "")})
	np := upstreamProxy(18080, "192.0.2.7:18081")
	np.ChangeIP = true
	applyTest(t, near, np)
	c := portRequest(t, nearPorts, np.Port, "POST", "/change-ip", "", "")
	<-entered
	_ = c.Close()
	select {
	case <-canceled:
	case <-time.After(time.Second):
		t.Fatal("upstream kept a departed caller waiting")
	}
	waitFor(t, func() bool { near.mu.Lock(); defer near.mu.Unlock(); return near.requests == 0 })
	near.mu.Lock()
	up := near.byPort[np.Port].up
	near.mu.Unlock()
	if up.info == nil || up.info.PublicIPv4 != "192.0.2.1" || up.err != nil || !up.checked.IsZero() || up.changing != 0 {
		t.Fatalf("departed caller invalidated the upstream: %+v", up)
	}
}
