package proxy

import (
	"context"
	"fmt"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Kel077/pith/internal/api"
)

func TestPortChangeIPSwitchKeepsListener(t *testing.T) {
	for _, upstream := range []bool{false, true} {
		t.Run(fmt.Sprintf("upstream=%v", upstream), func(t *testing.T) {
			opts := Options{
				Dial: echoPeer(t).dial,
				Info: func(context.Context, api.InterfaceRef, time.Duration) (api.ProxyInfo, error) {
					return api.ProxyInfo{Online: true, CanChangeIP: true}, nil
				},
				ChangeIP: func(context.Context, api.InterfaceRef) (api.ProxyIPChange, error) {
					return api.ProxyIPChange{PublicIPv4: "192.0.2.2"}, nil
				},
			}
			p := testProxy(18080)
			if upstream {
				far, farPorts := newTestManager(t, opts)
				fp := testProxy(18081)
				fp.ChangeIP = true
				applyTest(t, far, fp)
				opts = Options{Dial: noDial(t), Upstream: farPorts.upstream(t, fp.Port, "192.0.2.7:18081", "")}
				p = upstreamProxy(p.Port, "192.0.2.7:18081")
			}
			m, f := newTestManager(t, opts)
			applyTest(t, m, p)
			stream := connectTest(t, f, p.Port)
			roundTrip(t, stream, "before")
			total := uint64(len("before"))
			waitFor(t, func() bool {
				st := m.Status(context.Background())[0]
				return st.Sent == total && st.Received == total
			})
			original := m.Status(context.Background())[0]
			for _, allowed := range []bool{false, true, false, true} {
				p.ChangeIP = allowed
				applyTest(t, m, p)
				st := m.Status(context.Background())[0]
				if st.Since != original.Since || st.Open != 1 || st.Sent != total || st.Received != total {
					t.Fatalf("ChangeIP=%v reset the listener: %+v", allowed, st)
				}
				var info api.ProxyInfo
				portAnswer(t, portRequest(t, f, p.Port, "GET", "/", "", ""), 200, &info)
				if info.CanChangeIP != allowed {
					t.Fatalf("ChangeIP=%v: info = %+v", allowed, info)
				}
				c := portRequest(t, f, p.Port, "POST", "/change-ip", "", "")
				if allowed {
					var changed api.ProxyIPChange
					portAnswer(t, c, 200, &changed)
					if changed.Port != p.Port || changed.PublicIPv4 != "192.0.2.2" {
						t.Fatalf("ChangeIP enabled: %+v", changed)
					}
				} else {
					portError(t, c, 403, api.ErrNotAllowed)
				}
				roundTrip(t, stream, "after")
				total += uint64(len("after"))
				waitFor(t, func() bool {
					st := m.Status(context.Background())[0]
					return st.Sent == total && st.Received == total
				})
				if st := m.Status(context.Background())[0]; st.Since != original.Since || st.Open != 1 {
					t.Fatalf("request or stream replaced the listener: %+v", st)
				}
			}
		})
	}
}

func TestPortChangeIPSwitchKeepsRequests(t *testing.T) {
	postEntered, infoEntered := make(chan struct{}, 2), make(chan struct{})
	releasePost, releaseInfo := make(chan struct{}), make(chan struct{})
	var blockInfo atomic.Bool
	m, f := newTestManager(t, Options{Dial: noDial(t),
		Info: func(ctx context.Context, _ api.InterfaceRef, _ time.Duration) (api.ProxyInfo, error) {
			if blockInfo.CompareAndSwap(true, false) {
				close(infoEntered)
				select {
				case <-releaseInfo:
				case <-ctx.Done():
					return api.ProxyInfo{}, ctx.Err()
				}
			}
			return api.ProxyInfo{Online: true, CanChangeIP: true}, nil
		},
		ChangeIP: func(ctx context.Context, _ api.InterfaceRef) (api.ProxyIPChange, error) {
			postEntered <- struct{}{}
			select {
			case <-releasePost:
				return api.ProxyIPChange{PublicIPv4: "192.0.2.2"}, nil
			case <-ctx.Done():
				return api.ProxyIPChange{}, ctx.Err()
			}
		},
	})
	p := testProxy(18080)
	p.ChangeIP = true
	applyTest(t, m, p)
	original := m.Status(context.Background())[0]
	post := portRequest(t, f, p.Port, "POST", "/change-ip", "", "")
	<-postEntered
	blockInfo.Store(true)
	get := portRequest(t, f, p.Port, "GET", "/", "", "")
	<-infoEntered
	p.ChangeIP = false
	applyTest(t, m, p)
	var info api.ProxyInfo
	portAnswer(t, portRequest(t, f, p.Port, "GET", "/", "", ""), 200, &info)
	if info.CanChangeIP {
		t.Fatal("a GET after disabling ChangeIP still allows it")
	}
	portError(t, portRequest(t, f, p.Port, "POST", "/change-ip", "", ""), 403, api.ErrNotAllowed)
	close(releaseInfo)
	portAnswer(t, get, 200, &info)
	if info.CanChangeIP {
		t.Fatal("a GET in progress did not observe the new permission")
	}
	close(releasePost)
	var changed api.ProxyIPChange
	portAnswer(t, post, 200, &changed)
	if changed.Port != p.Port || changed.PublicIPv4 != "192.0.2.2" {
		t.Fatalf("a POST in progress did not finish: %+v", changed)
	}
	if st := m.Status(context.Background())[0]; st.Since != original.Since || st.Open != 0 || st.Sent != 0 || st.Received != 0 {
		t.Fatalf("changing permission reset state: %+v", st)
	}
}
