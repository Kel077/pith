//go:build darwin

package client

import (
	"context"
	"os"
	"testing"
	"time"
)

// Opt-in internet test. Ordinary go test only opens local loopback sockets.
// This makes one bounded probe without changing the machine's network state.
func TestLiveProbeInterface(t *testing.T) {
	iface := os.Getenv("PITH_TEST_INTERFACE")
	if iface == "" {
		t.Skip("set PITH_TEST_INTERFACE to opt into one internet probe")
	}
	c, err := New(Options{ConfigDir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 12*time.Second)
	defer cancel()
	result, err := c.Probe(ctx, iface, os.Getenv("PITH_TEST_TARGET"))
	if err != nil {
		t.Fatal(err)
	}
	if !result.OK {
		t.Fatalf("probe failed: %s", result.Problem)
	}
	// Do not print or persist the developer's addresses.
	t.Logf("bound probe succeeded; latency=%s ipv4=%t ipv6=%t", result.Latency, result.PublicIPv4 != "", result.PublicIPv6 != "")
}

func TestLiveSpeedTest(t *testing.T) {
	iface := os.Getenv("PITH_TEST_INTERFACE")
	if iface == "" || os.Getenv("PITH_TEST_SPEED") == "" {
		t.Skip("set PITH_TEST_INTERFACE and PITH_TEST_SPEED to opt into one speed test, of about 33 MB")
	}
	c, err := New(Options{ConfigDir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	start := time.Now()
	r, err := c.SpeedTest(ctx, iface)
	if err != nil {
		t.Fatal(err)
	}
	if !r.OK || r.Problem != "" {
		t.Fatalf("speed test failed: %s", r.Problem)
	}
	// Do not print or persist the developer's addresses.
	t.Logf("server %s, latency %v, download %.0f Mbps, upload %.0f Mbps, public IP %t, in %v", r.Server, r.Latency.Round(100*time.Microsecond),
		float64(r.Download)/1e6, float64(r.Upload)/1e6, r.PublicIP != "", time.Since(start).Round(10*time.Millisecond))
}
