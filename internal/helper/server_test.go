//go:build darwin

package helper

import (
	"context"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func shortSocket(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp("", "pith-peer-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	return filepath.Join(dir, "s")
}

func TestRejectForeignUID(t *testing.T) {
	if os.Getuid() == 0 {
		t.Skip("root is intentionally authorized")
	}
	path := shortSocket(t)
	server := NewServer(ServerOptions{SocketPath: path, OwnerUID: uint32(os.Getuid() + 1)})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- server.Run(ctx) }()
	defer func() {
		cancel()
		select {
		case err := <-done:
			if err != nil {
				t.Error(err)
			}
		case <-time.After(2 * time.Second):
			t.Error("server failed to exit")
		}
	}()
	var conn net.Conn
	for attempt := 0; attempt < 100; attempt++ {
		var err error
		conn, err = net.DialTimeout("unix", path, 20*time.Millisecond)
		if err == nil {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	if conn == nil {
		t.Fatal("server did not listen")
	}
	defer conn.Close()
	conn.SetDeadline(time.Now().Add(time.Second))
	_, _ = conn.Write([]byte("GET /v1/status HTTP/1.1\r\nHost: pith\r\n\r\n"))
	var response [32]byte
	n, err := conn.Read(response[:])
	if err == nil || n != 0 {
		t.Fatal("unauthorized peer received a response")
	}
}

func TestSocketPathProtection(t *testing.T) {
	path := shortSocket(t)
	if err := os.WriteFile(path, []byte("do not remove"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := NewServer(ServerOptions{SocketPath: path}).Run(context.Background()); err == nil {
		t.Fatal("removed non-socket path")
	}
	data, err := os.ReadFile(path)
	if err != nil || string(data) != "do not remove" {
		t.Fatal("existing file changed")
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	listener, err := net.ListenUnix("unix", &net.UnixAddr{Name: path, Net: "unix"})
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	if err := NewServer(ServerOptions{SocketPath: path}).Run(context.Background()); err == nil {
		t.Fatal("replaced active socket")
	}
}
