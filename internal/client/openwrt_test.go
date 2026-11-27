//go:build darwin

package client

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Kel077/pith/internal/api"
)

const openWrtRotateReply = `{"ok":true,"old_mac":"02:00:00:00:00:41","new_mac":"02:00:00:00:00:42","mac_changed":true,"old_ipv4":"203.0.113.10","new_ipv4":"203.0.113.11","old_ipv6_prefix":"2001:db8:10::","new_ipv6_prefix":"2001:db8:11::","mwan3_wan":"online","mwan3_usbwan":"online","elapsed_sec":8}`

func openWrtConfigForTest(cfg api.Config) api.Config {
	cfg.OpenWrt = []api.OpenWrtRouter{{
		Via: api.InterfaceRef{Name: exampleExit.Name, HardwareAddr: exampleExit.HardwareAddr}, Host: "router.example.com",
	}}
	return cfg
}

func openWrtStatusReply(up bool, addr, wan, backup string) []byte {
	return fmt.Appendf(nil, `{"ok":true,"router":"example-openwrt","wan_up":%t,"ipv4":%q,"mwan3_wan":%q,"mwan3_usbwan":%q}`, up, addr, wan, backup)
}

func TestOpenWrtRotateWithoutStandbyOrHelper(t *testing.T) {
	c, n := exitClient(t)
	n.links[0].Kind = api.KindEthernet
	cfg := openWrtConfigForTest(api.DefaultConfig())
	if err := c.SaveConfig(t.Context(), cfg); err != nil {
		t.Fatal(err)
	}
	calls := []string{}
	c.openWrtSSH = func(_ context.Context, args ...string) ([]byte, error) {
		calls = append(calls, args[len(args)-1])
		if !slices.Equal(args[len(args)-5:len(args)-1], []string{"-B", exampleExit.Name, "--", "router.example.com"}) {
			t.Fatalf("SSH destination = %v", args)
		}
		if strings.HasSuffix(args[len(args)-1], " --status") {
			if len(calls) == 1 {
				return openWrtStatusReply(true, "203.0.113.10", "online", ""), nil
			}
			return openWrtStatusReply(true, "203.0.113.11", "online", ""), nil
		}
		n.mu.Lock()
		n.public = [2]string{"198.51.100.11", "2001:db8::11"}
		n.mu.Unlock()
		// The script has reacquired a lease but mwan3 has not switched back.
		return []byte(strings.Replace(openWrtRotateReply, `"mwan3_wan":"online"`, `"mwan3_wan":"offline"`, 1)), nil
	}

	got, err := c.RenewRouter(t.Context(), exampleExit.Name)
	if err != nil || got.Standby != "" || got.Tries != 1 || got.WAN == nil || !got.WAN.MACChanged ||
		got.WAN.IPv4 != "203.0.113.11" || got.WAN.IPv6Prefix != "2001:db8:11::" || got.WAN.WANState != "online" || got.WAN.Seconds != 8 ||
		got.Before.PublicIPv4 != "198.51.100.9" || got.After.PublicIPv4 != "198.51.100.11" ||
		got.Before.PublicIPv6 != "2001:db8::9" || got.After.PublicIPv6 != "2001:db8::11" || got.Took <= 0 {
		t.Fatalf("renewal = %+v, %v", got, err)
	}
	if len(calls) != 3 || !strings.HasSuffix(calls[1], " --rotate") {
		t.Fatalf("SSH calls = %v", calls)
	}
	for _, file := range []string{routerMark, drainFile} {
		if _, err := os.Stat(filepath.Join(c.opts.ConfigDir, file)); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("created a helper-side renewal mark %s: %v", file, err)
		}
	}
}

