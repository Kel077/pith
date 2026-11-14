package proxy

import (
	"context"
	"errors"
	"fmt"
	"net"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/Kel077/pith/internal/api"
)

func sharedProxy(port int) api.Proxy {
	p := testProxy(port)
	p.Shared = true
	return p
}

// establish asks an open connection for a CONNECT and expects it granted.
func establish(t *testing.T, c net.Conn) {
	t.Helper()
	if _, err := c.Write(append([]byte{5, 1, 0}, requestBytes(Target{Host: "192.0.2.9", Port: 443})...)); err != nil {
		t.Fatal(err)
	}
	readMethod(t, c, 0)
	if code, _ := readReply(t, c); code != 0 {
		t.Fatalf("CONNECT reply = %d", code)
	}
}

func connectFrom(t *testing.T, f *listenerFixture, port int, address string) *net.TCPConn {
	t.Helper()
	c := f.dialFrom(t, port, address)
	establish(t, c)
	return c
}

// refusedFrom connects as a device that isn't listed and expects to be told
// that no way of signing in is accepted, then the connection closed.
func refusedFrom(t *testing.T, f *listenerFixture, port int, address string) {
	t.Helper()
	c := f.dialFrom(t, port, address)
	if _, err := c.Write([]byte{5, 1, 0}); err != nil {
		t.Fatal(err)
	}
	readMethod(t, c, 0xff)
	readClosed(t, c)
}

func deviceOf(m *Manager, address string) (api.DeviceStatus, bool) {
	for _, d := range m.Devices() {
		if d.Address == address {
			return d, true
		}
	}
	return api.DeviceStatus{}, false
}

func TestSharedListenerTakesEveryAddress(t *testing.T) {
	m, f := newTestManager(t, Options{Dial: echoPeer(t).dial})
	shareTest(t, m, []string{"192.0.2.10"}, sharedProxy(1080), testProxy(1081))
	if f.host(1080) != "0.0.0.0" || f.host(1081) != "127.0.0.1" {
		t.Fatalf("listeners on %q and %q", f.host(1080), f.host(1081))
	}
	// A device that is listed still can't use a port that isn't shared: it
	// can't reach 127.0.0.1, and nothing says the port takes it.
	if m.Devices() != nil {
		t.Fatal("devices before any connected", m.Devices())
	}
}

func TestSharedProxyServesListedDevicesOnly(t *testing.T) {
	peer := echoPeer(t)
	var dials atomic.Int64
	m, f := newTestManager(t, Options{Dial: func(ctx context.Context, via api.InterfaceRef, target Target) (net.Conn, error) {
		dials.Add(1)
		return peer.dial(ctx, via, target)
	}})
	shareTest(t, m, []string{"192.0.2.10", "198.51.100.0/24"}, sharedProxy(1080))
	listed := connectFrom(t, f, 1080, "192.0.2.10")
	roundTrip(t, listed, "listed")
	ranged := connectFrom(t, f, 1080, "198.51.100.77")
	roundTrip(t, ranged, "in a range")
	own := connectTest(t, f, 1080)
	roundTrip(t, own, "this Mac")
	refusedFrom(t, f, 1080, "192.0.2.11")
	if dials.Load() != 3 {
		t.Fatalf("%d dials, want one a stream that was taken", dials.Load())
	}
	waitFor(t, func() bool {
		d, _ := deviceOf(m, "192.0.2.10")
		return d.Sent == 6 && d.Received == 6
	})
	devices := m.Devices()
	want := []struct {
		address string
		allowed bool
		open    int
	}{{"192.0.2.11", false, 0}, {"198.51.100.77", true, 1}, {"192.0.2.10", true, 1}}
	if len(devices) != len(want) {
		t.Fatalf("devices = %+v", devices)
	}
	for i, w := range want {
		if d := devices[i]; d.Address != w.address || d.Allowed != w.allowed || d.Open != w.open || d.Seen.IsZero() {
			t.Fatalf("device %d = %+v, want %+v", i, d, w)
		}
	}
	if st := m.Status(context.Background())[0]; st.Open != 3 || st.Sent != 6+10+8 {
		t.Fatalf("port status = %+v", st)
	}
	_ = listed.Close()
	waitFor(t, func() bool {
		d, ok := deviceOf(m, "192.0.2.10")
		return ok && d.Open == 0 && d.Sent == 6
	})
	roundTrip(t, ranged, "others stay")
	roundTrip(t, own, "others stay")
}

