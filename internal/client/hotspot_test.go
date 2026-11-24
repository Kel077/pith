package client

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Kel077/pith/internal/api"
	"github.com/Kel077/pith/internal/hotspot/hotspottest"
	"github.com/Kel077/pith/internal/keychain"
	"github.com/Kel077/pith/internal/proxy"
)

func TestHotspot(t *testing.T) {
	c := testClient(t)
	c.hotspotIndex = func(string) (int, error) { return 9, nil }
	f := hotspottest.New()
	mifi := httptest.NewServer(f)
	defer mifi.Close()
	_, c.hotspotPort, _ = net.SplitHostPort(mifi.Listener.Addr().String())
	c.dialer = func(string, time.Duration, []string) (*net.Dialer, error) {
		return &net.Dialer{Timeout: time.Second}, nil
	}
	c.interfaces = func(context.Context) ([]api.Interface, error) {
		return []api.Interface{
			{Name: "en0", Label: "Ethernet", Kind: api.KindEthernet, Up: true, Router: "127.0.0.1"},
			{Name: "en9", Label: "M2000", Kind: api.KindCellular, HardwareAddr: "02:00:5e:10:00:09", Up: true, Router: "127.0.0.1"},
		}, nil
	}
	secrets := map[string]string{}
	c.secretGet = func(_ context.Context, service, account string) (string, error) {
		if s, ok := secrets[service+"/"+account]; ok {
			return s, nil
		}
		return "", keychain.ErrNotFound
	}
	c.secretSet = func(_ context.Context, service, account, secret string) error {
		secrets[service+"/"+account] = secret
		return nil
	}
	ipify := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { fmt.Fprint(w, "192.0.2.9\n") }))
	defer ipify.Close()
	c.probeURLs = [2]string{ipify.URL, ipify.URL}
	ctx := context.Background()

	if _, err := c.Hotspot(ctx, "en0"); api.Code(err) != api.ErrUnsupported {
		t.Fatalf("Ethernet: %v", err)
	}
	if _, err := c.RenewHotspot(ctx, "en0"); api.Code(err) != api.ErrUnsupported {
		t.Fatalf("renewed Ethernet: %v", err)
	}
	h, err := c.Hotspot(ctx, "en9")
	if err != nil || !h.Locked || h.Interface != "en9" || h.Bars != 4 || h.Cell != nil {
		t.Fatalf("without a password: %+v %v", h, err)
	}
	if err := c.SetHotspotPassword(ctx, "en9", "wrong"); api.Code(err) != api.ErrWrongPassword || len(secrets) != 0 {
		t.Fatalf("wrong password: %v, kept %v", err, secrets)
	}
	if err := c.SetHotspotPassword(ctx, "en9", hotspottest.Password); err != nil || secrets[hotspotKeychain+"/02:00:5e:10:00:09"] != hotspottest.Password {
		t.Fatalf("password: %v, kept %v", err, secrets)
	}
	h, err = c.Hotspot(ctx, "en9")
	if err != nil || h.Locked || h.Cell == nil || h.Cell.Band != "n71" || h.Cell.Operator != "Example Mobile" {
		t.Fatalf("with the password: %+v %v", h, err)
	}
	r, err := c.RenewHotspot(ctx, "en9")
	if err != nil || !r.Before.OK || r.After.PublicIPv4 != "192.0.2.9" || r.Took <= 0 {
		t.Fatalf("renewal: %+v %v", r, err)
	}
	f.Do(func(f *hotspottest.MiFi) {
		if strings.Join(f.Calls, ",") != "off,on" || !f.DataOn {
			t.Fatalf("cellular data: %v, on %v", f.Calls, f.DataOn)
		}
	})
}

