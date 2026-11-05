//go:build !darwin

package capture

import (
	"context"
	"fmt"
	"net/netip"

	"golang.zx2c4.com/wireguard/tun"
)

func Snapshot() ([]Route, error) { return nil, fmt.Errorf("capture requires macOS") }

type nativeDriver struct{}

func (nativeDriver) BootSession() (string, error) { return "", fmt.Errorf("capture requires macOS") }

func (nativeDriver) Index(string) (int, error) { return 0, fmt.Errorf("capture requires macOS") }

func (nativeDriver) Snapshot() ([]Route, error)     { return Snapshot() }
func (nativeDriver) Create(int) (tun.Device, error) { return nil, fmt.Errorf("capture requires macOS") }
func (nativeDriver) Configure(context.Context, string, bool) error {
	return fmt.Errorf("capture requires macOS")
}
func (nativeDriver) Add(context.Context, netip.Prefix, string) error {
	return fmt.Errorf("capture requires macOS")
}
func (nativeDriver) Delete(context.Context, netip.Prefix, string) error {
	return fmt.Errorf("capture requires macOS")
}
