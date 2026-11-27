//go:build !darwin

package client

import (
	"context"
	"net/netip"
)

func gatewayHardwareAddr(context.Context, int, netip.Addr) string { return "" }
