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
