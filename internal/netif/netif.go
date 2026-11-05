// Package netif discovers interfaces and opens sockets bound to one of them.
// Discovery and probes never modify routes, DNS, or interface configuration.
package netif

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"slices"
	"strings"
	"sync/atomic"
	"time"

	"github.com/Kel077/pith/internal/api"
)

// List returns physical links and VPN interfaces, including disconnected links.
func List(ctx context.Context) ([]api.Interface, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	links, err := net.Interfaces()
	if err != nil {
		return nil, err
	}
	metadata, err := platformMetadata(ctx)
	if err != nil {
		return nil, err
	}
	result := make([]api.Interface, 0, len(links))
	for _, link := range links {
		if link.Flags&net.FlagLoopback != 0 {
			continue
		}
		meta, configured := metadata[link.Name]
		// Do not offer Apple peer-to-peer transports as internet exits.
		if !configured && (strings.HasPrefix(link.Name, "awdl") || strings.HasPrefix(link.Name, "llw") || strings.HasPrefix(link.Name, "anpi") || strings.HasPrefix(link.Name, "gif") || strings.HasPrefix(link.Name, "stf")) {
			continue
		}
		iface := meta
		iface.Name, iface.HardwareAddr = link.Name, link.HardwareAddr.String()
		if iface.Label == "" {
			iface.Label = link.Name
		}
		if iface.Kind == "" {
			iface.Kind = kind(link.Name, iface.Label, "", "")
		}
		addresses, err := link.Addrs()
		if err != nil {
			continue
		} // A hot-unplug can race the snapshot.
		for _, address := range addresses {
			prefix, err := netip.ParsePrefix(address.String())
			if err != nil {
				continue
			}
			addr := prefix.Addr().Unmap()
			if !addr.IsGlobalUnicast() {
				continue
			}
			if addr.Is4() {
				iface.IPv4 = append(iface.IPv4, addr.String())
			} else {
				iface.IPv6 = append(iface.IPv6, addr.String())
			}
		}
		slices.Sort(iface.IPv4)
		slices.Sort(iface.IPv6)
		iface.Up = link.Flags&net.FlagUp != 0 && link.Flags&net.FlagRunning != 0 && len(iface.IPv4)+len(iface.IPv6) > 0
		result = append(result, iface)
	}
	slices.SortFunc(result, func(a, b api.Interface) int {
		if a.Primary != b.Primary {
			if a.Primary {
				return -1
			}
			return 1
		}
		return strings.Compare(a.Name, b.Name)
	})
	return result, ctx.Err()
}

func kind(name, label, typ, hardware string) api.InterfaceKind {
	value := strings.ToLower(label + " " + typ + " " + hardware)
	switch {
	case strings.HasPrefix(name, "utun"), strings.HasPrefix(name, "ipsec"), strings.HasPrefix(name, "ppp"):
		return api.KindVPN
	case strings.Contains(value, "ieee80211"), strings.Contains(value, "wi-fi"), strings.Contains(value, "airport"):
		return api.KindWiFi
	case strings.Contains(value, "iphone"), strings.Contains(value, "cellular"), strings.Contains(value, "wwan"), strings.Contains(value, "hotspot"), strings.Contains(value, "m2000"):
		return api.KindCellular
	case strings.Contains(value, "thunderbolt"):
		return api.KindThunderbolt
	case strings.Contains(value, "ethernet"), strings.HasPrefix(name, "en"):
		return api.KindEthernet
	default:
		return api.KindOther
	}
}

// Dialer binds TCP and UDP sockets, including DNS, to name. An empty name uses
// system routing. servers are the chosen interface's DNS IPs, when available.
func Dialer(name string, timeout time.Duration, servers []string) (*net.Dialer, error) {
	index := 0
	if name != "" {
		iface, err := net.InterfaceByName(name)
		if err != nil {
			return nil, err
		}
		index = iface.Index
	}
	d := &net.Dialer{Timeout: timeout, KeepAlive: 30 * time.Second}
	if index == 0 {
		return d, nil
	}
	d.ControlContext = bindControl(index)
	dnsDialer := &net.Dialer{Timeout: timeout, ControlContext: bindControl(index)}
	var nextServer atomic.Uint32
	d.Resolver = &net.Resolver{PreferGo: true, Dial: func(ctx context.Context, network, address string) (net.Conn, error) {
		if len(servers) == 0 {
			return dnsDialer.DialContext(ctx, network, address)
		}
		var failures []error
		start := int(nextServer.Add(1)-1) % len(servers)
		for offset := range servers {
			server := servers[(start+offset)%len(servers)]
			if _, err := netip.ParseAddr(server); err != nil {
				continue
			}
			conn, err := dnsDialer.DialContext(ctx, network, net.JoinHostPort(server, "53"))
			if err == nil {
				return conn, nil
			}
			failures = append(failures, err)
		}
		return nil, fmt.Errorf("interface DNS: %w", errors.Join(failures...))
	}}
	return d, nil
}

