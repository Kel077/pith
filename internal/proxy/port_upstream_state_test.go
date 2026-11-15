package proxy

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"reflect"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Kel077/pith/internal/api"
)

func knownUpstreamInfo() api.ProxyInfo {
	return api.ProxyInfo{Pith: 1, Port: 18081, Exit: api.ExitInterface, Interface: "Known adapter",
		Online: true, PublicIPv4: "192.0.2.1", CanChangeIP: true}
}

func TestPortUpstreamErrorsKeepLastAnswer(t *testing.T) {
	for _, tc := range []struct {
		status, want int
		code         api.ErrorCode
	}{
		{503, 503, api.ErrBusy}, {508, 508, api.ErrInternal}, {500, 502, api.ErrNoStandby}, {501, 502, api.ErrUnsupported},
		{502, 502, api.ErrUpstreamUnreachable}, {502, 502, api.ErrUpstreamRefused},
	} {
		t.Run(fmt.Sprintf("%d/%s", tc.status, tc.code), func(t *testing.T) {
			var failing atomic.Bool
			peer := portUpstream(t, func(c net.Conn, req *http.Request) {
				if req.Method != "GET" {
					t.Error("a failed preflight still passed the change on")
				}
				if failing.Load() {
					writePortError(c, portProblem(tc.status, tc.code, "the upstream cannot answer now"))
				} else {
					writePortJSON(c, 200, knownUpstreamInfo(), "")
				}
			})
			m, f := newTestManager(t, Options{Dial: noDial(t), Upstream: peer.reach})
			p := upstreamProxy(18080, "example.com:18081")
			p.ChangeIP = true
			applyTest(t, m, p)
			var info api.ProxyInfo
			portAnswer(t, portRequest(t, f, p.Port, "GET", "/", "", ""), 200, &info)
			if info.Upstream == nil {
				t.Fatal("no initial upstream information")
			}
			failing.Store(true)
			portError(t, portRequest(t, f, p.Port, "GET", "/", "", ""), tc.want, tc.code)
			portError(t, portRequest(t, f, p.Port, "POST", "/change-ip", "", ""), tc.want, tc.code)
			if _, err := m.ChangeIP(context.Background(), p.Port); api.Code(err) != tc.code {
				t.Fatalf("local change = %v, want %s", err, tc.code)
			}
			if st := m.Status(context.Background())[0]; st.Error != nil || !reflect.DeepEqual(st.Upstream, info.Upstream) {
				t.Fatalf("the upstream's own error changed this port's availability: %+v", st)
			}
		})
	}
}

func TestPortSilentKnownUpstreamKeepsLastAnswer(t *testing.T) {
	var silent atomic.Bool
	peer := portUpstream(t, func(c net.Conn, _ *http.Request) {
		if silent.Load() {
			_, _ = io.Copy(io.Discard, c)
			return
		}
		writePortJSON(c, 200, knownUpstreamInfo(), "")
	})
	m, f := newTestManager(t, Options{Dial: noDial(t), Upstream: peer.reach, ConnectTimeout: 160 * time.Millisecond})
	p := upstreamProxy(18080, "example.com:18081")
	p.ChangeIP = true
	applyTest(t, m, p)
	var info api.ProxyInfo
	portAnswer(t, portRequest(t, f, p.Port, "GET", "/", "", ""), 200, &info)
	silent.Store(true)
	portError(t, portRequest(t, f, p.Port, "GET", "/", "", ""), 502, api.ErrUpstreamUnreachable)
	portError(t, portRequest(t, f, p.Port, "POST", "/change-ip", "", ""), 502, api.ErrUpstreamUnreachable)
	if _, err := m.ChangeIP(context.Background(), p.Port); api.Code(err) != api.ErrUpstreamUnreachable {
		t.Fatal(err)
	}
	if st := m.Status(context.Background())[0]; st.Error != nil || !reflect.DeepEqual(st.Upstream, info.Upstream) {
		t.Fatalf("a silent lookup changed this port's availability: %+v", st)
	}
}

func TestPortSilentUnknownUpstreamDoesNotMeanUnsupported(t *testing.T) {
	var greetings atomic.Int32
	peer := newPeer(t, func(c *net.TCPConn) {
		br := bufio.NewReader(c)
		first, err := br.Peek(1)
		if err != nil {
			return
		}
		if first[0] == 5 {
			var greeting [3]byte
			_, _ = io.ReadFull(br, greeting[:])
			greetings.Add(1)
			_, _ = c.Write([]byte{5, 0})
			return
		}
		if _, err := http.ReadRequest(br); err == nil {
			_, _ = io.Copy(io.Discard, br)
		}
	})
	m, f := newTestManager(t, Options{Dial: noDial(t), Upstream: peer.reach, ConnectTimeout: 100 * time.Millisecond})
	p := upstreamProxy(18080, "example.com:18081")
	p.ChangeIP = true
	applyTest(t, m, p)
	var info api.ProxyInfo
	portAnswer(t, portRequest(t, f, p.Port, "GET", "/", "", ""), 200, &info)
	if !info.Online || info.Upstream != nil || greetings.Load() != 1 {
		t.Fatalf("a first GET did not fall back to the greeting: %+v, greetings %d", info, greetings.Load())
	}
	portError(t, portRequest(t, f, p.Port, "POST", "/change-ip", "", ""), 502, api.ErrUpstreamUnreachable)
	if _, err := m.ChangeIP(context.Background(), p.Port); api.Code(err) != api.ErrUpstreamUnreachable {
		t.Fatal(err)
	}
	if greetings.Load() != 1 {
		t.Fatal("a silent change preflight was mistaken for another kind of proxy")
	}
}

