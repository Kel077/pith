//go:build darwin

package echo

import (
	"context"
	"net/netip"
	"testing"
	"time"
)

func TestDatagramEchoLoopback(t *testing.T) {
	// A short local datagram exchange only: no raw sockets, routes, root, or
	// external destination. Verify the kernel's ICMP identity/checksum behavior.
	for _, raw := range []string{"127.0.0.1", "::1"} {
		t.Run(raw, func(t *testing.T) {
			destination := netip.MustParseAddr(raw)
			request := []byte{8, 0, 0, 0, 0, 1, 0, 2, 112, 105, 116, 104}
			if destination.Is6() {
				request[0] = 128
			}
			ctx, cancel := context.WithTimeout(context.Background(), time.Second)
			defer cancel()
			reply, err := exchange(ctx, "lo0", destination, request)
			if err != nil {
				t.Fatal(err)
			}
			if len(reply) != len(request) || string(reply[8:]) != "pith" {
				t.Fatal("incorrect local echo")
			}
		})
	}
}
