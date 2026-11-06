//go:build darwin

package client

import (
	"context"
	"errors"
	"net"
	"net/http"
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

type fakeEngine struct {
	helper.IdleBackend
	mu              sync.Mutex
	status          api.Status
	lastScene       api.Scene
	reloads, starts int
	subs            map[chan api.Event]bool
}

func newFakeEngine() *fakeEngine {
	return &fakeEngine{status: api.Status{State: api.Stopped}, subs: make(map[chan api.Event]bool)}
}
func (e *fakeEngine) Status(context.Context) (api.Status, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.status, nil
}
func (e *fakeEngine) publish() {
	value := e.status
	for ch := range e.subs {
		select {
		case ch <- api.Event{Status: &value}:
		default:
		}
	}
}
func (e *fakeEngine) Start(ctx context.Context, scene api.Scene, settings api.Settings) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.starts++
	e.status = api.Status{State: api.Running, SceneID: scene.ID}
	e.lastScene = scene
	e.publish()
	return nil
}
func (e *fakeEngine) Reload(ctx context.Context, scene api.Scene, settings api.Settings) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.reloads++
	e.lastScene = scene
	return nil
}
func (e *fakeEngine) Stop(ctx context.Context) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.status = api.Status{State: api.Stopped}
	e.publish()
	return nil
}
func (e *fakeEngine) Subscribe(ctx context.Context) (<-chan api.Event, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	ch := make(chan api.Event, 16)
	e.subs[ch] = true
	initial := e.status
	ch <- api.Event{Status: &initial}
	go func() { <-ctx.Done(); e.mu.Lock(); defer e.mu.Unlock(); delete(e.subs, ch); close(ch) }()
	return ch, nil
}

