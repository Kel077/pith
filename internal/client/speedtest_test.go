package client

import (
	"context"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Kel077/pith/internal/api"
)

// fakeSpeed answers as Cloudflare's speed test does, with made-up values.
type fakeSpeed struct {
	mu     sync.Mutex
	downs  []int   // the bytes asked for
	ups    []int64 // the bytes received
	failUp bool
}

func (f *fakeSpeed) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	switch r.URL.Path {
	case "/__down":
		n, err := strconv.Atoi(r.URL.Query().Get("bytes"))
		if err != nil || n < 0 || n > speedDownload {
			http.Error(w, "forbidden", http.StatusForbidden)
			return
		}
		f.mu.Lock()
		f.downs = append(f.downs, n)
		f.mu.Unlock()
		w.Header().Set("Colo", "SJC")
		w.Header().Set("Cf-Meta-Ip", "198.51.100.9")
		w.Header().Set("Server-Timing", "cfSpeedEdge;dur=1, cfSpeedWorker;dur=2")
		w.Header().Set("Content-Length", strconv.Itoa(n))
		chunk := make([]byte, 64<<10)
		for n > 0 {
			k, err := w.Write(chunk[:min(n, len(chunk))])
			if err != nil {
				return
			}
			n -= k
		}
	case "/__up":
		n, _ := io.Copy(io.Discard, r.Body)
		f.mu.Lock()
		f.ups = append(f.ups, n)
		fail := f.failUp
		f.mu.Unlock()
		if fail {
			http.Error(w, "unavailable", http.StatusServiceUnavailable)
			return
		}
		w.Header().Set("Server-Timing", "cfSpeedEdge;dur=1, cfSpeedWorker;dur=3")
	default:
		http.NotFound(w, r)
	}
}

func speedClient(t *testing.T, server string) *Client {
	t.Helper()
	c := testClient(t)
	c.dialer = func(string, time.Duration, []string) (*net.Dialer, error) {
		return &net.Dialer{Timeout: time.Second}, nil
	}
	c.interfaces = func(context.Context) ([]api.Interface, error) {
		return []api.Interface{{Name: "en0", Label: "Example Ethernet", Up: true, IPv4: []string{"192.0.2.2"}}}, nil
	}
	c.speedURL = server
	return c
}

func TestSpeedTest(t *testing.T) {
	f := &fakeSpeed{}
	srv := httptest.NewServer(f)
	defer srv.Close()
	r, err := speedClient(t, srv.URL).SpeedTest(context.Background(), "en0")
	if err != nil || !r.OK || r.Problem != "" || r.Server != "SJC" || r.PublicIP != "198.51.100.9" || r.Download <= 0 || r.Upload <= 0 || r.Latency < 0 || r.Latency > time.Second {
		t.Fatalf("%+v %v", r, err)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if want := []int{0, 0, 0, 0, 0, speedDownload}; !slices.Equal(f.downs, want) {
		t.Errorf("downloads %v, want %v", f.downs, want)
	}
	if want := []int64{speedUpload}; !slices.Equal(f.ups, want) {
		t.Errorf("uploads %v, want %v", f.ups, want)
	}
}

func TestSpeedTestTellsAPartThatFailed(t *testing.T) {
	srv := httptest.NewServer(&fakeSpeed{failUp: true})
	defer srv.Close()
	r, err := speedClient(t, srv.URL).SpeedTest(context.Background(), "en0")
	if err != nil || !r.OK || r.Download <= 0 || r.Upload != 0 || !strings.Contains(r.Problem, "upload") {
		t.Fatalf("%+v %v", r, err)
	}
}

func TestSpeedTestWithoutTheServer(t *testing.T) {
	srv := httptest.NewServer(&fakeSpeed{})
	srv.Close()
	r, err := speedClient(t, srv.URL).SpeedTest(context.Background(), "en0")
	if err != nil || r.OK || r.Problem == "" || r.Download != 0 {
		t.Fatalf("%+v %v", r, err)
	}
}

func TestSpeedTestAnswerHeaders(t *testing.T) {
	h := http.Header{}
	h.Add("Server-Timing", "cfSpeedEdge;dur=4, cfSpeedWorker;dur=20.5")
	h.Add("Server-Timing", `cfL4;desc="?proto=TCP&rtt=4871&min_rtt=4023"`)
	if got := serverTime(h); got != 24500*time.Microsecond {
		t.Errorf("server time %v", got)
	}
	for _, tt := range []struct{ colo, ray, want string }{
		{"BOS", "", "BOS"},
		{"", "0123456789abcdef-EWR", "EWR"},
		{"<b>", "", ""},
		{"", "", ""},
	} {
		h := http.Header{}
		h.Set("Colo", tt.colo)
		h.Set("Cf-Ray", tt.ray)
		if got := serverPlace(h); got != tt.want {
			t.Errorf("colo %q, ray %q: %q", tt.colo, tt.ray, got)
		}
	}
	for raw, want := range map[string]string{"198.51.100.9": "198.51.100.9", "::ffff:198.51.100.9": "198.51.100.9", "2001:db8::9": "2001:db8::9", "127.0.0.1": "", "not an address": ""} {
		h := http.Header{}
		h.Set("Cf-Meta-Ip", raw)
		if got := publicIP(h); got != want {
			t.Errorf("%q: %q, want %q", raw, got, want)
		}
	}
}
