// Package mcp lets an agent work Pith for its user: a Model Context
// Protocol server whose tools read Pith's state, change its rules, scenes
// and local proxies, start and stop routing, and change public addresses.
//
// The tools run in the app, which alone holds the configuration and the
// local proxies: Listen serves them on a Unix socket that only the app's
// user can open. "Pith mcp", the same executable, is what an agent's
// program starts: Serve speaks the protocol on its standard input and
// output, and Remote passes each call to the app. Pith may open, quit and be
// replaced while the agent's session lasts: a call made meanwhile fails,
// and the next one finds the app again.
//
// It implements the tools of the protocol's 2025-11-25 revision, over its
// stdio transport: https://modelcontextprotocol.io/specification/2025-11-25
package mcp

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"io"
	"slices"
	"sync"

	"github.com/Kel077/pith/internal/api"
)

// Host is the Pith an agent works: its backend, and its configuration as the
// interface holds it, so that what an agent changes shows there at once and
// isn't lost to what the user changes meanwhile.
type Host interface {
	api.Service
	// Edit changes the configuration as fn has it, and returns it once it is
	// saved. Nothing changes when fn fails, or when what it leaves isn't
	// valid. fn only changes cfg: it must not block.
	Edit(ctx context.Context, fn func(cfg *api.Config) error) (api.Config, error)
	// Apps lists the apps installed, sorted by name.
	Apps(ctx context.Context) ([]api.AppRef, error)
}

// Result is what a tool answers: JSON, or, when it Failed, a sentence that
// says what went wrong.
type Result struct {
	Text   string `json:"text"`
	Failed bool   `json:"failed,omitempty"`
}

// Caller runs a tool by its name: the app's own, as Tools does, or the
// running app's, as Remote asks it.
type Caller interface {
	Call(ctx context.Context, name string, arguments json.RawMessage) Result
}

// versions are the revisions of the protocol Serve speaks, the latest
// first. The tools it offers are the same in each.
var versions = []string{"2025-11-25", "2025-06-18", "2025-03-26", "2024-11-05"}

// maxMessage bounds one message of the client's.
const maxMessage = 4 << 20

// JSON-RPC's error codes.
const (
	codeParse    = -32700
	codeRequest  = -32600
	codeMethod   = -32601
	codeParams   = -32602
	codeInternal = -32603
)

type message struct {
	Version string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method,omitempty"`
	Params  json.RawMessage `json:"params,omitempty"`
}

type response struct {
	Version string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Result  any             `json:"result,omitempty"`
	Error   *rpcError       `json:"error,omitempty"`
}

type rpcError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

