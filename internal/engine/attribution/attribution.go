package attribution

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/netip"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/Kel077/pith/internal/api"
	"github.com/Kel077/pith/internal/rules"
	"howett.net/plist"
)

type Attribution struct {
	App api.AppRef
	// Process always describes the socket owner, even for a delegated socket.
	Process      api.Process
	EffectivePID int
	Delegated    bool
	// SocketGeneration is XNU's already-observed socket lifetime identity. A zero
	// value cannot authorize carrying a UDP name across a relay replacement.
	SocketGeneration uint64
}

type identity struct {
	unique  uint64
	version uint32
	uuid    [16]byte
}
type processEntry struct {
	identity identity
	process  api.Process
	app      api.AppRef
	until    time.Time
}
type snapshot struct {
	at      time.Time
	sockets []Socket
}

// Resolver caches PCB snapshots for 100 ms, refreshes once on a miss, and
// checks a process's unique identity before using its cached executable path.
// It owns no background goroutines or persistent system resources.
type Resolver struct {
	mu        sync.Mutex
	tables    map[api.Protocol]snapshot
	processes map[int]processEntry
	read      func(api.Protocol) ([]Socket, error)
	identify  func(int) (identity, error)
	path      func(int) (string, error)
}

func New() *Resolver {
	return &Resolver{tables: make(map[api.Protocol]snapshot), processes: make(map[int]processEntry), read: Snapshot, identify: processIdentity, path: ProcessPath}
}

func (r *Resolver) Lookup(ctx context.Context, proto api.Protocol, local, remote netip.AddrPort) (Attribution, error) {
	return r.lookup(ctx, proto, local, remote, false)
}

// LookupFresh independently reads the PCB table before comparing socket
// lifetimes. A cached tuple cannot prove that its native port was not reused.
func (r *Resolver) LookupFresh(ctx context.Context, proto api.Protocol, local, remote netip.AddrPort) (Attribution, error) {
	return r.lookup(ctx, proto, local, remote, true)
}

func (r *Resolver) lookup(ctx context.Context, proto api.Protocol, local, remote netip.AddrPort, fresh bool) (Attribution, error) {
	if !local.IsValid() || local.Port() == 0 || !remote.IsValid() {
		return Attribution{}, fmt.Errorf("invalid flow tuple")
	}
	if proto != api.TCP && proto != api.UDP {
		return Attribution{}, fmt.Errorf("unsupported protocol %q", proto)
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return Attribution{}, err
	}
	table := r.tables[proto]
	refreshed := false
	if fresh || time.Since(table.at) > 100*time.Millisecond {
		sockets, err := r.read(proto)
		if err != nil {
			return Attribution{}, err
		}
		table = snapshot{at: time.Now(), sockets: sockets}
		r.tables[proto] = table
		refreshed = true
	}
	socket, err := Find(table.sockets, local, remote)
	if errors.Is(err, ErrNotFound) && !refreshed {
		sockets, readErr := r.read(proto)
		if readErr != nil {
			return Attribution{}, readErr
		}
		r.tables[proto] = snapshot{at: time.Now(), sockets: sockets}
		socket, err = Find(sockets, local, remote)
	}
	if err != nil {
		return Attribution{}, err
	}
	owner, ownerApp, ownerErr := r.process(socket.PID)
	if ownerErr != nil {
		owner = api.Process{PID: socket.PID}
	}
	result := Attribution{Process: owner, App: ownerApp, EffectivePID: socket.PID, SocketGeneration: socket.Generation}
	if socket.EffectivePID > 0 && socket.EffectivePID != socket.PID {
		result.Delegated, result.EffectivePID = true, socket.EffectivePID
		_, app, err := r.process(socket.EffectivePID)
		// Never substitute the helper's app when the delegated process exits or
		// its path is inaccessible: that would match the wrong app's rules.
		result.App = app
		if err != nil {
			return result, fmt.Errorf("resolve effective process: %w", err)
		}
	} else if ownerErr != nil {
		return result, ownerErr
	}
	return result, ctx.Err()
}

func (r *Resolver) process(pid int) (api.Process, api.AppRef, error) {
	if pid <= 0 {
		return api.Process{}, api.AppRef{}, ErrNotFound
	}
	id, err := r.identify(pid)
	if err != nil {
		return api.Process{}, api.AppRef{}, err
	}
	if cached, ok := r.processes[pid]; ok && cached.identity == id && time.Now().Before(cached.until) {
		return cached.process, cached.app, nil
	}
	path, err := r.path(pid)
	if err != nil {
		return api.Process{}, api.AppRef{}, err
	}
	after, err := r.identify(pid)
	if err != nil {
		return api.Process{}, api.AppRef{}, err
	}
	if after != id {
		return api.Process{}, api.AppRef{}, errors.New("process changed during attribution")
	}
	process := api.Process{PID: pid, Path: path, Name: filepath.Base(path)}
	app, err := AppForPath(path)
	if err != nil {
		return process, api.AppRef{}, err
	}
	if len(r.processes) >= 4096 {
		clear(r.processes)
	}
	r.processes[pid] = processEntry{identity: id, process: process, app: app, until: time.Now().Add(time.Second)}
	return process, app, nil
}

// AppForPath uses the outermost bundle, so nested helpers and XPC services
// inherit the containing app's identifier, not their own nested Info.plist.
func AppForPath(path string) (api.AppRef, error) {
	if !filepath.IsAbs(path) {
		return api.AppRef{}, fmt.Errorf("process path is not absolute")
	}
	path = filepath.Clean(path)
	bundle := rules.OuterBundle(path)
	if bundle == "" {
		return api.AppRef{Path: path, Name: filepath.Base(path)}, nil
	}
	app := api.AppRef{Path: bundle, Name: strings.TrimSuffix(filepath.Base(bundle), filepath.Ext(bundle))}
	// Bundle metadata is writable by the app's user. A FIFO must not block the
	// resolver (and tunnel shutdown) while opening or reading it. Follow ordinary
	// symlinks, but inspect the opened descriptor before accepting any contents.
	f, err := os.OpenFile(filepath.Join(bundle, "Contents", "Info.plist"), os.O_RDONLY|syscall.O_NONBLOCK, 0)
	if errors.Is(err, os.ErrNotExist) || errors.Is(err, os.ErrPermission) {
		return app, nil
	}
	if err != nil {
		return app, err
	}
	defer f.Close()
	stat, err := f.Stat()
	if err != nil {
		return app, err
	}
	if !stat.Mode().IsRegular() {
		return app, nil
	}
	data, err := io.ReadAll(io.LimitReader(f, 1<<20+1))
	if err != nil {
		return app, err
	}
	if len(data) > 1<<20 {
		return app, fmt.Errorf("app Info.plist exceeds 1 MiB")
	}
	var info struct {
		BundleID    string `plist:"CFBundleIdentifier"`
		DisplayName string `plist:"CFBundleDisplayName"`
		Name        string `plist:"CFBundleName"`
	}
	if _, err := plist.Unmarshal(data, &info); err != nil {
		return app, fmt.Errorf("read app Info.plist: %w", err)
	}
	app.BundleID = info.BundleID
	if info.DisplayName != "" {
		app.Name = info.DisplayName
	} else if info.Name != "" {
		app.Name = info.Name
	}
	return app, nil
}