func TestOpenWrtProxyChangeKeepsPortOnlineAndRefreshesAddresses(t *testing.T) {
	c, n := exitClient(t)
	n.links[0].Kind = api.KindWiFi
	h := installExitPort(t, c, "192.0.2.10")
	port := proxyPort(t)
	cfg := proxyConfig(port, exampleExit.Name)
	cfg.Proxies[0].ChangeIP = true
	if err := c.SaveConfig(t.Context(), cfg); err != nil {
		t.Fatal(err)
	}
	plain := readExitPort(t, port, "/")
	if plain.CanChangeIP {
		t.Fatal("unconfigured port can change its address")
	}

	entered, release := make(chan struct{}), make(chan struct{})
	var released atomic.Bool
	defer func() {
		if released.CompareAndSwap(false, true) {
			close(release)
		}
	}()
	var rotates atomic.Int32
	c.openWrtSSH = func(ctx context.Context, args ...string) ([]byte, error) {
		switch {
		case strings.HasSuffix(args[len(args)-1], " --status"):
			return openWrtStatusReply(true, "203.0.113.10", "online", "online"), nil
		case strings.HasSuffix(args[len(args)-1], " --rotate"):
			if rotates.Add(1) == 1 {
				close(entered)
			}
			select {
			case <-ctx.Done():
				return nil, ctx.Err()
			case <-release:
			}
			n.mu.Lock()
			n.public = [2]string{"198.51.100.10", "2001:db8::10"}
			n.mu.Unlock()
			return []byte(openWrtRotateReply), nil
		default:
			return nil, errors.New("unexpected command")
		}
	}
	cfg = openWrtConfigForTest(cfg)
	if err := c.SaveConfig(t.Context(), cfg); err != nil {
		t.Fatal(err)
	}
	if before := readExitPort(t, port, "/"); !before.CanChangeIP || before.PublicIPv4 != plain.PublicIPv4 {
		t.Fatalf("configured port = %+v", before)
	}
	stream := openLocalProxy(t, port)
	answer := make(chan exitPortAnswer, 1)
	go func() { answer <- askExitPort(port, http.MethodPost, "/change-ip", nil) }()
	awaitExit(t, h.entered)
	awaitExit(t, entered)
	during := readExitPort(t, port, "/")
	if !during.Online || !during.Changing || !during.CanChangeIP || during.PublicIPv4 != "" || during.PublicIPv6 != "" {
		t.Fatalf("port while changing = %+v", during)
	}
	proxyExchange(t, stream, "connection stays open")
	if released.CompareAndSwap(false, true) {
		close(release)
	}
	select {
	case got := <-answer:
		change := got.change
		if got.status != http.StatusOK || got.err != nil || change.Port != port || change.WAN == nil || !change.WAN.MACChanged ||
			change.WAN.IPv4 != "203.0.113.11" || change.PublicIPv4 != "198.51.100.10" ||
			change.PublicIPv6 != "2001:db8::10" || change.BeforeIPv4 != "198.51.100.9" || change.Seconds <= 0 {
			t.Fatalf("POST = %d %s, %v", got.status, got.body, got.err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("POST did not finish")
	}
	calls := n.tests()
	if after := readExitPort(t, port, "/"); after.Changing || after.PublicIPv4 != "198.51.100.10" ||
		after.PublicIPv6 != "2001:db8::10" || after.At.IsZero() || calls != n.tests() {
		t.Fatalf("port cache after change = %+v, %v -> %v", after, calls, n.tests())
	}
	cfg.OpenWrt = nil
	if err := c.SaveConfig(t.Context(), cfg); err != nil {
		t.Fatal(err)
	}
	if after := readExitPort(t, port, "/"); after.CanChangeIP || rotates.Load() != 1 {
		t.Fatalf("removed router = %+v, rotations: %d", after, rotates.Load())
	}
}

func TestOpenWrtFailuresDoNotFallBackOrRepeatRotation(t *testing.T) {
	c, n := exitClient(t)
	n.links[0].Kind = api.KindEthernet
	installExitPort(t, c, "192.0.2.10")
	port := proxyPort(t)
	cfg := openWrtConfigForTest(proxyConfig(port, exampleExit.Name))
	cfg.Proxies[0].ChangeIP = true
	var up atomic.Bool
	var rotations atomic.Int32
	c.openWrtSSH = func(_ context.Context, args ...string) ([]byte, error) {
		if strings.HasSuffix(args[len(args)-1], " --status") {
			return openWrtStatusReply(up.Load(), "203.0.113.10", "offline", "online"), nil
		}
		rotations.Add(1)
		return []byte(`{"ok":false,"error":"failed to apply MAC"}`), errors.New("script failed")
	}
	if err := c.SaveConfig(t.Context(), cfg); err != nil {
		t.Fatal(err)
	}
	if r, err := c.RenewRouter(t.Context(), exampleExit.Name); api.Code(err) != api.ErrInterfaceUnavailable || r.Tries != 0 || rotations.Load() != 0 {
		t.Fatalf("down WAN rotation = %+v, %v", r, err)
	}
	up.Store(true)
	got := askExitPort(port, http.MethodPost, "/change-ip", nil)
	if got.status != http.StatusBadGateway || got.problem == nil || !strings.Contains(got.problem.Detail, "failed to apply MAC") || rotations.Load() != 1 {
		t.Fatalf("failed rotation = %d %s, %v, attempts %d", got.status, got.body, got.err, rotations.Load())
	}
	if after := readExitPort(t, port, "/"); !after.Online || !after.CanChangeIP || after.Changing {
		t.Fatalf("port after failed rotation = %+v", after)
	}
}
