//go:build darwin && arm64

package client

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Kel077/pith/internal/api"
	"github.com/Kel077/pith/internal/helper"
	"github.com/Kel077/pith/internal/helper/identity"
)

func TestArtifactCacheRechecksIdentityAndPermissions(t *testing.T) {
	path := filepath.Join(t.TempDir(), "helper")
	contents := []byte("first artifact bytes")
	if err := os.WriteFile(path, contents, 0755); err != nil {
		t.Fatal(err)
	}
	h := sha256.Sum256(contents)
	expected := identity.Identity{SHA256: fmt.Sprintf("%x", h)}
	var cache artifactCache
	check := func() error { return cache.check(path, false, expected, openArtifact) }
	if err := check(); err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	for range 8 {
		wg.Go(func() {
			if err := check(); err != nil {
				t.Error(err)
			}
		})
	}
	wg.Wait()
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("other artifact bytes"), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(path, info.ModTime(), info.ModTime()); err != nil {
		t.Fatal(err)
	}
	if err := check(); !errors.Is(err, errArtifactMismatch) {
		t.Fatal("same-size content change with restored mtime bypassed cache", err)
	}
	if err := os.WriteFile(path, contents, 0755); err != nil {
		t.Fatal(err)
	}
	if err := check(); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, 0777); err != nil {
		t.Fatal(err)
	}
	if err := check(); err == nil {
		t.Fatal("writable helper bypassed cached validation")
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	other := filepath.Join(filepath.Dir(path), "other")
	if err := os.WriteFile(other, contents, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(other, path); err != nil {
		t.Fatal(err)
	}
	if err := check(); err == nil {
		t.Fatal("symlink bypassed cached validation")
	}
}

func TestUntrustedStatusNeverClaimsStoppedOrPeerData(t *testing.T) {
	c, _, _ := startLocalHelper(t, "unverified-version", newFakeEngine())
	c.expectedHelper.CDHash = strings.Repeat("0", 40)
	status, err := c.Status(context.Background())
	if err != nil || status.State != api.Failed || status.Error == nil || status.Error.Code != api.ErrHelperUntrusted ||
		!status.Helper.Installed || status.Helper.Running || status.Helper.Outdated || status.Helper.Version != "" || status.SceneID != "" {
		t.Fatal(status, err)
	}
	for name, call := range map[string]func() error{
		"start":       func() error { return c.Start(context.Background(), "default") },
		"stop":        func() error { return c.Stop(context.Background()) },
		"connections": func() error { _, err := c.Connections(context.Background(), api.ConnectionQuery{}); return err },
		"traffic":     func() error { _, err := c.Traffic(context.Background(), api.TrafficQuery{}); return err },
		"clear":       func() error { return c.ClearConnections(context.Background()) },
	} {
		if err := call(); api.Code(err) != api.ErrHelperUntrusted {
			t.Errorf("%s: %v", name, err)
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	events, err := c.Subscribe(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if first := <-events; first.Status == nil || first.Status.State != api.Failed || api.Code(first.Status.Error) != api.ErrHelperUntrusted || first.Status.Helper.Running || first.Status.Helper.Version != "" || first.Status.SceneID != "" {
		t.Fatal("local subscription claimed unverified peer data", first)
	}
}

func TestRejectedPeerReceivesNoHTTPBytes(t *testing.T) {
	dir, err := os.MkdirTemp("", "pith-client-peer-")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(dir)
	socket := filepath.Join(dir, "s")
	listener, err := net.Listen("unix", socket)
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	if err := listener.(*net.UnixListener).SetDeadline(time.Now().Add(3 * time.Second)); err != nil {
		t.Fatal(err)
	}
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	expected, err := identity.Inspect(executable, "test", helper.ProtocolVersion)
	if err != nil {
		t.Fatal(err)
	}
	expected.CDHash = strings.Repeat("0", 39) + "1"
	c, err := New(Options{ConfigDir: dir, SocketPath: socket, HelperPath: executable, ExpectedHelper: &expected})
	if err != nil {
		t.Fatal(err)
	}
	c.peerUID = uint32(os.Getuid())
	c.binaryPath = executable
	c.artifactOpen = func(path string, _ bool) (*os.File, artifactStamp, error) { return openArtifact(path, false) }
	c.checkFile = func(path string, _ bool) error {
		return c.artifacts.check(path, false, c.expectedHelper, c.artifactOpen)
	}
	done := make(chan error, 1)
	go func() {
		conn, err := listener.Accept()
		if err != nil {
			done <- err
			return
		}
		defer conn.Close()
		if err := conn.SetReadDeadline(time.Now().Add(2 * time.Second)); err != nil {
			done <- err
			return
		}
		var data [1]byte
		n, err := conn.Read(data[:])
		if n != 0 || !errors.Is(err, io.EOF) {
			done <- fmt.Errorf("rejected peer received request bytes or was not closed: %d, %w", n, err)
			return
		}
		done <- nil
	}()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	status, err := c.Status(ctx)
	if err != nil || status.Error == nil || status.Error.Code != api.ErrHelperUntrusted {
		t.Error(status, err)
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}

func TestInvalidBundleCannotAuthorizeInstall(t *testing.T) {
	c := testClient(t)
	c.identityErr = errors.New("missing embedded identity")
	if err := c.InstallHelper(context.Background()); api.Code(err) != api.ErrHelperUntrusted {
		t.Fatal(err)
	}
	status, err := c.Status(context.Background())
	if err != nil || status.State != api.Failed || status.Error == nil || status.Error.Code != api.ErrHelperUntrusted {
		t.Fatal(status, err)
	}
}

func TestMovedBundleDoesNotPreventStatusSubscribeOrStop(t *testing.T) {
	c, _, _ := startLocalHelper(t, "test", newFakeEngine())
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if err := c.Start(ctx, "default"); err != nil {
		t.Fatal(err)
	}
	c.opts.HelperPath = filepath.Join(t.TempDir(), "bundle-was-moved")
	if _, err := c.checkBundledHelper(); api.Code(err) != api.ErrHelperUntrusted {
		t.Fatal("test bundle is still available", err)
	}
	if status, err := c.Status(ctx); err != nil || status.State != api.Running {
		t.Fatal("moved bundle revoked verified helper status", status, err)
	}
	events, err := c.Subscribe(ctx)
	if err != nil {
		t.Fatal(err)
	}
	select {
	case event := <-events:
		if event.Status == nil || event.Status.State != api.Running {
			t.Fatal(event)
		}
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	if err := c.Stop(ctx); err != nil {
		t.Fatal("moved bundle prevented safe Stop", err)
	}
	if status, err := c.Status(ctx); err != nil || status.State != api.Stopped {
		t.Fatal(status, err)
	}
}

func TestMissingAnchorDoesNotInventAnInstallation(t *testing.T) {
	c := testClient(t)
	c.identityErr = errors.New("missing anchor")
	status, err := c.Status(context.Background())
	if err != nil || status.State != api.Failed || status.Error == nil || status.Error.Code != api.ErrHelperUntrusted || status.Helper.Installed {
		t.Fatal(status, err)
	}
}