func TestPortSlowUpstreamChangeUsesConnectTimeout(t *testing.T) {
	var lookups, changes atomic.Int32
	far, farPorts := newTestManager(t, Options{Dial: noDial(t),
		Info: func(ctx context.Context, _ api.InterfaceRef, _ time.Duration) (api.ProxyInfo, error) {
			if lookups.Add(1) == 1 {
				timer := time.NewTimer(3200 * time.Millisecond)
				defer timer.Stop()
				select {
				case <-timer.C:
				case <-ctx.Done():
					return api.ProxyInfo{}, ctx.Err()
				}
			}
			info := knownUpstreamInfo()
			if changes.Load() != 0 {
				info.PublicIPv4 = "192.0.2.2"
			}
			return info, nil
		},
		ChangeIP: func(context.Context, api.InterfaceRef) (api.ProxyIPChange, error) {
			changes.Add(1)
			return api.ProxyIPChange{PublicIPv4: "192.0.2.2"}, nil
		},
	})
	fp := testProxy(18081)
	fp.ChangeIP = true
	applyTest(t, far, fp)
	m, f := newTestManager(t, Options{Dial: noDial(t), Upstream: farPorts.upstream(t, fp.Port, "example.com:18081", "")})
	p := upstreamProxy(18080, "example.com:18081")
	p.ChangeIP = true
	applyTest(t, m, p)
	c := portRequest(t, f, p.Port, "POST", "/change-ip", "", "")
	_ = c.SetDeadline(time.Now().Add(12 * time.Second))
	var changed api.ProxyIPChange
	portAnswer(t, c, 200, &changed)
	if changed.PublicIPv4 != "192.0.2.2" || changes.Load() != 1 {
		t.Fatalf("a slow preflight prevented the change: %+v, changes %d", changed, changes.Load())
	}
	if st := m.Status(context.Background())[0]; st.Upstream == nil || st.Upstream.PublicIPv4 != "192.0.2.2" {
		t.Fatalf("the completed change was not refreshed: %+v", st.Upstream)
	}
}

func TestPortCallerHopLimitPreservesUpstreamState(t *testing.T) {
	for _, known := range []bool{false, true} {
		t.Run(fmt.Sprint(known), func(t *testing.T) {
			peer := portUpstream(t, func(c net.Conn, _ *http.Request) { writePortJSON(c, 200, knownUpstreamInfo(), "") })
			var down atomic.Bool
			var calls atomic.Int32
			m, f := newTestManager(t, Options{Dial: noDial(t), Upstream: func(ctx context.Context, network, address string) (net.Conn, error) {
				calls.Add(1)
				if down.Load() {
					return nil, errors.New("unavailable")
				}
				return peer.reach(ctx, network, address)
			}})
			p := upstreamProxy(18080, "example.com:18081")
			p.ChangeIP = true
			applyTest(t, m, p)
			if known {
				portAnswer(t, portRequest(t, f, p.Port, "GET", "/", "", ""), 200, nil)
			}
			down.Store(true)
			if known {
				if code, _ := ask(t, f.dial(t, p.Port), Target{Host: "192.0.2.9", Port: 443}); code != replyNetwork {
					t.Fatalf("unavailable upstream reply = %d", code)
				}
			} else {
				portAnswer(t, portRequest(t, f, p.Port, "GET", "/", "", ""), 200, nil)
			}
			m.mu.Lock()
			before := m.byPort[p.Port].up
			m.mu.Unlock()
			beforeCalls := calls.Load()
			portError(t, portRequest(t, f, p.Port, "GET", "/", "Pith-Hops: 4\r\n", ""), 508, api.ErrInternal)
			portError(t, portRequest(t, f, p.Port, "POST", "/change-ip", "Pith-Hops: 4\r\n", ""), 508, api.ErrInternal)
			m.mu.Lock()
			after := m.byPort[p.Port].up
			m.mu.Unlock()
			if !reflect.DeepEqual(before, after) || calls.Load() != beforeCalls {
				t.Fatalf("a caller's hop count changed the upstream: before %+v, after %+v", before, after)
			}
		})
	}
}
