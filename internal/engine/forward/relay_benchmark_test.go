package forward

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Kel077/pith/internal/api"
	"golang.org/x/sys/unix"
)

// These are models, not native utun measurements: both TCP stacks, the memory
// link and relay run in this process. Only interface enumeration touches the OS,
// read-only, for a dynamically selected loopback device; no real network traffic
// or route/interface mutation occurs. The alternate lookup and MTU apply only to
// test objects and must not be read as production configuration changes.
type benchmarkCheckedConn struct {
	net.Conn
	check   func() error
	maxRead int
	checks  atomic.Uint64
}

func (c *benchmarkCheckedConn) Read(p []byte) (int, error) {
	c.checks.Add(1)
	if err := c.check(); err != nil {
		return 0, err
	}
	if c.maxRead > 0 && len(p) > c.maxRead {
		p = p[:c.maxRead]
	}
	return c.Conn.Read(p)
}
func (c *benchmarkCheckedConn) Write(p []byte) (int, error) {
	c.checks.Add(1)
	if err := c.check(); err != nil {
		return 0, err
	}
	return c.Conn.Write(p)
}

func benchmarkLoopback(b *testing.B) net.Interface {
	b.Helper()
	ifaces, err := net.Interfaces()
	if err != nil {
		b.Fatal(err)
	}
	for _, iface := range ifaces {
		if iface.Flags&net.FlagLoopback != 0 && iface.Flags&net.FlagUp != 0 {
			return iface
		}
	}
	b.Skip("no loopback interface available for read-only identity checks")
	return net.Interface{}
}

func benchmarkInterfaceCheck(iface net.Interface, mode string) func() error {
	return func() error {
		if mode == "none" {
			return nil
		}
		var current *net.Interface
		var err error
		if mode == "name" {
			current, err = net.InterfaceByName(iface.Name)
		} else {
			current, err = net.InterfaceByIndex(iface.Index)
		}
		if err != nil {
			return err
		}
		if current.Index != iface.Index || current.Name != iface.Name {
			return errors.New("loopback identity changed during benchmark")
		}
		return nil
	}
}

