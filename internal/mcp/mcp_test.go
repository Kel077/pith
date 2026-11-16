package mcp

import (
	"bufio"
	"context"
	"encoding/json"
	"io"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"
)

// fakeCaller answers each tool with what its test says.
type fakeCaller func(ctx context.Context, name string, arguments json.RawMessage) Result

func (f fakeCaller) Call(ctx context.Context, name string, arguments json.RawMessage) Result {
	return f(ctx, name, arguments)
}

// client speaks the protocol to a Serve of its own over pipes.
type client struct {
	t      *testing.T
	in     *io.PipeWriter
	out    *bufio.Reader
	served chan error
}

func serve(t *testing.T, c Caller) *client {
	t.Helper()
	inR, inW := io.Pipe()
	outR, outW := io.Pipe()
	cl := &client{t: t, in: inW, out: bufio.NewReader(outR), served: make(chan error, 1)}
	go func() {
		cl.served <- Serve(context.Background(), inR, outW, c, "1.2.3")
		outW.Close()
	}()
	t.Cleanup(func() {
		inW.Close()
		go io.Copy(io.Discard, outR)
		select {
		case <-cl.served:
		case <-time.After(5 * time.Second):
			t.Error("Serve didn't end with its input")
		}
	})
	return cl
}

func (c *client) send(line string) {
	c.t.Helper()
	if _, err := io.WriteString(c.in, line+"\n"); err != nil {
		c.t.Fatal(err)
	}
}

// answer is one message of the server's.
type answer struct {
	ID     json.RawMessage `json:"id"`
	Result json.RawMessage `json:"result"`
	Error  *rpcError       `json:"error"`
}

func (c *client) read() answer {
	c.t.Helper()
	got := make(chan answer, 1)
	go func() {
		line, err := c.out.ReadBytes('\n')
		var a answer
		if err == nil {
			err = json.Unmarshal(line, &a)
		}
		if err != nil {
			c.t.Errorf("reading an answer: %v (%q)", err, line)
		}
		got <- a
	}()
	select {
	case a := <-got:
		return a
	case <-time.After(5 * time.Second):
		c.t.Fatal("no answer")
		return answer{}
	}
}

// toolResult is what tools/call answers.
type toolResult struct {
	Content []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	} `json:"content"`
	IsError bool `json:"isError"`
}

func (a answer) tool(t *testing.T) toolResult {
	t.Helper()
	if a.Error != nil {
		t.Fatalf("protocol error %d: %s", a.Error.Code, a.Error.Message)
	}
	var r toolResult
	if err := json.Unmarshal(a.Result, &r); err != nil || len(r.Content) != 1 || r.Content[0].Type != "text" {
		t.Fatalf("not a tool's result: %s (%v)", a.Result, err)
	}
	return r
}

func TestInitialize(t *testing.T) {
	c := serve(t, fakeCaller(nil))
	for _, tc := range []struct{ asked, want string }{
		{"2025-11-25", "2025-11-25"},
		{"2024-11-05", "2024-11-05"},
		{"1999-01-01", versions[0]}, // unknown: the latest this server speaks
	} {
		c.send(`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"` + tc.asked + `","capabilities":{},"clientInfo":{"name":"test","version":"0"}}}`)
		var r struct {
			ProtocolVersion string `json:"protocolVersion"`
			Capabilities    struct {
				Tools *struct{} `json:"tools"`
			} `json:"capabilities"`
			ServerInfo struct {
				Name    string `json:"name"`
				Version string `json:"version"`
			} `json:"serverInfo"`
			Instructions string `json:"instructions"`
		}
		a := c.read()
		if err := json.Unmarshal(a.Result, &r); err != nil {
			t.Fatal(err)
		}
		if r.ProtocolVersion != tc.want || r.Capabilities.Tools == nil || r.ServerInfo.Name != "pith" || r.ServerInfo.Version != "1.2.3" || r.Instructions == "" {
			t.Errorf("initialize %s: %s", tc.asked, a.Result)
		}
	}
	// A notification gets no answer: the next one is the ping's.
	c.send(`{"jsonrpc":"2.0","method":"notifications/initialized"}`)
	c.send(`{"jsonrpc":"2.0","id":"p","method":"ping"}`)
	if a := c.read(); string(a.ID) != `"p"` || string(a.Result) != `{}` {
		t.Errorf("ping: %+v", a)
	}
}

func TestProtocolErrors(t *testing.T) {
	c := serve(t, fakeCaller(nil))
	for _, tc := range []struct {
		line string
		code int
	}{
		{`not json`, codeParse},
		{`[{"jsonrpc":"2.0","id":1,"method":"ping"}]`, codeRequest},
		{`{"jsonrpc":"2.0","id":2,"method":"resources/list"}`, codeMethod},
		{`{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"no_such_tool"}}`, codeParams},
		{`{"jsonrpc":"2.0","id":4,"method":"tools/call","params":"status"}`, codeParams},
	} {
		c.send(tc.line)
		if a := c.read(); a.Error == nil || a.Error.Code != tc.code {
			t.Errorf("%s: %+v, want error %d", tc.line, a, tc.code)
		}
	}
}

