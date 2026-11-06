//go:build darwin && arm64

package helper

import (
	"errors"
	"net"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/Kel077/pith/internal/helper/identity"
)

func TestPeerAuditIdentityAndGeneration(t *testing.T) {
	dir, err := os.MkdirTemp("", "pith-peer-")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(dir)
	listener, err := net.ListenUnix("unix", &net.UnixAddr{Name: filepath.Join(dir, "s"), Net: "unix"})
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	if err := listener.SetDeadline(time.Now().Add(2 * time.Second)); err != nil {
		t.Fatal(err)
	}
	client, err := net.DialUnix("unix", nil, listener.Addr().(*net.UnixAddr))
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	server, err := listener.AcceptUnix()
	if err != nil {
		t.Fatal(err)
	}
	defer server.Close()
	path, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	id, err := identity.Inspect(path, "test", ProtocolVersion)
	if err != nil {
		t.Fatal(err)
	}
	if err := VerifyPeer(client, uint32(os.Getuid()), id.CDHash); err != nil {
		t.Fatal(err)
	}
	if err := VerifyPeer(client, uint32(os.Getuid()), strings.Repeat("0", 40)); err == nil {
		t.Fatal("accepted a different executing artifact")
	}
	if err := VerifyPeer(client, ^uint32(0), id.CDHash); err == nil {
		t.Fatal("accepted a different owner")
	}
	token, err := peerToken(client)
	if err != nil {
		t.Fatal(err)
	}
	token[7]++
	if _, err := processCodeHash(token); !errors.Is(err, syscall.ESRCH) {
		t.Fatalf("reused PID generation was not rejected: %v", err)
	}
}
