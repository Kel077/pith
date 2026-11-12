//go:build darwin

package client

import (
	"context"
	"errors"
	"net/http"
	"net/http/cookiejar"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/Kel077/pith/internal/api"
	"github.com/Kel077/pith/internal/keychain"
	"github.com/Kel077/pith/internal/netif"
	"github.com/Kel077/pith/internal/router"
)

// primaryOf names macOS's primary interface, or "".
func primaryOf(t *testing.T) string {
	t.Helper()
	ifaces, err := netif.List(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	for _, i := range ifaces {
		if i.Primary {
			return i.Name
		}
	}
	return ""
}

// waitPrimary waits until want is macOS's primary, or another than not.
func waitPrimary(t *testing.T, want, not string, limit time.Duration) (string, time.Duration) {
	t.Helper()
	start := time.Now()
	for {
		p := primaryOf(t)
		if want != "" && p == want || want == "" && p != "" && p != not {
			return p, time.Since(start)
		}
		if time.Since(start) > limit {
			t.Fatalf("macOS's primary is %q after %v", p, limit)
		}
		time.Sleep(100 * time.Millisecond)
	}
}

// Opt-in hardware test of the installed helper of this build: macOS makes
// another interface primary for a few seconds, then PITH_TEST_YIELD is primary
// again. It changes macOS's routing meanwhile, not the router's.
func TestLiveYield(t *testing.T) {
	iface := os.Getenv("PITH_TEST_YIELD")
	if iface == "" {
		t.Skip("set PITH_TEST_YIELD to an interface to have it yield macOS's primary for a few seconds")
	}
	c, err := New(Options{ConfigDir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	if p := primaryOf(t); p != iface {
		t.Fatalf("%s is not primary but %q", iface, p)
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	start := time.Now()
	primary, ended, release, err := c.yield(ctx, iface)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("the helper says %s is primary after %v", primary, time.Since(start).Round(time.Millisecond))
	got, took := waitPrimary(t, primary, "", 5*time.Second)
	t.Logf("the routing table says %s is primary %v later", got, took.Round(time.Millisecond))
	// The internet through the new primary, without binding: as an app has it.
	res, err := (&http.Client{Timeout: 8 * time.Second}).Get("https://api.ipify.org")
	if err != nil {
		release()
		t.Fatalf("the internet through %s: %v", primary, err)
	}
	res.Body.Close()
	time.Sleep(3 * time.Second)
	select {
	case <-ended:
		t.Fatal("the yield ended early")
	default:
	}
	start = time.Now()
	release()
	_, took = waitPrimary(t, iface, "", 10*time.Second)
	t.Logf("%s primary again %v after the release", iface, took.Round(time.Millisecond))
}

// liveRouterClient is a client whose routers' password is the generic
// password PITH_TEST_ROUTER_KEYCHAIN, read by security; it keeps none.
func liveRouterClient(t *testing.T) (*Client, string) {
	t.Helper()
	iface, item := os.Getenv("PITH_TEST_ROUTER"), os.Getenv("PITH_TEST_ROUTER_KEYCHAIN")
	if iface == "" || item == "" {
		t.Skip("set PITH_TEST_ROUTER and PITH_TEST_ROUTER_KEYCHAIN to renew the router's address: its whole network is offline meanwhile")
	}
	c, err := New(Options{ConfigDir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { c.Close() })
	c.secretGet = func(ctx context.Context, service, _ string) (string, error) {
		if service != routerKeychain {
			return "", keychain.ErrNotFound
		}
		out, err := exec.CommandContext(ctx, "/usr/bin/security", "find-generic-password", "-s", item, "-w").Output()
		return strings.TrimRight(string(out), "\n"), err
	}
	c.secretSet = func(context.Context, string, string, string) error {
		return errors.New("the live test keeps no password")
	}
	return c, iface
}

// changed tells whether two addresses differ, without saying them.
func changed(before, after string) string {
	switch {
	case after == "":
		return "none after"
	case before == after:
		return "unchanged"
	}
	return "changed"
}

// Opt-in hardware test of one release and renewal while another interface is
// primary, watched by a second session every half second.
func TestLiveRouterCycle(t *testing.T) {
	c, iface := liveRouterClient(t)
	if os.Getenv("PITH_TEST_ROUTER_CYCLE") == "" {
		t.Skip("set PITH_TEST_ROUTER_CYCLE too, to watch one cycle")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Minute)
	defer cancel()
	s, err := c.router(ctx, iface)
	if err != nil {
		t.Fatal(err)
	}
	before, err := s.device.WAN(ctx)
	if err != nil || !before.Connected {
		t.Fatalf("before: %+v %v", before, err)
	}
	// A second session to watch the connection while the first renews it.
	jar, _ := cookiejar.New(nil)
	watchHTTP := *s.device.HTTP
	watchHTTP.Jar = jar
	watcher := &router.Verizon{Base: s.device.Base, Model: s.device.Model, HTTP: &watchHTTP, Password: s.device.Password}
	defer watcher.Close(context.Background())
	if _, err := watcher.WAN(ctx); err != nil {
		t.Fatalf("a second session: %v", err)
	}
	primary, _, release, err := c.yield(ctx, iface)
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	t.Logf("%s is primary", primary)
	time.Sleep(2 * time.Second)
	start := time.Now()
	stop := make(chan struct{})
	watched := make(chan struct{})
	go func() {
		defer close(watched)
		last := "connected"
		for {
			w, err := watcher.WAN(ctx)
			state := "error: " + errString(err)
			if err == nil {
				state = "disconnected"
				if w.Connected {
					state = "connected, address " + changed(before.IPv4.String(), w.IPv4.String())
				}
			}
			if state != last {
				t.Logf("%6.1f s  %s", time.Since(start).Seconds(), state)
				last = state
			}
			select {
			case <-stop:
				return
			case <-time.After(500 * time.Millisecond):
			}
		}
	}()
	pause := 5 * time.Second
	after, err := s.device.Renew(ctx, pause)
	took := time.Since(start)
	time.Sleep(time.Second)
	close(stop)
	<-watched
	if err != nil {
		t.Fatalf("renew: %v after %v", err, took)
	}
	t.Logf("renewed in %v with a pause of %v: address %s, lease %v", took.Round(100*time.Millisecond), pause, changed(before.IPv4.String(), after.IPv4.String()), after.Lease)
	p, err := c.retest(ctx, iface, api.Probe{OK: true, PublicIPv4: "x"})
	t.Logf("the internet through %s: ok %v, public IPv4 %s as the router's (%v)", iface, p.OK, changed(after.IPv4.String(), p.PublicIPv4), err)
}

func errString(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}

// Opt-in hardware test of RenewRouter as the app runs it.
func TestLiveRenewRouter(t *testing.T) {
	c, iface := liveRouterClient(t)
	if os.Getenv("PITH_TEST_ROUTER_RENEW") == "" {
		t.Skip("set PITH_TEST_ROUTER_RENEW too, to renew it as the app does")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Minute)
	defer cancel()
	r, err := c.RenewRouter(ctx, iface)
	t.Logf("tries %d, took %v, standby %s, IPv4 %s, IPv6 %s, after ok %v (%v)", r.Tries, r.Took.Round(100*time.Millisecond), r.Standby,
		changed(r.Before.PublicIPv4, r.After.PublicIPv4), changed(r.Before.PublicIPv6, r.After.PublicIPv6), r.After.OK, err)
	if err != nil {
		t.Fatal(err)
	}
	if p := primaryOf(t); p != iface {
		_, took := waitPrimary(t, iface, "", 10*time.Second)
		t.Logf("%s primary again %v after the renewal returned", iface, took)
	}
}
