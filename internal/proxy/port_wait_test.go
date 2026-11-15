package proxy

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/Kel077/pith/internal/api"
)

func assertChangeWaitEnded(t *testing.T, err error) {
	t.Helper()
	var problem *api.Error
	if !errors.As(err, &problem) || problem.Code != api.ErrInternal ||
		!strings.Contains(problem.Detail, "wait") || !strings.Contains(problem.Detail, "may still be in progress") ||
		strings.Contains(problem.Detail, "failed") {
		t.Fatalf("ended wait was reported as a failed change: %v", err)
	}
}

func TestPortChangeWaitEndsOnCallerEOF(t *testing.T) {
	for _, upstream := range []bool{false, true} {
		t.Run(fmt.Sprintf("upstream=%v", upstream), func(t *testing.T) {
			entered := make(chan struct{})
			opts := Options{Dial: noDial(t), ChangeIP: func(ctx context.Context, _ api.InterfaceRef) (api.ProxyIPChange, error) {
				close(entered)
				<-ctx.Done()
				return api.ProxyIPChange{}, fmt.Errorf("wait ended: %w", ctx.Err())
			}}
			p := testProxy(18080)
			if upstream {
				far, farPorts := newTestManager(t, opts)
				fp := testProxy(18081)
				fp.ChangeIP = true
				applyTest(t, far, fp)
				opts = Options{Dial: noDial(t), Upstream: farPorts.upstream(t, fp.Port, "example.com:18081", "")}
				p = upstreamProxy(p.Port, "example.com:18081")
			}
			m, f := newTestManager(t, opts)
			p.ChangeIP = true
			applyTest(t, m, p)
			c := portRequest(t, f, p.Port, "POST", "/change-ip", "", "")
			waitPortRequestStarted(t, entered)
			_ = c.CloseWrite()
			var body errorBody
			portAnswer(t, c, 502, &body)
			assertChangeWaitEnded(t, body.Error)
		})
	}
}

func TestPortChangeWaitEndsAtCallerDeadline(t *testing.T) {
	m, _ := newTestManager(t, Options{Dial: noDial(t), ChangeIP: func(ctx context.Context, _ api.InterfaceRef) (api.ProxyIPChange, error) {
		<-ctx.Done()
		return api.ProxyIPChange{}, ctx.Err()
	}})
	applyTest(t, m, testProxy(18080))
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()
	_, err := m.ChangeIP(ctx, 18080)
	assertChangeWaitEnded(t, err)
}

func TestPortUpstreamPreflightWaitEndsAtCallerDeadline(t *testing.T) {
	peer := portUpstream(t, func(c net.Conn, req *http.Request) {
		if req.Method != "GET" {
			t.Error("a canceled preflight passed the change on")
		}
		_, _ = io.Copy(io.Discard, c)
	})
	m, _ := newTestManager(t, Options{Dial: noDial(t), Upstream: peer.reach})
	applyTest(t, m, upstreamProxy(18080, "example.com:18081"))
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()
	_, err := m.ChangeIP(ctx, 18080)
	assertChangeWaitEnded(t, err)
}
