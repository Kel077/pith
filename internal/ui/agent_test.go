package ui

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Kel077/pith/internal/api"
	"github.com/Kel077/pith/internal/mcp"
	"github.com/Kel077/pith/internal/mock"
)

func TestAgentEdit(t *testing.T) {
	a, svc := testApp(t, mock.Options{Sample: true, HelperInstalled: true})
	g := a.Agent()
	ctx := context.Background()
	a.sceneID = "office" // the scene the rules page shows

	// What an agent changes shows in the interface, and is saved.
	rule := api.Rule{ID: "r-new", Enabled: true, Protocol: api.AnyProtocol, Action: api.Block, Targets: []string{"example.com"}}
	got, err := g.Edit(ctx, func(cfg *api.Config) error {
		cfg.Scenes[0].Rules = append(cfg.Scenes[0].Rules, rule)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	saved, _ := svc.LoadConfig(ctx)
	for name, cfg := range map[string]api.Config{"answered": got, "shown": a.cfg, "saved": saved} {
		if rules := cfg.Scenes[0].Rules; len(rules) != 6 || rules[5].ID != "r-new" {
			t.Errorf("%s: %d rules in the first scene, want the new one last", name, len(rules))
		}
	}
	if read, err := g.LoadConfig(ctx); err != nil || len(read.Scenes[0].Rules) != 6 {
		t.Errorf("LoadConfig: %v, %v", read.Scenes, err)
	}

	// A change that fails, or leaves what can't be saved, changes nothing.
	gen := a.gen
	if _, err := g.Edit(ctx, func(cfg *api.Config) error {
		cfg.Scenes = nil
		return errors.New("no")
	}); err == nil || err.Error() != "no" {
		t.Errorf("a failed change: %v", err)
	}
	if _, err := g.Edit(ctx, func(cfg *api.Config) error {
		cfg.Selected = "nowhere"
		return nil
	}); api.Code(err) != api.ErrInvalidConfig {
		t.Errorf("an invalid change: %v", err)
	}
	if a.gen != gen || len(a.cfg.Scenes) != 2 || a.cfg.Selected != "home" {
		t.Errorf("refused changes changed the interface's configuration: %+v", a.cfg)
	}

	// The scene the interface shows is gone: it shows the selected one.
	if _, err := g.Edit(ctx, func(cfg *api.Config) error {
		cfg.Scenes = cfg.Scenes[:1]
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if a.sceneID != "home" {
		t.Errorf("the interface shows scene %q, which is gone", a.sceneID)
	}
}

func TestAgentRouting(t *testing.T) {
	a, svc := testApp(t, mock.Options{Sample: true, HelperInstalled: true})
	g := a.Agent()
	ctx := context.Background()
	if err := g.Start(ctx, "office"); err != nil {
		t.Fatal(err)
	}
	st, _ := svc.Status(ctx)
	saved, _ := svc.LoadConfig(ctx)
	if st.State != api.Running || st.SceneID != "office" || a.cfg.Selected != "office" || saved.Selected != "office" || !a.wasOn {
		t.Errorf("started: %s by %q, selected %q, saved %q, left on %v", st.State, st.SceneID, a.cfg.Selected, saved.Selected, a.wasOn)
	}
	if err := g.Start(ctx, "nowhere"); api.Code(err) != api.ErrNoScene || a.cfg.Selected != "office" {
		t.Errorf("starting a scene that isn't: %v, selected %q", err, a.cfg.Selected)
	}
	if err := g.Stop(ctx); err != nil {
		t.Fatal(err)
	}
	if st, _ := svc.Status(ctx); st.State != api.Stopped || a.wasOn {
		t.Errorf("stopped: %s, left on %v", st.State, a.wasOn)
	}
}

// mainThread runs what is posted to it in order on one goroutine, as the
// app's main thread does.
type mainThread struct {
	posts chan func()
	done  chan struct{}
}

func newMainThread(t *testing.T) *mainThread {
	m := &mainThread{posts: make(chan func(), 1024), done: make(chan struct{})}
	go func() {
		defer close(m.done)
		for fn := range m.posts {
			fn()
		}
	}()
	t.Cleanup(func() {
		close(m.posts)
		<-m.done
	})
	return m
}

// do runs fn on the thread and waits for it.
func (m *mainThread) do(fn func()) {
	ran := make(chan struct{})
	m.posts <- func() { fn(); close(ran) }
	<-ran
}

// slowSaves is a backend whose saves take a while, and fail when told to.
type slowSaves struct {
	api.Service
	mu   sync.Mutex
	fail error
	n    int
}

func (s *slowSaves) SaveConfig(ctx context.Context, cfg api.Config) error {
	time.Sleep(time.Millisecond)
	s.mu.Lock()
	err := s.fail
	s.n++
	s.mu.Unlock()
	if err != nil {
		return err
	}
	return s.Service.SaveConfig(ctx, cfg)
}

// TestAgentBesideUser: agents and the user change the configuration at the
// same time, as the app runs it: saves off the main thread, the newest
// snapshot alone. Every change is kept, and each agent hears of its save.
func TestAgentBesideUser(t *testing.T) {
	svc := &slowSaves{Service: mock.New(mock.Options{Sample: true, HelperInstalled: true})}
	main := newMainThread(t)
	a := New(Options{Service: svc, Desktop: &fakeDesktop{}, Post: func(fn func()) { main.posts <- fn }})
	ctx := context.Background()
	cfg, _ := svc.LoadConfig(ctx)
	main.do(func() { a.setConfig(cfg) })
	g := a.Agent()

	add := func(id string) func(cfg *api.Config) {
		return func(cfg *api.Config) {
			cfg.Scenes[0].Rules = append(cfg.Scenes[0].Rules, api.Rule{ID: id, Enabled: true, Protocol: api.AnyProtocol, Action: api.Block})
		}
	}
	const each = 40
	var agents sync.WaitGroup
	for k := range 4 {
		agents.Go(func() {
			for i := range each {
				if _, err := g.Edit(ctx, func(cfg *api.Config) error {
					add(fmt.Sprintf("agent-%d-%d", k, i))(cfg)
					return nil
				}); err != nil {
					t.Errorf("agent %d, change %d: %v", k, i, err)
				}
			}
		})
	}
	for i := range each {
		main.do(func() { a.change(add(fmt.Sprintf("user-%d", i))) })
	}
	agents.Wait()
	// The last agent's change was saved before its Edit returned, with
	// everything before it; the user's last may still be on its way.
	last := make(chan error, 1)
	main.do(func() {
		a.gen++
		saved := a.saveWait(a.gen, cloneConfig(a.cfg))
		go func() { last <- <-saved }()
	})
	if err := <-last; err != nil {
		t.Fatal(err)
	}
	saved, _ := svc.LoadConfig(ctx)
	var shown api.Config
	main.do(func() { shown = cloneConfig(a.cfg) })
	want := 5 + 5*each
	if len(saved.Scenes[0].Rules) != want || len(shown.Scenes[0].Rules) != want {
		t.Errorf("%d rules saved, %d shown, want %d", len(saved.Scenes[0].Rules), len(shown.Scenes[0].Rules), want)
	}
	svc.mu.Lock()
	n := svc.n
	svc.mu.Unlock()
	if n == 0 || n > 5*each+2 {
		t.Errorf("%d saves", n)
	}

	// A save that fails: the agent hears why, and the interface goes back to
	// what is saved.
	svc.mu.Lock()
	svc.fail = &api.Error{Code: api.ErrInternal, Detail: "disk full"}
	svc.mu.Unlock()
	if _, err := g.Edit(ctx, func(cfg *api.Config) error {
		add("lost")(cfg)
		return nil
	}); err == nil || !strings.Contains(err.Error(), "disk full") {
		t.Errorf("a failed save: %v", err)
	}
	main.do(func() {}) // after the failure was posted
	main.do(func() { shown = cloneConfig(a.cfg) })
	if len(shown.Scenes[0].Rules) != want {
		t.Errorf("after a failed save the interface shows %d rules, want the %d saved", len(shown.Scenes[0].Rules), want)
	}

	// An agent that left gets no change made in its name.
	gone, cancel := context.WithCancel(ctx)
	cancel()
	if _, err := g.Edit(gone, func(cfg *api.Config) error {
		t.Error("a change ran for an agent that left")
		return nil
	}); !errors.Is(err, context.Canceled) {
		t.Errorf("an agent that left: %v", err)
	}
}

// TestAgentEndToEnd is the whole way an agent's call goes: the protocol on
// a pipe, as "Pith mcp" speaks it, the socket, the tools, the interface.
func TestAgentEndToEnd(t *testing.T) {
	a, svc := testApp(t, mock.Options{Sample: true, HelperInstalled: true})
	dir, err := os.MkdirTemp("", "pm")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(dir)
	path := filepath.Join(dir, "mcp.sock")
	l, err := mcp.Listen(path, mcp.Tools{Host: a.Agent()})
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()

	inR, inW := io.Pipe()
	outR, outW := io.Pipe()
	served := make(chan error, 1)
	go func() {
		served <- mcp.Serve(context.Background(), inR, outW, mcp.Remote{Path: path}, "test")
		outW.Close()
	}()
	out := bufio.NewReader(outR)
	// call has a tool run and returns its answer's text.
	id := 0
	call := func(name, arguments string) string {
		t.Helper()
		id++
		fmt.Fprintf(inW, `{"jsonrpc":"2.0","id":%d,"method":"tools/call","params":{"name":%q,"arguments":%s}}`+"\n", id, name, arguments)
		line, err := out.ReadBytes('\n')
		if err != nil {
			t.Fatal(err)
		}
		var m struct {
			ID     int `json:"id"`
			Result struct {
				Content []struct {
					Text string `json:"text"`
				} `json:"content"`
				IsError bool `json:"isError"`
			} `json:"result"`
		}
		if err := json.Unmarshal(line, &m); err != nil || m.ID != id || len(m.Result.Content) != 1 {
			t.Fatalf("%s answered %s (%v)", name, line, err)
		}
		if m.Result.IsError {
			t.Fatalf("%s failed: %s", name, m.Result.Content[0].Text)
		}
		return m.Result.Content[0].Text
	}

	var added struct {
		Rule api.Rule `json:"rule"`
	}
	json.Unmarshal([]byte(call("add_rule", `{"apps": ["Safari"], "targets": ["*.example.com"], "via": ["M2000"]}`)), &added)
	if r := a.cfg.Scenes[0].Rules[0]; r.ID == "" || r.ID != added.Rule.ID || r.Apps[0].BundleID != "com.apple.Safari" || r.Via[0].Name != "en9" {
		t.Errorf("the interface's first rule: %+v, answered %+v", r, added.Rule)
	}
	if saved, _ := svc.LoadConfig(context.Background()); saved.Scenes[0].Rules[0].ID != added.Rule.ID {
		t.Errorf("the rule isn't saved")
	}
	if text := call("check", `{"target": "www.example.com", "app": "Safari"}`); !strings.Contains(text, `"outcome":"routed"`) || !strings.Contains(text, `"interface":"en9"`) {
		t.Errorf("check: %s", text)
	}
	if text := call("start_routing", `{}`); !strings.Contains(text, `"state":"running"`) || !a.wasOn {
		t.Errorf("start: %s, left on %v", text, a.wasOn)
	}
	if text := call("change_ip", `{"port": 1081}`); !strings.Contains(text, `"changed":true`) {
		t.Errorf("change_ip: %s", text)
	}
	var cfg api.Config
	if err := json.Unmarshal([]byte(call("get_config", `{}`)), &cfg); err != nil || len(cfg.Scenes[0].Rules) != 6 {
		t.Errorf("get_config: %v, %+v", err, cfg.Scenes)
	}

	inW.Close()
	if err := <-served; err != nil {
		t.Errorf("Serve: %v", err)
	}
}
