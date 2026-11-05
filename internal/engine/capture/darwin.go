//go:build darwin

package capture

import (
	"context"
	"fmt"
	"net"
	"net/netip"
	"os"
	"os/exec"
	"strings"
	"time"

	"golang.org/x/net/route"
	"golang.org/x/sys/unix"
	"golang.zx2c4.com/wireguard/tun"
)

// Snapshot reads the routing information base without opening a route writer.
func Snapshot() ([]Route, error) {
	data, err := route.FetchRIB(unix.AF_UNSPEC, route.RIBTypeRoute, 0)
	if err != nil {
		return nil, err
	}
	messages, err := route.ParseRIB(route.RIBTypeRoute, data)
	if err != nil {
		return nil, err
	}
	interfaces, err := net.Interfaces()
	if err != nil {
		return nil, err
	}
	names := make(map[int]string)
	for _, iface := range interfaces {
		names[iface.Index] = iface.Name
	}
	return decodeRoutes(messages, names), nil
}

func decodeRoutes(messages []route.Message, names map[int]string) []Route {
	var result []Route
	for _, message := range messages {
		r, ok := message.(*route.RouteMessage)
		if !ok || r.Flags&unix.RTF_UP == 0 || len(r.Addrs) <= unix.RTAX_NETMASK {
			continue
		}
		dst := address(r.Addrs[unix.RTAX_DST])
		if !dst.IsValid() {
			continue
		}
		bits := 0
		if r.Flags&unix.RTF_HOST != 0 {
			bits = dst.BitLen()
		} else if mask := address(r.Addrs[unix.RTAX_NETMASK]); mask.IsValid() {
			var total int
			bits, total = net.IPMask(mask.AsSlice()).Size()
			if total != dst.BitLen() {
				continue
			}
		} else if !dst.IsUnspecified() {
			continue
		}
		prefix := netip.PrefixFrom(dst, bits).Masked()
		result = append(result, Route{Prefix: prefix, Interface: names[r.Index], Index: r.Index, Gateway: address(r.Addrs[unix.RTAX_GATEWAY]), Scoped: r.Flags&unix.RTF_IFSCOPE != 0, Local: r.Flags&unix.RTF_LOCAL != 0, Reject: r.Flags&(unix.RTF_REJECT|unix.RTF_BLACKHOLE) != 0})
	}
	return result
}

func address(value route.Addr) netip.Addr {
	switch a := value.(type) {
	case *route.Inet4Addr:
		return netip.AddrFrom4(a.IP)
	case *route.Inet6Addr:
		return netip.AddrFrom16(a.IP).WithZone("")
	}
	return netip.Addr{}
}

type nativeDriver struct{}

// XNU bsd/kern/kern_sysctl.c exposes this read-only UUID for the current boot.
// Unlike kern.bootuuid, it identifies a boot session, not the boot volume.
func (nativeDriver) BootSession() (string, error) {
	id, err := unix.Sysctl("kern.bootsessionuuid")
	if err != nil {
		return "", err
	}
	if len(id) != 36 {
		return "", fmt.Errorf("invalid kernel boot session identity")
	}
	return id, nil
}

func (nativeDriver) Snapshot() ([]Route, error) { return Snapshot() }
func (nativeDriver) Index(name string) (int, error) {
	iface, err := net.InterfaceByName(name)
	if err != nil {
		return 0, err
	}
	return iface.Index, nil
}
func (nativeDriver) Create(mtu int) (tun.Device, error) {
	if os.Geteuid() != 0 {
		return nil, fmt.Errorf("utun creation requires the privileged helper")
	}
	// The library's optional name-file environment variable must never give a
	// caller an arbitrary privileged file write. Launchd does not need it.
	if os.Getenv("WG_TUN_NAME_FILE") != "" {
		return nil, fmt.Errorf("WG_TUN_NAME_FILE must be unset in the helper")
	}
	return tun.CreateTUN("utun", mtu)
}
func (nativeDriver) Configure(ctx context.Context, name string, ipv6 bool) error {
	if err := command(ctx, "/sbin/ifconfig", name, "inet", "198.18.0.1", "198.18.0.1", "netmask", "255.255.255.255", "up"); err != nil {
		return err
	}
	if ipv6 {
		return command(ctx, "/sbin/ifconfig", name, "inet6", "fd7a:7069:7468::1", "prefixlen", "128", "alias")
	}
	return nil
}
func (nativeDriver) Add(ctx context.Context, prefix netip.Prefix, name string) error {
	return routeCommand(ctx, "add", prefix, name)
}
func (nativeDriver) Delete(ctx context.Context, prefix netip.Prefix, name string) error {
	return routeCommand(ctx, "delete", prefix, name)
}

func routeCommand(ctx context.Context, action string, prefix netip.Prefix, name string) error {
	family := "-inet"
	if prefix.Addr().Is6() {
		family = "-inet6"
	}
	return command(ctx, "/sbin/route", "-n", action, family, "-net", prefix.String(), "-interface", name)
}

func command(ctx context.Context, path string, args ...string) error {
	ctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, path, args...)
	cmd.Env = []string{"PATH=/usr/bin:/bin:/usr/sbin:/sbin", "LC_ALL=C"}
	output, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("%s: %w: %s", path, err, strings.TrimSpace(string(output)))
	}
	return nil
}
