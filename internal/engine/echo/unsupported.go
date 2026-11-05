//go:build !darwin

package echo

import (
	"context"
	"errors"
	"net/netip"
)

func exchange(context.Context, string, netip.Addr, []byte) ([]byte, error) {
	return nil, errors.New("ICMP relay requires macOS")
}
