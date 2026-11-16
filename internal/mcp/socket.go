package mcp

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"time"

	"github.com/Kel077/pith/internal/helper"
)

// SocketEnv names the environment variable that puts the socket elsewhere
// than in Pith's folder, for the app and for "Pith mcp" alike: for
// development, as a Pith on the mock backend never listens where the real
// one does.
const SocketEnv = "PITH_MCP_SOCKET"

// SocketPath is where the app listens for "Pith mcp": mcp.sock beside the
// configuration, in the user's Application Support folder, which is the
// same whatever started the program that asks.
func SocketPath() (string, error) {
	if path := os.Getenv(SocketEnv); path != "" {
		return path, nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, "Library", "Application Support", "Pith", "mcp.sock"), nil
}

// A call crosses the socket on a connection of its own: one line of JSON
// for the tool and its arguments, one for the Result. Closing the
// connection before the answer cancels the call.
type call struct {
	Tool      string          `json:"tool"`
	Arguments json.RawMessage `json:"arguments,omitempty"`
}

// maxCalls bounds the calls a Listener runs at once.
const maxCalls = 32

// Listener serves a Caller's tools on a Unix socket.
type Listener struct {
	ln     *net.UnixListener
	path   string
	file   os.FileInfo // the socket file, to remove no other's
	caller Caller
	cancel context.CancelFunc
	calls  sync.WaitGroup
	slots  chan struct{}
}

// Listen serves c's tools on the Unix socket at path until Close, to
// programs of the user who runs it alone: the socket's permissions keep
// others out, and each connection's peer is checked. A socket left there,
// as by a Pith that crashed, is replaced.
func Listen(path string, c Caller) (*Listener, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, err
	}
	// Bound under another name, closed to others, then moved into place: the
	// socket is never there with the permissions the umask gave it, and takes
	// the place of a stale one in one step.
	tmp := path + "." + strconv.Itoa(os.Getpid())
	os.Remove(tmp)
	ln, err := net.ListenUnix("unix", &net.UnixAddr{Name: tmp, Net: "unix"})
	if err != nil {
		return nil, err
	}
	ln.SetUnlinkOnClose(false)
	file, err := os.Lstat(tmp)
	if err == nil {
		err = os.Chmod(tmp, 0o600)
	}
	if err == nil {
		err = os.Rename(tmp, path)
	}
	if err != nil {
		ln.Close()
		os.Remove(tmp)
		return nil, err
	}
	ctx, cancel := context.WithCancel(context.Background())
	l := &Listener{ln: ln, path: path, file: file, caller: c, cancel: cancel, slots: make(chan struct{}, maxCalls)}
	l.calls.Go(func() { l.accept(ctx) })
	return l, nil
}

// Close stops listening, cancels the calls that run and waits for them.
func (l *Listener) Close() error {
	l.cancel()
	err := l.ln.Close()
	if now, statErr := os.Lstat(l.path); statErr == nil && os.SameFile(now, l.file) {
		os.Remove(l.path)
	}
	l.calls.Wait()
	return err
}

func (l *Listener) accept(ctx context.Context) {
	for {
		conn, err := l.ln.AcceptUnix()
		if err != nil {
			if ctx.Err() != nil || errors.Is(err, net.ErrClosed) {
				return
			}
			time.Sleep(50 * time.Millisecond) // out of file descriptors, most likely
			continue
		}
		l.calls.Go(func() { l.serve(ctx, conn) })
	}
}

func (l *Listener) serve(ctx context.Context, conn *net.UnixConn) {
	defer conn.Close()
	// The kernel tells who connected; nothing the peer says does.
	if uid, err := helper.PeerUID(conn); err != nil || uid != uint32(os.Getuid()) {
		return
	}
	answer := func(r Result) {
		data, _ := json.Marshal(r)
		conn.SetWriteDeadline(time.Now().Add(10 * time.Second))
		conn.Write(append(data, '\n'))
	}
	select {
	case l.slots <- struct{}{}:
		defer func() { <-l.slots }()
	default:
		answer(Result{Text: "Pith is running as many tools as it takes at once: try again in a moment", Failed: true})
		return
	}
	conn.SetReadDeadline(time.Now().Add(10 * time.Second))
	r := bufio.NewReader(io.LimitReader(conn, maxMessage))
	line, err := r.ReadBytes('\n')
	if err != nil {
		return
	}
	conn.SetReadDeadline(time.Time{})
	var c call
	if err := json.Unmarshal(line, &c); err != nil {
		answer(Result{Text: "not a call: " + err.Error(), Failed: true})
		return
	}
	// The caller says nothing more: when its end closes, it has gone.
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	go func() {
		io.Copy(io.Discard, r)
		cancel()
	}()
	answer(l.caller.Call(ctx, c.Tool, c.Arguments))
}

// Remote asks the app that listens on the Unix socket at Path: the Caller
// of "Pith mcp".
type Remote struct{ Path string }

// Call has the app run a tool. Without an app that listens, it fails with
// what to do about it.
func (r Remote) Call(ctx context.Context, name string, arguments json.RawMessage) Result {
	failed := func(format string, args ...any) Result {
		return Result{Text: fmt.Sprintf(format, args...), Failed: true}
	}
	d := net.Dialer{Timeout: 3 * time.Second}
	conn, err := d.DialContext(ctx, "unix", r.Path)
	if err != nil {
		if ctx.Err() != nil {
			return failed("canceled")
		}
		return failed("Pith isn't running on this Mac, or is a version without tools for agents: open Pith, then try again")
	}
	defer conn.Close()
	stop := context.AfterFunc(ctx, func() { conn.Close() })
	defer stop()
	data, err := json.Marshal(call{Tool: name, Arguments: arguments})
	if err != nil {
		return failed("arguments: %v", err)
	}
	if _, err := conn.Write(append(data, '\n')); err != nil {
		return failed("Pith didn't take the call: %v", err)
	}
	line, err := bufio.NewReader(conn).ReadBytes('\n')
	switch {
	case ctx.Err() != nil:
		return failed("canceled")
	case err != nil:
		return failed("Pith quit before it answered: what the tool was doing may be half done. Open Pith, and look before trying again")
	}
	var res Result
	if err := json.Unmarshal(line, &res); err != nil {
		return failed("Pith's answer can't be read: %v", err)
	}
	return res
}
