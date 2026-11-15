package proxy

import (
	"bufio"
	"context"
	"io"
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/Kel077/pith/internal/api"
)

func upstreamInfoTree(depth int) *api.ProxyInfo {
	info := knownUpstreamInfo()
	if depth > 1 {
		info.Exit, info.Interface = api.ExitUpstream, ""
		info.Upstream = upstreamInfoTree(depth - 1)
	}
	return &info
}

func infoUpstream(t *testing.T, load func() *api.ProxyInfo) *loopbackPeer {
	t.Helper()
	return newPeer(t, func(c *net.TCPConn) {
		br := bufio.NewReader(c)
		first, err := br.Peek(1)
		if err != nil {
			return
		}
		if first[0] == 5 {
			var greeting [3]byte
			_, _ = io.ReadFull(br, greeting[:])
			_, _ = c.Write([]byte{5, 0})
			return
		}
		req, err := http.ReadRequest(br)
		if err != nil {
			t.Error(err)
			return
		}
		if req.Method != "GET" {
			t.Error("an invalid upstream answer still received a change request")
		}
		writePortJSON(c, 200, load(), "")
	})
}

func TestPortRejectsInvalidUpstreamInformation(t *testing.T) {
	for _, tc := range []struct {
		name string
		edit func(*api.ProxyInfo)
	}{
		{"zero port", func(info *api.ProxyInfo) { info.Port = 0 }},
		{"port too large", func(info *api.ProxyInfo) { info.Port = 65536 }},
		{"unknown exit", func(info *api.ProxyInfo) { info.Exit = "unknown" }},
		{"missing exit", func(info *api.ProxyInfo) { info.Exit = "" }},
		{"nested port", func(info *api.ProxyInfo) { info.Upstream = upstreamInfoTree(1); info.Upstream.Port = -1 }},
		{"nested version", func(info *api.ProxyInfo) { info.Upstream = upstreamInfoTree(1); info.Upstream.Pith = 0 }},
		{"nested exit", func(info *api.ProxyInfo) { info.Upstream = upstreamInfoTree(1); info.Upstream.Exit = "unknown" }},
		{"too many hops", func(info *api.ProxyInfo) { *info = *upstreamInfoTree(api.MaxProxyHops + 1) }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var answer atomic.Pointer[api.ProxyInfo]
			good := knownUpstreamInfo()
			answer.Store(&good)
			peer := infoUpstream(t, answer.Load)
			m, f := newTestManager(t, Options{Dial: noDial(t), Upstream: peer.reach})
			p := upstreamProxy(18080, "example.com:18081")
			p.ChangeIP = true
			applyTest(t, m, p)
			var info api.ProxyInfo
			portAnswer(t, portRequest(t, f, p.Port, "GET", "/", "", ""), 200, &info)
			if info.Upstream == nil {
				t.Fatal("the initial valid information was not kept")
			}
			bad := knownUpstreamInfo()
			tc.edit(&bad)
			answer.Store(&bad)
			info = api.ProxyInfo{}
			portAnswer(t, portRequest(t, f, p.Port, "GET", "/", "", ""), 200, &info)
			if !info.Online || info.Upstream != nil || info.CanChangeIP || info.PublicIPv4 != "" {
				t.Fatalf("invalid information was passed on: %+v", info)
			}
			if st := m.Status(context.Background())[0]; st.Upstream != nil {
				t.Fatalf("invalid information was cached: %+v", st.Upstream)
			}
			portError(t, portRequest(t, f, p.Port, "POST", "/change-ip", "", ""), 501, api.ErrUnsupported)
		})
	}
}