func startLocalHelper(t *testing.T, version string, backend helper.Backend) (*Client, context.CancelFunc, <-chan error) {
	t.Helper()
	dir, err := os.MkdirTemp("", "pith-ipc-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	socket := filepath.Join(dir, "s")
	ctx, cancel := context.WithCancel(context.Background())
	server := helper.NewServer(helper.ServerOptions{SocketPath: socket, Version: version, OwnerUID: uint32(os.Getuid()), Backend: backend, WatchInterfaces: func(ctx context.Context) (<-chan []api.Interface, error) {
		ch := make(chan []api.Interface, 1)
		ch <- []api.Interface{{Name: "en0", Label: "Example", Up: true}}
		go func() { <-ctx.Done(); close(ch) }()
		return ch, nil
	}})
	done := make(chan error, 1)
	go func() { done <- server.Run(ctx) }()
	t.Cleanup(func() {
		cancel()
		select {
		case err := <-done:
			if err != nil {
				t.Error(err)
			}
		case <-time.After(3 * time.Second):
			t.Error("helper server did not stop")
		}
	})
	for attempt := 0; attempt < 100; attempt++ {
		conn, err := net.DialTimeout("unix", socket, 20*time.Millisecond)
		if err == nil {
			conn.Close()
			break
		}
		if attempt == 99 {
			t.Fatal("local helper did not start")
		}
		time.Sleep(5 * time.Millisecond)
	}
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	expected, err := identity.Inspect(executable, "test-helper", helper.ProtocolVersion)
	if err != nil {
		t.Fatal(err)
	}
	c, err := New(Options{ConfigDir: filepath.Join(dir, "config"), SocketPath: socket, Version: "test-app", HelperPath: executable, ExpectedHelper: &expected})
	if err != nil {
		t.Fatal(err)
	}
	c.peerUID = uint32(os.Getuid()) // Only same-process tests accept an unprivileged helper.
	c.binaryPath = executable
	c.artifactOpen = func(path string, _ bool) (*os.File, artifactStamp, error) { return openArtifact(path, false) }
	return c, cancel, done
}

func TestIPCStartHotReloadAndSceneRemoval(t *testing.T) {
	backend := newFakeEngine()
	c, _, _ := startLocalHelper(t, "test", backend)
	ctx := context.Background()
	if err := c.Start(ctx, "default"); err != nil {
		t.Fatal(err)
	}
	status, err := c.Status(ctx)
	if err != nil || status.State != api.Running || !status.Helper.Running || !status.Helper.Installed {
		t.Fatal(status, err)
	}
	cfg := api.DefaultConfig()
	cfg.Scenes[0].Rules = []api.Rule{{ID: "block", Enabled: true, Protocol: api.TCP, Action: api.Block, Targets: []string{"example.com"}}}
	if err := c.SaveConfig(ctx, cfg); err != nil {
		t.Fatal(err)
	}
	backend.mu.Lock()
	reloads, rules := backend.reloads, len(backend.lastScene.Rules)
	backend.mu.Unlock()
	if reloads != 1 || rules != 1 {
		t.Fatal("saved rules were not hot-applied")
	}
	cfg.Scenes = []api.Scene{{ID: "replacement"}}
	cfg.Selected = "replacement"
	if err := c.SaveConfig(ctx, cfg); err != nil {
		t.Fatal(err)
	}
	status, err = c.Status(ctx)
	if err != nil || status.State != api.Stopped {
		t.Fatal(status, err)
	}
	if _, err := c.Connections(ctx, api.ConnectionQuery{}); err != nil {
		t.Fatal(err)
	}
	if _, err := c.Traffic(ctx, api.TrafficQuery{}); err != nil {
		t.Fatal(err)
	}
	if err := c.ClearConnections(ctx); err != nil {
		t.Fatal(err)
	}
}

func TestIPCSubscriptionOrderAndCancellation(t *testing.T) {
	c, _, _ := startLocalHelper(t, "test", newFakeEngine())
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	cfg := api.DefaultConfig()
	cfg.Aliases = map[string]string{"en0": "Desk"}
	if err := c.SaveConfig(ctx, cfg); err != nil {
		t.Fatal(err)
	}
	events, err := c.Subscribe(ctx)
	if err != nil {
		t.Fatal(err)
	}
	first, second := <-events, <-events
	if first.Status == nil || second.Interfaces == nil || second.Status != nil {
		t.Fatal("incorrect initial event order")
	}
	if len(second.Interfaces) != 1 || second.Interfaces[0].Label != "Example" {
		t.Fatal("subscription replaced the system label with an alias", second.Interfaces)
	}
	if err := c.Start(ctx, "default"); err != nil {
		t.Fatal(err)
	}
	select {
	case event := <-events:
		if event.Status == nil || event.Status.State != api.Running {
			t.Fatal(event)
		}
	case <-ctx.Done():
		t.Fatal("missing running event")
	}
	cancel()
	select {
	case _, ok := <-events:
		if ok {
			t.Fatal("unexpected event after cancellation")
		}
	case <-time.After(time.Second):
		t.Fatal("subscription did not close")
	}
}

func TestIPCArtifactAndRootPeerChecks(t *testing.T) {
	c, _, _ := startLocalHelper(t, "older", newFakeEngine())
	status, err := c.Status(context.Background())
	if err != nil || status.Helper.Outdated || status.Helper.Version != "test-helper" {
		t.Fatal(status, err)
	}
	// Informational app and peer version strings cannot trigger replacement.
	c.opts.Version = "new-app-version"
	if err := c.Start(context.Background(), "default"); err != nil {
		t.Fatal(err)
	}
	checkFile := c.checkFile
	c.checkFile = func(path string, installed bool) error {
		if installed {
			return errArtifactMismatch
		}
		return checkFile(path, installed)
	}
	status, err = c.Status(context.Background())
	if err != nil || !status.Helper.Outdated || status.Helper.Running || status.Helper.Version != "" {
		t.Fatal(status, err)
	}
	if err := c.Start(context.Background(), "default"); api.Code(err) != api.ErrHelperOutdated {
		t.Fatal(err)
	}
	c.checkFile = checkFile
	if os.Getuid() != 0 {
		c.peerUID = 0
		if status, err := c.Status(context.Background()); err != nil || status.State != api.Failed || api.Code(status.Error) != api.ErrHelperUntrusted {
			t.Fatal("client did not report an untrusted helper", status, err)
		}
	}
}

func TestIPCRejectsBadRequestsAndIdleStart(t *testing.T) {
	c, _, _ := startLocalHelper(t, "test", helper.IdleBackend{})
	if err := c.Start(context.Background(), "default"); api.Code(err) != api.ErrInternal {
		t.Fatal(err)
	}
	req, err := http.NewRequest(http.MethodPost, "http://pith/v1/start", strings.NewReader(`{}`))
	if err != nil {
		t.Fatal(err)
	}
	response, err := c.http.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	if err := responseError(response); api.Code(err) != api.ErrHelperOutdated {
		t.Fatal(err)
	}
	response.Body.Close()
	if err := c.request(context.Background(), http.MethodPost, "/v1/start", helper.StartRequest{}, nil); api.Code(err) != api.ErrInvalidConfig {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := c.Status(ctx); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
}
