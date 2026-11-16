package mcp

import (
	"context"
	"encoding/json"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// socketPath is a path short enough for a Unix socket, which a test's own
// temporary folder isn't on a Mac.
func socketPath(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp("", "pm")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	return filepath.Join(dir, "pith", "mcp.sock")
}

func TestSocket(t *testing.T) {
	path := socketPath(t)
	l, err := Listen(path, fakeCaller(func(_ context.Context, name string, arguments json.RawMessage) Result {
		return Result{Text: name + " " + string(arguments), Failed: name == "stop_routing"}
	}))
	if err != nil {
		t.Fatal(err)
	}
	info, err := os.Lstat(path)
	if err != nil || info.Mode()&os.ModeSocket == 0 || info.Mode().Perm() != 0o600 {
		t.Fatalf("the socket: %v, %v: want a socket its user alone opens", info.Mode(), err)
	}
	if dir, err := os.Stat(filepath.Dir(path)); err != nil || dir.Mode().Perm() != 0o700 {
		t.Errorf("its folder: %v, %v", dir.Mode(), err)
	}
	if entries, _ := os.ReadDir(filepath.Dir(path)); len(entries) != 1 {
		t.Errorf("left beside the socket: %v", entries)
	}
	r := Remote{Path: path}
	if res := r.Call(context.Background(), "check", json.RawMessage(`{"target":"example.com"}`)); res.Failed || res.Text != `check {"target":"example.com"}` {
		t.Errorf("call: %+v", res)
	}
	if res := r.Call(context.Background(), "stop_routing", nil); !res.Failed || res.Text != "stop_routing " {
		t.Errorf("failed call: %+v", res)
	}
	// What isn't a call is told so, and the listener goes on.
	conn, err := net.Dial("unix", path)
	if err != nil {
		t.Fatal(err)
	}
	conn.Write([]byte("hello\n"))
	buf := make([]byte, 256)
	conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	if n, _ := conn.Read(buf); !strings.Contains(string(buf[:n]), `"failed":true`) {
		t.Errorf("answer to nonsense: %q", buf[:n])
	}
	conn.Close()
	if res := r.Call(context.Background(), "status", nil); res.Failed {
		t.Errorf("call after nonsense: %+v", res)
	}

	if err := l.Close(); err != nil {
		t.Error(err)
	}
	if _, err := os.Lstat(path); !os.IsNotExist(err) {
		t.Errorf("the socket stays after Close: %v", err)
	}
	// Without a Pith that listens, the answer says what to do.
	if res := r.Call(context.Background(), "status", nil); !res.Failed || !strings.Contains(res.Text, "open Pith") {
		t.Errorf("call without a listener: %+v", res)
	}
}

// TestSocketReplaced: a socket a Pith that crashed left is replaced, and a
// listener that closes late leaves the one that took its place.
func TestSocketReplaced(t *testing.T) {
	path := socketPath(t)
	os.MkdirAll(filepath.Dir(path), 0o700)
	stale, err := net.ListenUnix("unix", &net.UnixAddr{Name: path, Net: "unix"})
	if err != nil {
		t.Fatal(err)
	}
	stale.SetUnlinkOnClose(false)
	stale.Close()
	named := func(name string) Caller {
		return fakeCaller(func(context.Context, string, json.RawMessage) Result { return Result{Text: name} })
	}
	first, err := Listen(path, named("first"))
	if err != nil {
		t.Fatal(err)
	}
	r := Remote{Path: path}
	if res := r.Call(context.Background(), "status", nil); res.Text != "first" {
		t.Errorf("after a stale socket: %+v", res)
	}
	second, err := Listen(path, named("second"))
	if err != nil {
		t.Fatal(err)
	}
	defer second.Close()
	first.Close()
	if res := r.Call(context.Background(), "status", nil); res.Text != "second" {
		t.Errorf("after the first closed: %+v", res)
	}
}

// TestSocketCancel: a caller that leaves cancels its call in the app, and
// closing the listener cancels the ones that run.
func TestSocketCancel(t *testing.T) {
	path := socketPath(t)
	started := make(chan struct{}, 2)
	ended := make(chan error, 2)
	l, err := Listen(path, fakeCaller(func(ctx context.Context, _ string, _ json.RawMessage) Result {
		started <- struct{}{}
		<-ctx.Done()
		ended <- ctx.Err()
		return Result{Text: "late"}
	}))
	if err != nil {
		t.Fatal(err)
	}
	wait := func(what string) {
		t.Helper()
		select {
		case <-started:
		case err := <-ended:
			if err != context.Canceled {
				t.Errorf("%s: ended with %v", what, err)
			}
		case <-time.After(5 * time.Second):
			t.Fatalf("%s: nothing happened", what)
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	answered := make(chan Result, 2)
	go func() { answered <- Remote{Path: path}.Call(ctx, "change_ip", nil) }()
	wait("the call starting")
	cancel()
	if res := <-answered; !res.Failed || res.Text != "canceled" {
		t.Errorf("a canceled call: %+v", res)
	}
	wait("the call ending")

	go func() { answered <- Remote{Path: path}.Call(context.Background(), "change_ip", nil) }()
	wait("the second call starting")
	l.Close() // waits for the call, which it cancels
	wait("the second call ending")
	if res := <-answered; res.Text != "late" {
		t.Errorf("a call the listener's Close ended: %+v", res)
	}
}