// Serve speaks the protocol with one client, a JSON-RPC message a line,
// until in ends or ctx does: it answers the tools of this package, and has c
// run the ones the client calls, each on its own goroutine, so that a long
// one, as a change of address is, keeps no other waiting. Version is Pith's.
func Serve(ctx context.Context, in io.Reader, out io.Writer, c Caller, version string) error {
	s := &session{out: out, caller: c, version: version, calls: map[string]context.CancelFunc{}}
	ctx, cancel := context.WithCancel(ctx)
	defer s.running.Wait()
	defer cancel()
	lines := bufio.NewScanner(in)
	lines.Buffer(make([]byte, 0, 64<<10), maxMessage)
	for lines.Scan() {
		if line := bytes.TrimSpace(lines.Bytes()); len(line) > 0 {
			s.handle(ctx, line)
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
	}
	return lines.Err()
}

type session struct {
	caller  Caller
	version string
	running sync.WaitGroup

	mu    sync.Mutex // guards out and calls
	out   io.Writer
	calls map[string]context.CancelFunc // the tools running, by the ID of their request
}

func (s *session) handle(ctx context.Context, line []byte) {
	var m message
	if err := json.Unmarshal(line, &m); err != nil {
		code := codeParse
		if json.Valid(line) {
			code = codeRequest // a batch, or anything but an object
		}
		s.fail(json.RawMessage("null"), code, "not a JSON-RPC message")
		return
	}
	if len(m.ID) == 0 || string(m.ID) == "null" {
		s.notified(m)
		return
	}
	if m.Method == "" {
		return // an answer to a request this server never makes
	}
	switch m.Method {
	case "initialize":
		var p struct {
			Version string `json:"protocolVersion"`
		}
		json.Unmarshal(m.Params, &p)
		if !slices.Contains(versions, p.Version) {
			p.Version = versions[0]
		}
		s.reply(m.ID, map[string]any{
			"protocolVersion": p.Version,
			"capabilities":    map[string]any{"tools": map[string]any{}},
			"serverInfo":      map[string]any{"name": "pith", "title": "Pith", "version": s.version},
			"instructions":    instructions,
		})
	case "ping":
		s.reply(m.ID, map[string]any{})
	case "tools/list":
		s.reply(m.ID, map[string]any{"tools": listed})
	case "tools/call":
		var p struct {
			Name      string          `json:"name"`
			Arguments json.RawMessage `json:"arguments"`
		}
		if err := json.Unmarshal(m.Params, &p); err != nil {
			s.fail(m.ID, codeParams, "tools/call takes a name and arguments")
			return
		}
		if _, ok := byName[p.Name]; !ok {
			s.fail(m.ID, codeParams, "Unknown tool: "+p.Name)
			return
		}
		s.call(ctx, string(m.ID), p.Name, p.Arguments)
	default:
		s.fail(m.ID, codeMethod, "Method not found: "+m.Method)
	}
}

// notified takes a message that wants no answer: only the one that cancels
// a tool matters here.
func (s *session) notified(m message) {
	if m.Method != "notifications/cancelled" {
		return
	}
	var p struct {
		ID json.RawMessage `json:"requestId"`
	}
	if json.Unmarshal(m.Params, &p) != nil {
		return
	}
	s.mu.Lock()
	cancel := s.calls[string(p.ID)]
	s.mu.Unlock()
	if cancel != nil {
		cancel()
	}
}

// call runs a tool and answers with what it says, unless the client
// canceled it meanwhile: a canceled request gets no answer.
func (s *session) call(ctx context.Context, id, name string, arguments json.RawMessage) {
	ctx, cancel := context.WithCancel(ctx)
	s.mu.Lock()
	if s.calls[id] != nil {
		s.mu.Unlock()
		cancel()
		s.fail(json.RawMessage(id), codeRequest, "a request with this ID is running")
		return
	}
	s.calls[id] = cancel
	s.mu.Unlock()
	s.running.Go(func() {
		defer cancel()
		res := s.caller.Call(ctx, name, arguments)
		s.mu.Lock()
		delete(s.calls, id)
		s.mu.Unlock()
		if ctx.Err() != nil {
			return
		}
		s.reply(json.RawMessage(id), map[string]any{
			"content": []map[string]string{{"type": "text", "text": res.Text}},
			"isError": res.Failed,
		})
	})
}

func (s *session) reply(id json.RawMessage, result any) {
	s.write(response{Version: "2.0", ID: id, Result: result})
}

func (s *session) fail(id json.RawMessage, code int, msg string) {
	s.write(response{Version: "2.0", ID: id, Error: &rpcError{Code: code, Message: msg}})
}

// write sends one message: a line, which json.Marshal never breaks.
func (s *session) write(r response) {
	data, err := json.Marshal(r)
	if err != nil {
		data, _ = json.Marshal(response{Version: "2.0", ID: r.ID, Error: &rpcError{Code: codeInternal, Message: err.Error()}})
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.out.Write(append(data, '\n'))
}

// instructions tell an agent what Pith is, once, as it connects.
const instructions = `Pith is a macOS menu bar app that sends each app's connections out through the network interface its user chooses. These tools work the Pith that runs on this Mac; what they change shows in Pith at once.

How Pith decides: a scene is a named, ordered list of rules, and one scene routes at a time, while routing is on. A connection matches an enabled rule when its app, its destination and its protocol all match; an empty list of apps or targets matches every one. Block rules win over route rules; among route rules the first in the list wins. A route rule tries its interfaces in order. A connection no rule matches goes where macOS sends it. Rules take effect only while routing is on (status tells); local proxies work whether it is on or off.

Read before changing: get_config for the scenes and rules with their IDs, list_interfaces for the interfaces by name. After changing rules, check tells which rule a connection would match. Name interfaces by BSD name (en0), by the user's name for them or by their label; apps by path, bundle ID or name.

Change only what the user asked for. change_ip, stop_routing and removing things interrupt connections: change_ip takes a network offline for seconds to over a minute, and a router takes every device behind it along. Installing the helper and entering a hotspot's or router's admin password are the user's to do, in Pith.`