func TestUnlistingADeviceClosesItsStreams(t *testing.T) {
	m, f := newTestManager(t, Options{Dial: echoPeer(t).dial})
	shareTest(t, m, []string{"192.0.2.10", "192.0.2.20"}, sharedProxy(1080))
	old := m.Status(context.Background())[0]
	gone, kept, own := connectFrom(t, f, 1080, "192.0.2.10"), connectFrom(t, f, 1080, "192.0.2.20"), connectTest(t, f, 1080)
	shareTest(t, m, []string{"192.0.2.20"}, sharedProxy(1080))
	var data [1]byte
	if _, err := gone.Read(data[:]); !errors.Is(err, syscall.ECONNRESET) {
		t.Fatalf("an unlisted device's stream was not reset: %v", err)
	}
	roundTrip(t, kept, "still listed")
	roundTrip(t, own, "this Mac")
	if st := m.Status(context.Background())[0]; st.Since != old.Since || st.Open != 2 {
		t.Fatal("the listener restarted", st)
	}
	if devices := m.Devices(); len(devices) != 1 || devices[0].Address != "192.0.2.20" || !devices[0].Allowed {
		t.Fatalf("devices = %+v", devices)
	}
	refusedFrom(t, f, 1080, "192.0.2.10")
	if d, ok := deviceOf(m, "192.0.2.10"); !ok || d.Allowed {
		t.Fatalf("the device that came back = %+v, %v", d, ok)
	}
	// Listing it again takes it, and closes no one's stream.
	shareTest(t, m, []string{"192.0.2.20", "192.0.2.10"}, sharedProxy(1080))
	if d, ok := deviceOf(m, "192.0.2.10"); !ok || !d.Allowed {
		t.Fatalf("the device listed again = %+v, %v", d, ok)
	}
	roundTrip(t, connectFrom(t, f, 1080, "192.0.2.10"), "listed again")
	roundTrip(t, kept, "undisturbed")
}

func TestSharingStartsAndStopsWithItsListener(t *testing.T) {
	m, f := newTestManager(t, Options{Dial: echoPeer(t).dial})
	applyTest(t, m, testProxy(1080), testProxy(1081))
	first, second := connectTest(t, f, 1080), connectTest(t, f, 1081)
	shareTest(t, m, []string{"192.0.2.10"}, sharedProxy(1080), testProxy(1081))
	readClosed(t, first)
	roundTrip(t, second, "another port")
	if f.host(1080) != "0.0.0.0" {
		t.Fatalf("shared listener on %q", f.host(1080))
	}
	device := connectFrom(t, f, 1080, "192.0.2.10")
	roundTrip(t, device, "shared")
	shareTest(t, m, []string{"192.0.2.10"}, testProxy(1080), testProxy(1081))
	readClosed(t, device)
	roundTrip(t, second, "another port")
	if f.host(1080) != "127.0.0.1" {
		t.Fatalf("listener no longer shared on %q", f.host(1080))
	}
	if devices := m.Devices(); devices != nil {
		t.Fatalf("devices with nothing shared = %+v", devices)
	}
	roundTrip(t, connectTest(t, f, 1080), "this Mac")
}

func TestDisabledSharedProxyForgetsDevices(t *testing.T) {
	m, f := newTestManager(t, Options{Dial: echoPeer(t).dial})
	shareTest(t, m, []string{"192.0.2.10"}, sharedProxy(1080))
	device := connectFrom(t, f, 1080, "192.0.2.10")
	off := sharedProxy(1080)
	off.Enabled = false
	shareTest(t, m, []string{"192.0.2.10"}, off)
	readClosed(t, device)
	if devices := m.Devices(); devices != nil {
		t.Fatalf("devices with the shared proxy off = %+v", devices)
	}
}