func TestPortSanitizesUpstreamInformation(t *testing.T) {
	tree := upstreamInfoTree(api.MaxProxyHops)
	tree.Interface = "an upstream has no interface name"
	tree.PublicIPv4 = "not an IP address"
	tree.PublicIPv6 = "2001:0db8:0:0:0:0:0:1"
	tree.At = time.Date(2026, 1, 2, 3, 4, 5, 0, time.FixedZone("Example", 3600))
	tree.Upstream.PublicIPv4 = "2001:db8::1"
	tree.Upstream.PublicIPv6 = "2001:db8::1%example0"
	last := tree
	for last.Upstream != nil {
		last = last.Upstream
	}
	last.Interface = strings.Repeat("界", 200)
	peer := infoUpstream(t, func() *api.ProxyInfo { return tree })
	m, f := newTestManager(t, Options{Dial: noDial(t), Upstream: peer.reach})
	applyTest(t, m, upstreamProxy(18080, "example.com:18081"))
	var info api.ProxyInfo
	portAnswer(t, portRequest(t, f, 18080, "GET", "/", "", ""), 200, &info)
	if info.Upstream == nil || info.PublicIPv4 != "" || info.PublicIPv6 != "2001:db8::1" ||
		info.Upstream.Interface != "" || !info.At.Equal(tree.At) || info.At.Location() != time.UTC {
		t.Fatalf("upstream fields were not sanitized: %+v", info)
	}
	if child := info.Upstream.Upstream; child.PublicIPv4 != "" || child.PublicIPv6 != "" {
		t.Fatalf("wrong-family or scoped addresses were passed on: %+v", child)
	}
	depth := 1
	last = info.Upstream
	for last.Upstream != nil {
		depth++
		last = last.Upstream
	}
	if depth != api.MaxProxyHops || len(last.Interface) == 0 || len(last.Interface) > maxUpstreamInterface ||
		!utf8.ValidString(last.Interface) || !strings.HasPrefix(strings.Repeat("界", 200), last.Interface) {
		t.Fatalf("unbounded or invalid interface name at depth %d: %q", depth, last.Interface)
	}
	st := m.Status(context.Background())[0]
	if st.Upstream == nil || st.Upstream.PublicIPv4 != "" || st.Upstream.Upstream.PublicIPv6 != "" {
		t.Fatalf("unsanitized fields were cached: %+v", st.Upstream)
	}
}

func TestPortValidatesUpstreamChangeResults(t *testing.T) {
	for _, port := range []int{-1, 0, 1, 65535, 65536} {
		t.Run("port="+strconv.Itoa(port), func(t *testing.T) {
			at := time.Date(2026, 1, 2, 3, 4, 5, 0, time.FixedZone("Example", -3600))
			peer := portUpstream(t, func(c net.Conn, req *http.Request) {
				if req.Method == "GET" {
					writePortJSON(c, 200, knownUpstreamInfo(), "")
					return
				}
				writePortJSON(c, 200, api.ProxyIPChange{Port: port, BeforeIPv4: "invalid", BeforeIPv6: "192.0.2.1",
					PublicIPv4: "::ffff:192.0.2.2", PublicIPv6: "2001:0db8::2", At: at, Seconds: 1}, "")
			})
			m, f := newTestManager(t, Options{Dial: noDial(t), Upstream: peer.reach})
			p := upstreamProxy(18080, "example.com:18081")
			p.ChangeIP = true
			applyTest(t, m, p)
			c := portRequest(t, f, p.Port, "POST", "/change-ip", "", "")
			if port < 1 || port > 65535 {
				portError(t, c, 502, api.ErrInternal)
				if _, err := m.ChangeIP(context.Background(), p.Port); api.Code(err) != api.ErrInternal {
					t.Fatal(err)
				}
				return
			}
			var changed api.ProxyIPChange
			portAnswer(t, c, 200, &changed)
			if changed.Port != p.Port || changed.BeforeIPv4 != "" || changed.BeforeIPv6 != "" ||
				changed.PublicIPv4 != "192.0.2.2" || changed.PublicIPv6 != "2001:db8::2" ||
				!changed.At.Equal(at) || changed.At.Location() != time.UTC || changed.Seconds != 1 {
				t.Fatalf("invalid change fields were passed on: %+v", changed)
			}
		})
	}
}