func TestHotspotRefreshesDialerAndRefusesReplacedAdapter(t *testing.T) {
	c := testClient(t)
	index := 9
	c.hotspotIndex = func(string) (int, error) { return index, nil }
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Connection", "close")
		fmt.Fprint(w, "ok")
	}))
	defer srv.Close()
	_, c.hotspotPort, _ = net.SplitHostPort(srv.Listener.Addr().String())
	mac := "02:00:5e:10:00:09"
	c.interfaces = func(context.Context) ([]api.Interface, error) {
		return []api.Interface{{Name: "en9", Label: "M2000", HardwareAddr: mac, Router: "127.0.0.1", Up: true}}, nil
	}
	dials := 0
	c.dialer = func(name string, _ time.Duration, _ []string) (*net.Dialer, error) {
		if name != "en9" {
			t.Fatal(name)
		}
		dials++
		return &net.Dialer{Timeout: time.Second}, nil
	}
	s, err := c.hotspot(context.Background(), "en9")
	if err != nil {
		t.Fatal(err)
	}
	defer s.transport.CloseIdleConnections()
	for range 2 {
		res, err := s.device.HTTP.Get(srv.URL)
		if err != nil {
			t.Fatal(err)
		}
		res.Body.Close()
	}
	if dials != 2 {
		t.Fatal("cached dialer retained its old binding", dials)
	}
	index = 10
	if res, err := s.device.HTTP.Get(srv.URL); err == nil {
		res.Body.Close()
		t.Fatal("old pooled session accepted reindexed adapter")
	}
	newSession, err := c.hotspot(context.Background(), "en9")
	if err != nil || newSession == s {
		t.Fatal("reindex did not renew session", err)
	}
	defer newSession.transport.CloseIdleConnections()
	if res, err := newSession.device.HTTP.Get(srv.URL); err != nil {
		t.Fatal(err)
	} else {
		res.Body.Close()
	}
	s = newSession
	mac = "02:00:5e:10:00:08"
	if res, err := s.device.HTTP.Get(srv.URL); err == nil {
		res.Body.Close()
		t.Fatal("old session sent credentials to replaced adapter")
	}
	if dials != 3 {
		t.Fatal("replacement reached the dialer")
	}
	mac = ""
	if _, err := c.hotspot(context.Background(), "en9"); api.Code(err) != api.ErrUnsupported {
		t.Fatal("unidentified adapter may borrow name-based credentials", err)
	}
}

func TestHotspotRedirectStaysOnOriginalOrigin(t *testing.T) {
	c := testClient(t)
	c.hotspotIndex = func(string) (int, error) { return 9, nil }
	c.interfaces = func(context.Context) ([]api.Interface, error) {
		return []api.Interface{{Name: "en9", Label: "M2000", HardwareAddr: "02:00:5e:10:00:09", Router: "192.0.2.1", Up: true}}, nil
	}
	s, err := c.hotspot(context.Background(), "en9")
	if err != nil {
		t.Fatal(err)
	}
	defer s.transport.CloseIdleConnections()
	original, _ := http.NewRequest(http.MethodGet, s.device.Base, nil)
	for _, destination := range []string{"http://192.0.2.2/", "https://192.0.2.1:80/", "http://user@192.0.2.1:80/"} {
		u, _ := url.Parse(destination)
		if err := s.device.HTTP.CheckRedirect(&http.Request{URL: u}, []*http.Request{original}); err != http.ErrUseLastResponse {
			t.Fatal("unsafe redirect accepted", destination)
		}
	}
}

// renewalClient is a client of an M2000 that keeps its password, whose
// public IP tests answer after a renewal as v4 and v6 say: with the
// address, or with nothing for an error.
func renewalClient(t *testing.T, f *hotspottest.MiFi, v4, v6 func(after int) string) *Client {
	t.Helper()
	c := testClient(t)
	c.hotspotIndex = func(string) (int, error) { return 9, nil }
	c.hotspotPace = 20 * time.Millisecond
	mifi := httptest.NewServer(f)
	t.Cleanup(mifi.Close)
	_, c.hotspotPort, _ = net.SplitHostPort(mifi.Listener.Addr().String())
	c.dialer = func(string, time.Duration, []string) (*net.Dialer, error) {
		return &net.Dialer{Timeout: time.Second}, nil
	}
	c.interfaces = func(context.Context) ([]api.Interface, error) {
		return []api.Interface{{Name: "en9", Label: "M2000", Kind: api.KindCellular, HardwareAddr: "02:00:5e:10:00:09", Up: true, Router: "127.0.0.1"}}, nil
	}
	c.secretGet = func(context.Context, string, string) (string, error) { return hotspottest.Password, nil }
	answer := func(ip func(int) string) http.HandlerFunc {
		var mu sync.Mutex
		after := 0
		return func(w http.ResponseWriter, r *http.Request) {
			renewed := false
			f.Do(func(f *hotspottest.MiFi) { renewed = len(f.Calls) == 2 })
			mu.Lock()
			n := -1
			if renewed {
				n = after
				after++
			}
			mu.Unlock()
			if a := ip(n); a != "" {
				fmt.Fprint(w, a)
				return
			}
			http.Error(w, "unavailable", http.StatusServiceUnavailable)
		}
	}
	srv4 := httptest.NewServer(answer(v4))
	t.Cleanup(srv4.Close)
	listener6, err := net.Listen("tcp6", "[::1]:0")
	if err != nil {
		t.Fatal(err)
	}
	srv6 := httptest.NewUnstartedServer(answer(v6))
	srv6.Listener.Close()
	srv6.Listener = listener6
	srv6.Start()
	t.Cleanup(srv6.Close)
	c.probeURLs = [2]string{srv4.URL, srv6.URL}
	return c
}

