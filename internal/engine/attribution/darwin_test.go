//go:build darwin

package attribution

import (
	"context"
	"net"
	"net/netip"
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"time"

	"github.com/Kel077/pith/internal/api"
)

func TestLiveProcessPath(t *testing.T) {
	path, err := ProcessPath(os.Getpid())
	if err != nil {
		t.Fatal(err)
	}
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	want, err := filepath.EvalSymlinks(executable)
	if err != nil {
		t.Fatal(err)
	}
	got, err := filepath.EvalSymlinks(path)
	if err != nil || got != want {
		t.Fatal("proc_pidpath does not identify this test process", err)
	}
	id, err := processIdentity(os.Getpid())
	if err != nil || id.unique == 0 {
		t.Fatal("missing unique process identity", err)
	}
}

func TestLiveTCPAttribution(t *testing.T) {
	listener, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	conn, err := net.DialTimeout("tcp4", listener.Addr().String(), time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	accepted, err := listener.Accept()
	if err != nil {
		t.Fatal(err)
	}
	defer accepted.Close()
	checkLiveFlow(t, api.TCP, conn.LocalAddr().String(), conn.RemoteAddr().String())
}

func TestLiveUDPAttribution(t *testing.T) {
	listener, err := net.ListenPacket("udp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	conn, err := net.DialTimeout("udp4", listener.LocalAddr().String(), time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	if _, err := conn.Write([]byte("pith")); err != nil {
		t.Fatal(err)
	}
	checkLiveFlow(t, api.UDP, conn.LocalAddr().String(), conn.RemoteAddr().String())
}

func TestLiveIPv6Attribution(t *testing.T) {
	listener, err := net.Listen("tcp6", "[::1]:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	conn, err := net.DialTimeout("tcp6", listener.Addr().String(), time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	accepted, err := listener.Accept()
	if err != nil {
		t.Fatal(err)
	}
	defer accepted.Close()
	checkLiveFlow(t, api.TCP, conn.LocalAddr().String(), conn.RemoteAddr().String())
}

func checkLiveFlow(t *testing.T, proto api.Protocol, local, remote string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	result, err := New().Lookup(ctx, proto, netip.MustParseAddrPort(local), netip.MustParseAddrPort(remote))
	if err != nil {
		t.Fatal(err)
	}
	if result.Process.PID != os.Getpid() || result.EffectivePID != os.Getpid() || result.Process.Path == "" || result.App.Path == "" {
		t.Fatal("live socket was not attributed to this test process")
	}
}

func TestBundleFIFOInfoPlistDoesNotBlock(t *testing.T) {
	for _, withWriter := range []bool{false, true} {
		name := "without writer"
		if withWriter {
			name = "with writer"
		}
		t.Run(name, func(t *testing.T) {
			bundle := filepath.Join(t.TempDir(), "Example.app")
			contents := filepath.Join(bundle, "Contents")
			if err := os.MkdirAll(contents, 0700); err != nil {
				t.Fatal(err)
			}
			path := filepath.Join(contents, "Info.plist")
			if err := syscall.Mkfifo(path, 0600); err != nil {
				t.Fatal(err)
			}
			// Exercise both a blocking Open (no writer) and a blocking Read (an idle
			// writer). The failure path releases either case before reporting it.
			var keeper *os.File
			if withWriter {
				var err error
				keeper, err = os.OpenFile(path, os.O_RDWR|syscall.O_NONBLOCK, 0)
				if err != nil {
					t.Fatal(err)
				}
				defer keeper.Close()
			}
			type result struct {
				app api.AppRef
				err error
			}
			done := make(chan result, 1)
			go func() {
				app, err := AppForPath(filepath.Join(contents, "MacOS", "Example"))
				done <- result{app, err}
			}()
			select {
			case got := <-done:
				if got.err != nil || got.app.Path != bundle || got.app.Name != "Example" || got.app.BundleID != "" {
					t.Fatal("lost bundle path fallback", got.app, got.err)
				}
			case <-time.After(time.Second):
				if keeper != nil {
					keeper.Close()
				} else if writer, err := os.OpenFile(path, os.O_WRONLY|syscall.O_NONBLOCK, 0); err == nil {
					writer.Close()
				}
				select {
				case <-done:
				case <-time.After(time.Second):
				}
				t.Fatal("non-regular Info.plist blocked attribution")
			}
		})
	}
}
