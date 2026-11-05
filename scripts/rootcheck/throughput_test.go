package main

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"errors"
	"fmt"
	"io"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/Kel077/pith/internal/api"
	"github.com/Kel077/pith/internal/engine/capture"
	"github.com/Kel077/pith/internal/testprofile"
)

func throughputFixture(t *testing.T) *session {
	t.Helper()
	s, _, _ := baselineFixture(t)
	s.Throughput = &throughputPlan{Target: "192.0.2.30:443", ServerName: "example.com", Adapters: [2]throughputBudget{{Limit: 64 << 20, Download: 1 << 20, Upload: 128 << 10}, {Limit: 64 << 20, Download: 2 << 20, Upload: 256 << 10}}}
	if err := s.writeThroughputPlan(); err != nil {
		t.Fatal(err)
	}
	return s
}

func TestThroughputPlanAndBudgetCannotBeReused(t *testing.T) {
	s := throughputFixture(t)
	if err := s.reviewedThroughputPlan(); err != nil {
		t.Fatal(err)
	}
	before, _ := s.scopes()
	found := false
	for _, p := range before {
		found = found || p.String() == "192.0.2.30/32"
	}
	if !found {
		t.Fatal(before)
	}
	binding := s.cleanupBinding()
	s.Throughput.Adapters[0].Limit++
	if s.reviewedThroughputPlan() == nil {
		t.Fatal("budget tampering accepted")
	}
	s.Throughput.Adapters[0].Limit--
	if err := s.claimThroughput(0); err != nil {
		t.Fatal(err)
	}
	if s.claimThroughput(0) == nil {
		t.Fatal("repeated budget claim")
	}
	s.SmokePassed = false
	s.Installed = false
	s.BaselineLabel = "other"
	if s.claimThroughput(0) == nil {
		t.Fatal("phase/installation change refunded the attempt")
	}
	if err := s.claimThroughput(1); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(filepath.Join(s.Dir, "throughput-a-attempt.json"))
	if err != nil || info.Mode().Perm() != 0600 {
		t.Fatal(info, err)
	}
	s.ThroughputPlanSHA256 = strings.Repeat("0", 64)
	if binding == s.cleanupBinding() {
		t.Fatal("cleanup evidence not bound to throughput plan")
	}
	for _, name := range []string{"", "A", "both", "ab"} {
		if _, err := throughputAdapter(name); err == nil {
			t.Fatal(name)
		}
	}
}

func TestThroughputPlanRejectsUnboundedOrWrongScope(t *testing.T) {
	for _, mode := range []string{"missing-cap", "tiny-cap", "huge-cap", "download", "upload", "host", "port", "profile", "wildcard"} {
		t.Run(mode, func(t *testing.T) {
			s := throughputFixture(t)
			switch mode {
			case "missing-cap":
				s.Throughput.Adapters[1].Limit = 0
			case "tiny-cap":
				s.Throughput.Adapters[0].Limit = 16 << 20
				s.Throughput.Adapters[0].Download = 8 << 20
			case "huge-cap":
				s.Throughput.Adapters[0].Limit = 1 << 50
			case "download":
				s.Throughput.Adapters[0].Download = 1 << 40
			case "upload":
				s.Throughput.Adapters[0].Upload = -1
			case "host":
				s.Throughput.Target = "example.com:443"
			case "port":
				s.Throughput.Target = "192.0.2.30:80"
			case "profile":
				s.TestProfile = &testprofile.Profile{Mode: testprofile.FullIPv4}
			case "wildcard":
				s.Throughput.ServerName = "*.example.com"
			}
			if s.validateThroughput() == nil {
				t.Fatal("invalid plan accepted")
			}
		})
	}
}