func TestHotspotRenewalWaitsUntilTheHotspotWorksAgain(t *testing.T) {
	// Connected again, it has no DNS for a moment, then IPv4 before IPv6.
	f := hotspottest.New()
	c := renewalClient(t, f,
		func(after int) string { return map[bool]string{true: "192.0.2.9"}[after < 0 || after >= 3] },
		func(after int) string { return map[bool]string{true: "2001:db8::9"}[after < 0 || after >= 4] })
	r, err := c.RenewHotspot(context.Background(), "en9")
	if err != nil || r.After.PublicIPv4 != "192.0.2.9" || r.After.PublicIPv6 != "2001:db8::9" || r.Before.PublicIPv6 == "" {
		t.Fatalf("%+v %v", r, err)
	}
}

func TestHotspotRenewalStopsWaitingForAFamilyThatDoesNotComeBack(t *testing.T) {
	f := hotspottest.New()
	c := renewalClient(t, f,
		func(int) string { return "192.0.2.9" },
		func(after int) string { return map[bool]string{true: "2001:db8::9"}[after < 0] })
	start := time.Now()
	r, err := c.RenewHotspot(context.Background(), "en9")
	if err != nil || r.After.PublicIPv4 != "192.0.2.9" || r.After.PublicIPv6 != "" || !r.After.OK {
		t.Fatalf("%+v %v", r, err)
	}
	// It waited 6 paces for IPv6, not 25.
	if waited := time.Since(start) - r.Took; waited > 15*c.hotspotPace {
		t.Fatalf("waited %v for IPv6", waited)
	}
}

func TestHotspotRenewalMergesStaggeredFamiliesAndSuspendsProxies(t *testing.T) {
	f := hotspottest.New()
	var v6CallsAfter int
	var mu sync.Mutex
	// IPv6 works on the first post-renewal attempt; IPv4 comes up 3 attempts later.
	c := renewalClient(t, f,
		func(after int) string { return map[bool]string{true: "192.0.2.19", false: map[bool]string{true: "192.0.2.9"}[after < 0]}[after >= 3] },
		func(after int) string {
			if after < 0 {
				return "2001:db8::9"
			}
			mu.Lock()
			v6CallsAfter++
			mu.Unlock()
			return "2001:db8::19"
		})
	manager, err := proxy.New(proxy.Options{Dial: func(context.Context, api.InterfaceRef, proxy.Target) (net.Conn, error) {
		return nil, fmt.Errorf("unexpected proxy connection")
	}})
	if err != nil {
		t.Fatal(err)
	}
	c.proxies = manager
	var suspendedDuringOff bool
	s, err := c.hotspot(context.Background(), "en9")
	if err != nil {
		t.Fatal(err)
	}
	base := s.device.HTTP.Transport
	s.device.HTTP.Transport = exitRoundTrip(func(req *http.Request) (*http.Response, error) {
		if req.URL.Path == "/wwan/disablecellulardata/" {
			suspendedDuringOff = manager.Suspended(api.InterfaceRef{Name: "en9"})
		}
		return base.RoundTrip(req)
	})
	r, err := c.RenewHotspot(context.Background(), "en9")
	if err != nil || r.After.PublicIPv4 != "192.0.2.19" || r.After.PublicIPv6 != "2001:db8::19" {
		t.Fatalf("renewal: %+v %v", r, err)
	}
	if !suspendedDuringOff {
		t.Fatal("proxy port was not suspended before cellular data went off")
	}
	if manager.Suspended(api.InterfaceRef{Name: "en9"}) {
		t.Fatal("proxy port remained suspended after renewal")
	}
	mu.Lock()
	gotV6Calls := v6CallsAfter
	mu.Unlock()
	if gotV6Calls != 1 {
		t.Fatalf("IPv6 probed %d times after renewal, want 1", gotV6Calls)
	}
}

func TestParseUsableIPv6(t *testing.T) {
	onlyDeprecated := "\tinet6 fe80::1%en9 prefixlen 64 secured scopeid 0xe\n" +
		"\tinet6 2001:db8:1::2 prefixlen 64 deprecated autoconf secured\n" +
		"\tinet6 2001:db8:2::3 prefixlen 64 tentative autoconf\n"
	if parseUsableIPv6(onlyDeprecated) {
		t.Fatal("accepted deprecated and tentative IPv6 addresses as usable")
	}
	withActive := onlyDeprecated + "\tinet6 2001:db8:3::4 prefixlen 64 autoconf temporary\n"
	if !parseUsableIPv6(withActive) {
		t.Fatal("missed active global IPv6 address")
	}
}