func TestRefusedDevicesHoldNoSlot(t *testing.T) {
	m, f := newTestManager(t, Options{Dial: echoPeer(t).dial, MaxPerPort: 1})
	shareTest(t, m, nil, sharedProxy(1080))
	own := connectTest(t, f, 1080)
	silent := make([]*net.TCPConn, maxRefusing)
	for i := range silent {
		silent[i] = f.dialFrom(t, 1080, "192.0.2.99")
	}
	waitFor(t, func() bool { m.mu.Lock(); defer m.mu.Unlock(); return len(m.refusing) == maxRefusing })
	// One more than Pith tells at a time is reset at once: the reset may
	// reach it while it still connects, or right after.
	f.mu.Lock()
	f.from[1080] = append(f.from[1080], &deviceConn{remote: &net.TCPAddr{IP: net.ParseIP("192.0.2.99"), Port: 50000}})
	address := f.addrs[1080]
	f.mu.Unlock()
	if extra, err := net.DialTimeout("tcp4", address, time.Second); err == nil {
		defer extra.Close()
		_ = extra.SetDeadline(time.Now().Add(3 * time.Second))
		readClosed(t, extra)
	} else if !errors.Is(err, syscall.ECONNRESET) {
		t.Fatal(err)
	}
	roundTrip(t, own, "kept its slot")
	m.mu.Lock()
	total := m.total
	m.mu.Unlock()
	if total != 1 {
		t.Fatalf("%d sessions, want this Mac's alone", total)
	}
	if d, ok := deviceOf(m, "192.0.2.99"); !ok || d.Allowed || d.Open != 0 {
		t.Fatalf("refused device = %+v, %v", d, ok)
	}
	if err := m.Close(); err != nil {
		t.Fatal(err)
	}
	for _, c := range silent {
		readClosed(t, c)
	}
}

func TestDevicesForgetRefusedOnesFirst(t *testing.T) {
	m, f := newTestManager(t, Options{Dial: echoPeer(t).dial})
	shareTest(t, m, []string{"192.0.2.10"}, sharedProxy(1080))
	listed := connectFrom(t, f, 1080, "192.0.2.10")
	_ = listed.Close()
	waitFor(t, func() bool { m.mu.Lock(); defer m.mu.Unlock(); return m.total == 0 })
	for i := range api.MaxDevices + 5 {
		refusedFrom(t, f, 1080, fmt.Sprintf("198.51.100.%d", i+1))
	}
	if n := len(m.Devices()); n != api.MaxDevices {
		t.Fatalf("%d devices, want %d", n, api.MaxDevices)
	}
	if d, ok := deviceOf(m, "192.0.2.10"); !ok || !d.Allowed {
		t.Fatal("the listed device, the oldest, was forgotten for refused ones")
	}
	if _, ok := deviceOf(m, "198.51.100.1"); ok {
		t.Fatal("the refused device that came first was kept")
	}
	if _, ok := deviceOf(m, fmt.Sprintf("198.51.100.%d", api.MaxDevices+5)); !ok {
		t.Fatal("the refused device that came last was not kept")
	}
}

// A connection from the address it reached is this Mac's own, as one an app
// makes to this Mac's address on the network rather than to 127.0.0.1.
func TestThisMacsOwnAddressIsNotADevice(t *testing.T) {
	m, f := newTestManager(t, Options{Dial: echoPeer(t).dial})
	shareTest(t, m, nil, sharedProxy(1080))
	c := f.dialAs(t, 1080, &deviceConn{
		remote: &net.TCPAddr{IP: net.ParseIP("198.51.100.7"), Port: 50000},
		local:  &net.TCPAddr{IP: net.ParseIP("198.51.100.7"), Port: 1080},
	})
	establish(t, c)
	roundTrip(t, c, "own address")
	if devices := m.Devices(); devices != nil {
		t.Fatalf("this Mac was listed as a device: %+v", devices)
	}
}

func TestApplyRejectsInvalidDevices(t *testing.T) {
	m, _ := newTestManager(t, Options{Dial: echoPeer(t).dial})
	for _, address := range []string{"studio.example.com", "127.0.0.1", "2001:db8::1", "0.0.0.0/0"} {
		err := m.Apply(context.Background(), []api.Proxy{sharedProxy(1080)}, []api.Device{{Address: address}})
		if api.Code(err) != api.ErrInvalidConfig {
			t.Fatalf("device %q: %v", address, err)
		}
	}
	if st := m.Status(context.Background()); len(st) != 0 {
		t.Fatal("an invalid configuration opened a listener", st)
	}
}