func throughputTLSServer(t *testing.T, handler http.Handler) (*httptest.Server, *tls.Config) {
	t.Helper()
	pub, key, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "example.com"}, DNSNames: []string{"example.com"}, NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour), IsCA: true, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageDigitalSignature | x509.KeyUsageCertSign, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, pub, key)
	if err != nil {
		t.Fatal(err)
	}
	cert, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatal(err)
	}
	pool := x509.NewCertPool()
	pool.AddCert(cert)
	ts := httptest.NewUnstartedServer(handler)
	ts.TLS = &tls.Config{Certificates: []tls.Certificate{{Certificate: [][]byte{der}, PrivateKey: key}}, NextProtos: []string{"http/1.1"}}
	ts.StartTLS()
	t.Cleanup(ts.Close)
	return ts, &tls.Config{RootCAs: pool}
}

func TestThroughputOneSocketVerifiedTransferAndFailureBounds(t *testing.T) {
	for _, mode := range []string{"download", "upload", "redirect", "oversize", "short", "encoding", "bad-cert", "header-limit", "canceled"} {
		t.Run(mode, func(t *testing.T) {
			const size = int64(64 << 10)
			calls, bodyBytes := 0, int64(0)
			ts, config := throughputTLSServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls++
				if r.URL.Query().Get("bytes") != strconv.FormatInt(size, 10) || r.Header.Get("Accept-Encoding") != "identity" || !r.Close {
					t.Error("request scope/connection policy changed", r.URL, r.Header)
				}
				if mode == "upload" {
					if r.Method != "POST" || r.URL.Path != "/__up" || r.ContentLength != size {
						t.Error(r.Method, r.URL, r.ContentLength)
					}
					buf, err := io.ReadAll(io.LimitReader(r.Body, size+1))
					if err != nil {
						t.Error(err)
					}
					bodyBytes = int64(len(buf))
					for _, b := range buf {
						if b != 0 {
							t.Error("private payload sent")
							break
						}
					}
					w.WriteHeader(200)
					return
				}
				if mode == "redirect" {
					w.Header().Set("Location", "https://other.example.com/")
					w.WriteHeader(302)
					return
				}
				if mode == "encoding" {
					w.Header().Set("Content-Encoding", "gzip")
				}
				if mode == "header-limit" {
					w.Header().Set("X-Large", strings.Repeat("x", 20<<10))
				}
				n := size
				if mode == "oversize" {
					n++
				}
				if mode == "short" {
					n--
				}
				w.Header().Set("Content-Length", strconv.FormatInt(n, 10))
				_, _ = io.CopyN(w, zeroSource{}, n)
			}))
			p := &throughputPlan{Target: "192.0.2.30:443", ServerName: "example.com"}
			if mode == "bad-cert" {
				p.ServerName = "other.example.com"
			}
			dials := 0
			dial := func(ctx context.Context, iface, target string) (net.Conn, error) {
				dials++
				if iface != "test-adapter" || target != p.Target {
					t.Fatal(iface, target)
				}
				return (&net.Dialer{}).DialContext(ctx, "tcp", ts.Listener.Addr().String())
			}
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			if mode == "canceled" {
				cancel()
			}
			sample, err := exchangeThroughput(ctx, p, "test-adapter", throughputRequest{Name: mode, Upload: mode == "upload", Bytes: size}, dial, config)
			// Close waits for handler completion before inspecting its counters.
			ts.Close()
			if mode == "upload" || mode == "download" {
				if err != nil || sample.Payload != size || !sample.Verified || sample.Mbps <= 0 || sample.Sent <= 0 || sample.Received <= 0 || sample.Total < sample.Elapsed || calls != 1 {
					t.Fatal(sample, err, calls)
				}
				if mode == "upload" && bodyBytes != size {
					t.Fatal(bodyBytes)
				}
			} else if err == nil || sample.Error == "" {
				t.Fatal("invalid response accepted", sample, err)
			}
			if dials > 1 || calls > 1 || sample.Sent+sample.Received > throughputSocketLimit(size) {
				t.Fatal("unbounded or retried exchange", dials, calls, sample)
			}
		})
	}
}