// scDictionary parses scutil's human-readable dictionaries and arrays without
// interpreting strings as commands. Colons inside values (IPv6, labels) remain.
type scDictionary map[string]any

func parseStore(data string) (scDictionary, error) {
	scanner := bufio.NewScanner(strings.NewReader(data))
	scanner.Buffer(make([]byte, 4096), 1024*1024)
	var stack []scDictionary
	var root scDictionary
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}
		if line == "<dictionary> {" && len(stack) == 0 && root == nil {
			root = scDictionary{}
			stack = append(stack, root)
			continue
		}
		if line == "}" {
			if len(stack) == 0 {
				return nil, errors.New("scutil: unexpected closing brace")
			}
			stack = stack[:len(stack)-1]
			continue
		}
		if len(stack) == 0 {
			return nil, errors.New("scutil: expected dictionary")
		}
		key, value, ok := strings.Cut(line, " : ")
		if !ok {
			return nil, errors.New("scutil: expected key/value")
		}
		if value == "<dictionary> {" || value == "<array> {" {
			child := scDictionary{}
			stack[len(stack)-1][key] = child
			stack = append(stack, child)
		} else {
			stack[len(stack)-1][key] = value
		}
	}
	if err := scanner.Err(); err != nil {
		return nil, err
	}
	if root == nil || len(stack) != 0 {
		return nil, errors.New("scutil: incomplete dictionary")
	}
	return root, nil
}

func (d scDictionary) dict(key string) scDictionary { value, _ := d[key].(scDictionary); return value }
func (d scDictionary) str(key string) string        { value, _ := d[key].(string); return value }
func (d scDictionary) array(key string) []string {
	a := d.dict(key)
	var values []string
	for i := 0; i < len(a); i++ {
		if v := a.str(fmt.Sprint(i)); v != "" {
			values = append(values, v)
		}
	}
	return values
}

func storeInterfaces(store scDictionary) map[string]api.Interface {
	result := map[string]api.Interface{}
	var keys []string
	for key := range store {
		keys = append(keys, key)
	}
	slices.Sort(keys)
	// Prefer the primary service's metadata if several services share a device.
	primaryService := store.dict("State:/Network/Global/IPv4").str("PrimaryService")
	for _, key := range keys {
		if !strings.HasPrefix(key, "Setup:/Network/Service/") || !strings.HasSuffix(key, "/Interface") {
			continue
		}
		base := strings.TrimSuffix(key, "/Interface")
		id := strings.TrimPrefix(base, "Setup:/Network/Service/")
		props := store.dict(key)
		name := props.str("DeviceName")
		state := "State:/Network/Service/" + id
		if name == "" {
			name = store.dict(state + "/IPv4").str("InterfaceName")
		}
		if name == "" {
			name = store.dict(state + "/IPv6").str("InterfaceName")
		}
		if name == "" {
			continue
		}
		if _, exists := result[name]; exists && id != primaryService {
			continue
		}
		label := store.dict(base).str("UserDefinedName")
		if label == "" {
			label = props.str("UserDefinedName")
		}
		iface := api.Interface{Name: name, Label: label, Kind: kind(name, label, props.str("Type"), props.str("Hardware"))}
		iface.Router = store.dict(state + "/IPv4").str("Router")
		iface.Router6 = store.dict(state + "/IPv6").str("Router")
		iface.DNS = store.dict(state + "/DNS").array("ServerAddresses")
		if len(iface.DNS) == 0 {
			iface.DNS = store.dict(base + "/DNS").array("ServerAddresses")
		}
		result[name] = iface
	}
	return result
}
