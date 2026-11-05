//go:build darwin

package capture

import (
	"context"
	"errors"
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

// Snapshot reads configured forwarding routes without opening a route writer.
// Cloned host routes and link-layer neighbor caches are transient kernel state,
// not capture conflicts or rollback baseline entries.
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
		if !ok || r.Flags&unix.RTF_UP == 0 {
			continue
		}
		// Keep CLONING/PRCLONING parents, static hosts, and VPN routes. Only their
		// traffic-created children and ARP/NDP cache entries are excluded.
		if r.Flags&(unix.RTF_WASCLONED|unix.RTF_LLINFO) != 0 {
			continue
		}
		prefix, err := routePrefix(r)
		if err != nil {
			continue
		}
		result = append(result, Route{Prefix: prefix, Interface: names[r.Index], Index: r.Index, Gateway: address(routeAddr(r.Addrs, unix.RTAX_GATEWAY)), Scoped: r.Flags&unix.RTF_IFSCOPE != 0, Local: r.Flags&unix.RTF_LOCAL != 0, Reject: r.Flags&(unix.RTF_REJECT|unix.RTF_BLACKHOLE) != 0})
	}
	return result
}

func routeAddr(addrs []route.Addr, slot int) route.Addr {
	if slot < len(addrs) {
		return addrs[slot]
	}
	return nil
}

func routePrefix(r *route.RouteMessage) (netip.Prefix, error) {
	dst := address(routeAddr(r.Addrs, unix.RTAX_DST))
	if !dst.IsValid() {
		return netip.Prefix{}, errors.New("missing IP destination")
	}
	bits := 0
	if r.Flags&unix.RTF_HOST != 0 {
		bits = dst.BitLen()
	} else if mask := address(routeAddr(r.Addrs, unix.RTAX_NETMASK)); mask.IsValid() {
		var total int
		bits, total = net.IPMask(mask.AsSlice()).Size()
		if total != dst.BitLen() {
			return netip.Prefix{}, errors.New("invalid IP netmask")
		}
	} else if !dst.IsUnspecified() {
		return netip.Prefix{}, errors.New("non-default destination without HOST or netmask")
	}
	return netip.PrefixFrom(dst, bits).Masked(), nil
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
func (nativeDriver) InterfacePrefixes() ([]netip.Prefix, error) {
	addresses, err := net.InterfaceAddrs()
	if err != nil {
		return nil, err
	}
	result := make([]netip.Prefix, 0, len(addresses))
	for _, a := range addresses {
		p, err := netip.ParsePrefix(a.String())
		if err != nil {
			return nil, fmt.Errorf("invalid assigned interface prefix: %w", err)
		}
		result = append(result, p.Masked())
	}
	return result, nil
}
func (nativeDriver) Index(name string) (int, error) {
	index, _, err := (nativeDriver{}).InterfaceState(name)
	return index, err
}
func (nativeDriver) InterfaceState(name string) (int, bool, error) {
	iface, err := net.InterfaceByName(name)
	if err != nil {
		return 0, false, err
	}
	return iface.Index, iface.Flags&net.FlagUp != 0, nil
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
func (nativeDriver) Configure(ctx context.Context, name string, addresses TunnelAddresses) error {
	if !addresses.IPv4.Addr().Is4() || addresses.IPv4.Bits() != 30 || addresses.IPv4 != addresses.IPv4.Masked() ||
		addresses.IPv6.IsValid() && (!addresses.IPv6.Addr().Is6() || addresses.IPv6.Addr().Is4In6() || addresses.IPv6.Bits() != 126 || addresses.IPv6 != addresses.IPv6.Masked()) {
		return fmt.Errorf("invalid tunnel address plan")
	}
	local := addresses.IPv4.Addr().Next()
	peer := local.Next()
	if err := command(ctx, "/sbin/ifconfig", name, "inet", local.String(), peer.String(), "netmask", "255.255.255.252", "up"); err != nil {
		return err
	}
	if addresses.IPv6.IsValid() {
		return command(ctx, "/sbin/ifconfig", name, "inet6", addresses.IPv6.Addr().Next().String(), "prefixlen", "126", "alias")
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
	return command(ctx, "/sbin/route", routeArguments(action, prefix, name)...)
}

func routeArguments(action string, prefix netip.Prefix, name string) []string {
	family := "-inet"
	if prefix.Addr().Is6() {
		family = "-inet6"
	}
	kind, destination := "-net", prefix.String()
	if prefix.Bits() == prefix.Addr().BitLen() {
		// Darwin omits RTA_NETMASK for IPv6 /128. Forcing -net also clears
		// RTF_HOST, making that route ambiguous. Add and delete hosts explicitly.
		kind, destination = "-host", prefix.Addr().String()
	}
	return []string{"-n", action, family, kind, destination, "-interface", name}
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