type memoryThroughputConn struct {
	written, read int
	closed        bool
}

func (c *memoryThroughputConn) Read(p []byte) (int, error) {
	clear(p)
	c.read += len(p)
	return len(p), nil
}
func (c *memoryThroughputConn) Write(p []byte) (int, error)    { c.written += len(p); return len(p), nil }
func (c *memoryThroughputConn) Close() error                   { c.closed = true; return nil }
func (*memoryThroughputConn) LocalAddr() net.Addr              { return &net.TCPAddr{} }
func (*memoryThroughputConn) RemoteAddr() net.Addr             { return &net.TCPAddr{} }
func (*memoryThroughputConn) SetDeadline(time.Time) error      { return nil }
func (*memoryThroughputConn) SetReadDeadline(time.Time) error  { return nil }
func (*memoryThroughputConn) SetWriteDeadline(time.Time) error { return nil }

func TestThroughputBidirectionalMeterNeverExceedsReservation(t *testing.T) {
	raw := &memoryThroughputConn{}
	meter := &throughputConn{Conn: raw, left: 100}
	if n, err := meter.Write(make([]byte, 101)); n != 0 || !errors.Is(err, errThroughputBudget) || raw.written != 0 {
		t.Fatal(n, err, raw)
	}
	if n, err := meter.Write(make([]byte, 60)); n != 60 || err != nil {
		t.Fatal(n, err)
	}
	if n, err := meter.Read(make([]byte, 60)); n != 40 || err != nil {
		t.Fatal(n, err)
	}
	if n, err := meter.Read(make([]byte, 1)); n != 0 || !errors.Is(err, errThroughputBudget) {
		t.Fatal(n, err)
	}
	if meter.sent+meter.received != 100 || raw.written+raw.read != 100 {
		t.Fatal(meter, raw)
	}
}

func TestThroughputHelperUsageAndIdentity(t *testing.T) {
	for text, want := range map[string]time.Duration{"0:01.25": 1250 * time.Millisecond, "01:02:03.50": time.Hour + 2*time.Minute + 3500*time.Millisecond, "2-01:02:03": 49*time.Hour + 2*time.Minute + 3*time.Second} {
		r, err := parseHelperUsage(700, "700 "+text+" 2048\n")
		if err != nil || r.CPU != want || r.RSS != 2<<20 {
			t.Fatal(r, err)
		}
	}
	for _, text := range []string{"701 0:00.00 2048", "700 0:90.00 1", "700 -1:01 2", "700 0:01 -2", "700 0:01 2 extra"} {
		if _, err := parseHelperUsage(700, text); err == nil {
			t.Fatal(text)
		}
	}
	now := time.Now()
	a := helperUsage{At: now, PID: 700, Identity: 1, CPU: time.Second}
	b := a
	b.At = now.Add(2 * time.Second)
	b.CPU += time.Second
	if n, err := usageDelta(a, b); err != nil || n != 50 {
		t.Fatal(n, err)
	}
	b.Identity++
	if _, err := usageDelta(a, b); err == nil {
		t.Fatal("PID reuse accepted")
	}
	for _, text := range []string{"pid = 0", "pid = 7\npid = 8", "no pid"} {
		if _, err := launchdPID(text); err == nil {
			t.Fatal(text)
		}
	}
}