// TestToolsList checks each tool's schema against the arguments it decodes:
// what the schema promises is what the tool takes.
func TestToolsList(t *testing.T) {
	c := serve(t, fakeCaller(nil))
	c.send(`{"jsonrpc":"2.0","id":1,"method":"tools/list"}`)
	var r struct {
		Tools []struct {
			Name        string `json:"name"`
			Description string `json:"description"`
			InputSchema struct {
				Type       string                     `json:"type"`
				Properties map[string]json.RawMessage `json:"properties"`
				Required   []string                   `json:"required"`
				Additional *bool                      `json:"additionalProperties"`
			} `json:"inputSchema"`
			Annotations struct {
				ReadOnly    bool `json:"readOnlyHint"`
				Destructive bool `json:"destructiveHint"`
			} `json:"annotations"`
		} `json:"tools"`
	}
	if err := json.Unmarshal(c.read().Result, &r); err != nil {
		t.Fatal(err)
	}
	if len(r.Tools) != len(tools) {
		t.Fatalf("%d tools listed, want %d", len(r.Tools), len(tools))
	}
	for i, got := range r.Tools {
		tl := tools[i]
		if got.Name != tl.name || got.Description == "" {
			t.Errorf("tool %d: %q, want %q with a description", i, got.Name, tl.name)
		}
		if got.InputSchema.Type != "object" || got.InputSchema.Additional == nil || *got.InputSchema.Additional {
			t.Errorf("%s: its schema is not a closed object", tl.name)
		}
		var takes []string
		for name := range got.InputSchema.Properties {
			takes = append(takes, name)
		}
		fields := jsonFields(tl.does.arguments)
		slices.Sort(takes)
		slices.Sort(fields)
		if !slices.Equal(takes, fields) {
			t.Errorf("%s: its schema has %v, its arguments %v", tl.name, takes, fields)
		}
		for _, name := range got.InputSchema.Required {
			if !slices.Contains(fields, name) {
				t.Errorf("%s: requires %q, which it doesn't take", tl.name, name)
			}
		}
		if got.Annotations.ReadOnly != (tl.kind == reads) || got.Annotations.Destructive != (tl.kind == disrupts) {
			t.Errorf("%s: annotations %+v for kind %d", tl.name, got.Annotations, tl.kind)
		}
	}
}

// jsonFields lists the names a struct decodes from JSON, embedded structs'
// included.
func jsonFields(t reflect.Type) []string {
	var names []string
	for i := range t.NumField() {
		f := t.Field(i)
		if f.Anonymous {
			names = append(names, jsonFields(f.Type)...)
			continue
		}
		name, _, _ := strings.Cut(f.Tag.Get("json"), ",")
		names = append(names, name)
	}
	return names
}

func TestCall(t *testing.T) {
	c := serve(t, fakeCaller(func(_ context.Context, name string, arguments json.RawMessage) Result {
		if name == "stop_routing" {
			return Result{Text: "helper_missing", Failed: true}
		}
		return Result{Text: name + " " + string(arguments)}
	}))
	c.send(`{"jsonrpc":"2.0","id":7,"method":"tools/call","params":{"name":"check","arguments":{"target":"example.com"}}}`)
	a := c.read()
	if r := a.tool(t); string(a.ID) != "7" || r.IsError || r.Content[0].Text != `check {"target":"example.com"}` {
		t.Errorf("call: %s", a.Result)
	}
	// A tool that fails is an answer, not a protocol error: the agent reads why.
	c.send(`{"jsonrpc":"2.0","id":8,"method":"tools/call","params":{"name":"stop_routing"}}`)
	if r := c.read().tool(t); !r.IsError || r.Content[0].Text != "helper_missing" {
		t.Errorf("failed call: %+v", r)
	}
}

// TestCallsRunTogether: a tool that takes long, as a change of address
// does, keeps no other waiting, and a canceled one gets no answer.
func TestCallsRunTogether(t *testing.T) {
	started := make(chan struct{})
	ended := make(chan error, 1)
	c := serve(t, fakeCaller(func(ctx context.Context, name string, _ json.RawMessage) Result {
		if name != "change_ip" {
			return Result{Text: "quick"}
		}
		close(started)
		<-ctx.Done()
		ended <- ctx.Err()
		return Result{Text: "late"}
	}))
	c.send(`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"change_ip"}}`)
	<-started
	c.send(`{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"status"}}`)
	if a := c.read(); string(a.ID) != "2" || a.tool(t).Content[0].Text != "quick" {
		t.Fatalf("the quick call waited for the slow one: %+v", a)
	}
	c.send(`{"jsonrpc":"2.0","method":"notifications/cancelled","params":{"requestId":1,"reason":"test"}}`)
	select {
	case err := <-ended:
		if err != context.Canceled {
			t.Errorf("the canceled call ended with %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the call wasn't canceled")
	}
	// Nothing answers request 1: the next answer is the ping's.
	c.send(`{"jsonrpc":"2.0","id":3,"method":"ping"}`)
	if a := c.read(); string(a.ID) != "3" {
		t.Errorf("after a canceled call: %+v", a)
	}
}