func BenchmarkInterfaceIdentityLookup(b *testing.B) {
	iface := benchmarkLoopback(b)
	for _, mode := range []string{"name", "index"} {
		b.Run(mode, func(b *testing.B) {
			check := benchmarkInterfaceCheck(iface, mode)
			b.ReportAllocs()
			for b.Loop() {
				if err := check(); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}

func BenchmarkMemoryTCPRelay(b *testing.B) {
	iface := benchmarkLoopback(b)
	for _, direction := range []string{"download", "upload"} {
		for _, mode := range []string{"name", "index", "none"} {
			for _, mtu := range []uint32{1500, 9000} {
				b.Run(fmt.Sprintf("%s/%s/mtu%d", direction, mode, mtu), func(b *testing.B) {
					benchmarkMemoryTransfer(b, iface, direction, mode, mtu, 32<<10)
				})
			}
		}
	}
}

// Model sparse native reads independently of internal packet MTU. A fast server
// can fill 32 KiB reads; a slower link need not. Do not extrapolate per-byte CPU
// from a single burst size or mix setup time into steady transfer cost.
func BenchmarkMemorySmallReads(b *testing.B) {
	iface := benchmarkLoopback(b)
	for _, mode := range []string{"name", "index", "none"} {
		b.Run(mode, func(b *testing.B) { benchmarkMemoryTransfer(b, iface, "download", mode, 1500, 1460) })
	}
}

func benchmarkMemoryTransfer(b *testing.B, iface net.Interface, direction, mode string, mtu uint32, maxRead int) {
	const chunk = 4 << 20
	payload := make([]byte, chunk)
	serverDone := make(chan error, 1)
	var connMu sync.Mutex
	var outbound *benchmarkCheckedConn
	var server net.Conn
	dial := func(context.Context, string, string, string) (net.Conn, error) {
		connMu.Lock()
		defer connMu.Unlock()
		if outbound != nil {
			return nil, errors.New("benchmark unexpectedly created a second flow")
		}
		client, peer := net.Pipe()
		server = peer
		outbound = &benchmarkCheckedConn{Conn: client, check: benchmarkInterfaceCheck(iface, mode), maxRead: maxRead}
		go func() {
			defer peer.Close()
			var marker [1]byte
			for {
				_, err := io.ReadFull(peer, marker[:])
				if err != nil {
					serverDone <- err
					return
				}
				if direction == "download" {
					_, err = peer.Write(payload)
				} else {
					_, err = io.CopyN(io.Discard, peer, chunk)
					if err == nil {
						_, err = peer.Write(marker[:])
					}
				}
				if err != nil {
					serverDone <- err
					return
				}
			}
		}()
		return outbound, nil
	}
	x := newFixture(b, dial, api.Scene{ID: "benchmark"}, func(opts *Options) {
		settings := api.DefaultSettings()
		settings.SniffDomains = false
		if err := opts.Router.Reload(api.Scene{ID: "benchmark"}, settings); err != nil {
			b.Fatal(err)
		}
	})
	x.f.link.SetMTU(mtu)
	x.device.peer.SetMTU(mtu)
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	b.Cleanup(cancel)
	var app net.Conn
	b.Cleanup(func() {
		if app != nil {
			app.Close()
		}
		x.f.Close()
		connMu.Lock()
		peer := server
		connMu.Unlock()
		if peer != nil {
			peer.Close()
			select {
			case <-serverDone:
			case <-time.After(time.Second):
				b.Error("benchmark server did not exit")
			}
		}
	})
	client, err := x.dialTCP(ctx, "198.51.100.20:443")
	if err != nil {
		b.Fatal(err)
	}
	app = client
	if err := app.SetDeadline(time.Now().Add(45 * time.Second)); err != nil {
		b.Fatal(err)
	}
	transfer := func() error {
		if _, err := app.Write([]byte{1}); err != nil {
			return err
		}
		if direction == "download" {
			_, err := io.CopyN(io.Discard, app, chunk)
			return err
		}
		if _, err := app.Write(payload); err != nil {
			return err
		}
		var ack [1]byte
		_, err := io.ReadFull(app, ack[:])
		return err
	}
	if err := transfer(); err != nil {
		b.Fatal(err)
	}
	connMu.Lock()
	measured := outbound
	connMu.Unlock()
	startChecks := measured.checks.Load()
	b.SetBytes(chunk)
	b.ReportAllocs()
	cpuBefore := benchmarkCPUTime(b)
	b.ResetTimer()
	for b.Loop() {
		if err := transfer(); err != nil {
			b.Fatal(err)
		}
	}
	b.StopTimer()
	cpuElapsed := benchmarkCPUTime(b) - cpuBefore
	if cpuElapsed > 0 {
		bytes := float64(b.N) * chunk
		b.ReportMetric(float64(cpuElapsed.Nanoseconds())/bytes, "cpu-ns/B")
		b.ReportMetric(bytes*8/cpuElapsed.Seconds()/1e6, "cpu-Mbit/s")
	}
	b.ReportMetric(float64(measured.checks.Load()-startChecks)/float64(b.N), "identity-checks/op")
}

// Includes userspace and kernel CPU across every thread in this test process,
// including the second TCP stack. This is not a helper-only hardware estimate.
func benchmarkCPUTime(b *testing.B) time.Duration {
	b.Helper()
	var usage unix.Rusage
	if err := unix.Getrusage(unix.RUSAGE_SELF, &usage); err != nil {
		b.Fatal(err)
	}
	return time.Duration(usage.Utime.Sec+usage.Stime.Sec)*time.Second + time.Duration(usage.Utime.Usec+usage.Stime.Usec)*time.Microsecond
}