func TestThroughputRulesAndExternalView(t *testing.T) {
	s := throughputFixture(t)
	rules := throughputRules(s.Throughput, s.Interfaces[0], 1001)
	cfg := api.DefaultConfig()
	cfg.Scenes[0].Rules = rules
	if err := cfg.Validate(); err != nil {
		t.Fatal(err)
	}
	if len(rules) != 1001 || rules[1000].ID != "throughput-route" || rules[1000].Fallback {
		t.Fatal(rules[len(rules)-1])
	}
	base := snapshot{Routes: []capture.Route{{Interface: "external-vpn", Index: 9, Prefix: netip.MustParsePrefix("192.0.2.0/24")}}}
	with := base
	with.Routes = append(append([]capture.Route(nil), base.Routes...), capture.Route{Interface: "utun-test", Index: 10, Prefix: netip.MustParsePrefix("192.0.2.30/32")})
	st := api.Status{LastCapture: &api.CaptureIdentity{Interface: "utun-test", Index: 10}}
	view := throughputExternalView(with, &st)
	if !reflect.DeepEqual(view.Routes, base.Routes) || len(with.Routes) != 2 {
		t.Fatal(view, with)
	}
}

type speedService struct {
	*proofService
	state api.Status
}

func (c *speedService) Status(context.Context) (api.Status, error) { return c.state, nil }

func TestThroughputDirectBeforeCaptureAndExactEvidence(t *testing.T) {
	for _, choice := range []string{"a", "b"} {
		t.Run(choice, func(t *testing.T) {
			s := throughputFixture(t)
			adapter, _ := throughputAdapter(choice)
			cfg := api.DefaultConfig()
			c := &speedService{proofService: &proofService{session: s, cfg: cfg}, state: api.Status{State: api.Stopped, Helper: api.HelperStatus{Installed: true, Running: true, Version: s.Version}}}
			report := checkReport{Before: cloneSnapshot(s.Original)}
			report.Before.Launchd = "pid = 700\n"
			now := time.Now()
			calls, usageCalls := 0, 0
			var bindings []string
			path, err := os.Executable()
			if err != nil {
				t.Fatal(err)
			}
			deps := throughputDeps{
				snapshot: func(context.Context) (snapshot, error) { return s.Original, nil },
				usage: func(context.Context, int) (helperUsage, error) {
					usageCalls++
					now = now.Add(time.Second)
					return helperUsage{At: now, PID: 700, Identity: 701, CPU: time.Duration(usageCalls) * time.Millisecond, RSS: 32 << 20}, nil
				},
				pause: func(_ context.Context, d time.Duration) error { now = now.Add(d); return nil },
				exchange: func(_ context.Context, p *throughputPlan, iface string, request throughputRequest) (throughputSample, error) {
					calls++
					bindings = append(bindings, iface)
					claim := filepath.Join(s.Dir, "throughput-"+choice+"-attempt.json")
					if _, err := os.Stat(claim); err != nil {
						t.Fatal("request started before durable reservation", err)
					}
					if calls <= 3 && c.state.State != api.Stopped {
						t.Fatal("baseline ran during capture")
					}
					if calls > 3 && c.state.State != api.Running {
						t.Fatal("captured measurement ran while stopped")
					}
					sample := throughputSample{throughputRequest: request, Local: fmt.Sprintf("192.0.2.10:%d", 50000+calls), Remote: p.Target, Verified: true, Status: 200, Payload: request.Bytes, Sent: 2000, Received: request.Bytes + 2000, Elapsed: time.Second, Total: 2 * time.Second, Mbps: float64(request.Bytes) * 8 / 1e6}
					if request.Upload {
						sample.Sent, sample.Received = sample.Received, sample.Sent
					}
					if calls > 3 {
						c.logs = append(c.logs, api.Connection{ID: uint64(calls), Started: now, Local: sample.Local, Remote: p.Target, Process: api.Process{PID: os.Getpid(), Path: path}, Protocol: api.TCP, RuleID: "throughput-route", Outcome: api.Routed, Interface: s.Interfaces[adapter].Name, OutboundLocal: "198.51.100.10:55000", Sent: uint64(sample.Sent), Received: uint64(sample.Received)})
					}
					return sample, nil
				},
			}
			run, err := newThroughputRun(s, choice, &report, deps)
			if err != nil {
				t.Fatal(err)
			}
			if err := run.phase(context.Background(), c, "direct", 0, nil); err != nil {
				t.Fatal(err)
			}
			c.state.State, c.state.Since, c.state.LastCapture = api.Running, now, &api.CaptureIdentity{Token: "test-throughput-capture"}
			if err := run.captured(context.Background(), c, cfg, c.state); err != nil {
				t.Fatal(err)
			}
			if calls != 9 || usageCalls != 8 || len(report.Throughput.Phases) != 3 || len(c.saved) != 2 || len(c.saved[1].Scenes[0].Rules) != 1001 {
				t.Fatal(calls, usageCalls, report.Throughput, len(c.saved))
			}
			for i, iface := range bindings {
				want := ""
				if i < 3 {
					want = s.Interfaces[adapter].Name
				}
				if iface != want {
					t.Fatal("wrong binding stage", bindings)
				}
			}
			for _, phase := range report.Throughput.Phases[1:] {
				if len(phase.Connections) != 3 || phase.SerializedLogBytes <= 0 || phase.CPUPercent <= 0 {
					t.Fatal(phase)
				}
			}
			if _, err := newThroughputRun(s, choice, &report, deps); err == nil {
				t.Fatal("second run allowed")
			}
		})
	}
}