func TestHotspotRenewalGivesUpTesting(t *testing.T) {
	f := hotspottest.New()
	none := func(after int) string { return map[bool]string{true: "192.0.2.9"}[after < 0] }
	c := renewalClient(t, f, none, func(int) string { return "" })
	r, err := c.RenewHotspot(context.Background(), "en9")
	if err != nil || !r.Before.OK || r.After.OK || r.After.Problem == "" {
		t.Fatalf("%+v %v", r, err)
	}
}

func TestHotspotRenewalWritesSanitizedDiagnosticLogAndRespectsLogDays(t *testing.T) {
	f := hotspottest.New()
	c := renewalClient(t, f,
		func(after int) string {
			if after < 0 {
				return "192.0.2.10"
			}
			return "192.0.2.20"
		},
		func(after int) string {
			if after < 0 {
				return "2001:db8::10"
			}
			return "2001:db8::20"
		})
	now := time.Date(2026, 11, 24, 22, 30, 0, 0, time.UTC)
	c.exitNow = func() time.Time { return now }

	// Seed an expired log file older than default LogDays (7) to verify pruning.
	if err := os.MkdirAll(c.opts.ConfigDir, 0700); err != nil {
		t.Fatal(err)
	}
	stalePath := filepath.Join(c.opts.ConfigDir, "hotspot-renewals-2026-11-01.jsonl")
	if err := os.WriteFile(stalePath, []byte(`{"interface":"en9"}`+"\n"), 0600); err != nil {
		t.Fatal(err)
	}

	if _, err := c.RenewHotspot(context.Background(), "en9"); err != nil {
		t.Fatalf("RenewHotspot failed: %v", err)
	}
	if _, err := os.Stat(stalePath); !os.IsNotExist(err) {
		t.Fatalf("expected expired hotspot renewal log to be pruned, stat err = %v", err)
	}

	logPath := filepath.Join(c.opts.ConfigDir, "hotspot-renewals-2026-11-24.jsonl")
	raw, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatalf("expected diagnostic log at %s: %v", logPath, err)
	}
	text := string(raw)
	for _, secret := range []string{hotspottest.Password, "1111111111111111111111111111111111111111", "2222222222222222222222222222222222222222", "Cookie:", "mifisess"} {
		if strings.Contains(text, secret) {
			t.Fatalf("diagnostic log leaked secret %q: %s", secret, text)
		}
	}
	var entry hotspotRenewalLog
	if err := json.Unmarshal([]byte(strings.TrimSpace(text)), &entry); err != nil {
		t.Fatalf("invalid JSONL record: %v\n%s", err, text)
	}
	if !entry.OK || entry.Interface != "en9" || entry.Model != "M2000" ||
		entry.InitialMobileData != "enabled" || entry.FinalMobileData != "enabled" ||
		entry.BeforeIPv4 != "192.0.2.10" || entry.AfterIPv4 != "192.0.2.20" ||
		entry.BeforeIPv6 != "2001:db8::10" || entry.AfterIPv6 != "2001:db8::20" {
		t.Fatalf("unexpected log entry: %+v", entry)
	}
	seen := map[string]bool{}
	for _, st := range entry.Stages {
		seen[st.Stage] = true
	}
	for _, wantStage := range []string{"status", "before", "token", "disable", "dropWait", "enable", "reconnect", "retest"} {
		if !seen[wantStage] {
			t.Fatalf("missing stage %q in diagnostic log stages: %+v", wantStage, entry.Stages)
		}
	}

	// Setting LogDays = 0 removes existing logs and suppresses further disk writes.
	cfg := api.DefaultConfig()
	cfg.Settings.LogDays = 0
	c.keepExitAliases(cfg)
	c.pruneHotspotRenewalLogs(now, 0)
	if _, err := os.Stat(logPath); !os.IsNotExist(err) {
		t.Fatalf("expected log file removed when LogDays=0, stat err = %v", err)
	}
	if _, err := c.RenewHotspot(context.Background(), "en9"); err != nil {
		t.Fatalf("RenewHotspot with LogDays=0 failed: %v", err)
	}
	if _, err := os.Stat(logPath); !os.IsNotExist(err) {
		t.Fatalf("LogDays=0 should not write hotspot renewal log, stat err = %v", err)
	}
}