func TestThroughputFailedBaselineNeverStartsCaptureOrRefunds(t *testing.T) {
	s, c, ops := baselineFixture(t)
	s.Throughput = throughputFixture(t).Throughput
	if err := s.writeThroughputPlan(); err != nil {
		t.Fatal(err)
	}
	before := cloneSnapshot(s.Original)
	before.Launchd = "pid = 700"
	ops.snapshot = func(context.Context) (snapshot, error) { return before, nil }
	calls := 0
	deps := throughputDeps{snapshot: ops.snapshot, pause: processPause,
		usage: func(context.Context, int) (helperUsage, error) {
			return helperUsage{At: time.Now(), PID: 700, Identity: 1}, nil
		},
		exchange: func(context.Context, *throughputPlan, string, throughputRequest) (throughputSample, error) {
			calls++
			return throughputSample{}, errors.New("synthetic bounded transfer failure")
		},
	}
	ops.throughput = &deps
	err := checkWithOps(context.Background(), s, c, checkOptions{throughput: "a"}, ops)
	if err == nil || !strings.Contains(err.Error(), "synthetic bounded") || calls != 1 || c.starts != 0 || c.stops != 1 {
		t.Fatal(err, calls, c.starts, c.stops)
	}
	if s.claimThroughput(0) == nil {
		t.Fatal("failed request refunded budget")
	}
}

func TestThroughputRejectsWrongExitAndChangingTopology(t *testing.T) {
	s := throughputFixture(t)
	path, _ := os.Executable()
	sample := throughputSample{Local: "192.0.2.10:50000", Remote: s.Throughput.Target, Sent: 100, Received: 200}
	c := &proofService{logs: []api.Connection{{ID: 2, Local: sample.Local, Remote: sample.Remote, Process: api.Process{PID: os.Getpid(), Path: path}, Protocol: api.TCP, RuleID: "throughput-route", Outcome: api.Routed, Interface: s.Interfaces[1].Name}}}
	if _, err := waitThroughputConnection(context.Background(), c, 1, s.Throughput.Target, sample, s.Interfaces[0].Name); err == nil {
		t.Fatal("wrong exit accepted")
	}
	changed := cloneSnapshot(s.Original)
	changed.Routes = append(changed.Routes, capture.Route{Prefix: netip.MustParsePrefix("203.0.113.0/24"), Interface: "external-vpn", Index: 44})
	run := &throughputRun{s: s, report: &throughputEvidence{}, baseline: s.Original, deps: throughputDeps{snapshot: func(context.Context) (snapshot, error) { return changed, nil }}}
	if err := run.phase(context.Background(), c, "changed", 1, nil); err == nil || !strings.Contains(err.Error(), "before throughput") {
		t.Fatal(err)
	}
}
