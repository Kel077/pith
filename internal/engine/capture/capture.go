package capture

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/netip"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/Kel077/pith/internal/api"
	"golang.zx2c4.com/wireguard/tun"
)

const MTU = 1500

// Driver separates planning/rollback tests from every privileged system call.
type Driver interface {
	Snapshot() ([]Route, error)
	Create(int) (tun.Device, error)
	Index(string) (int, error)
	Configure(context.Context, string, bool) error
	Add(context.Context, netip.Prefix, string) error
	Delete(context.Context, netip.Prefix, string) error
}

type Options struct {
	IPv6      bool
	Scopes    []netip.Prefix
	DNS       []netip.Addr
	StatePath string
	Driver    Driver // nil: the native macOS implementation
}

type journal struct {
	Version   int       `json:"version"`
	Token     string    `json:"token"`
	PID       int       `json:"pid"`
	Created   time.Time `json:"created"`
	Interface string    `json:"interface"`
	Index     int       `json:"index"`
	Plan      Plan      `json:"plan"`
}

type Capture struct {
	lifecycle sync.Mutex
	Device    tun.Device
	Name      string
	Plan      Plan
	Faults    <-chan error
	opts      Options
	state     journal
	active    atomic.Bool
	closed    atomic.Bool
	drained   chan struct{}
	closeOnce sync.Once
	closeErr  error
}

// Prepare snapshots routing, rejects conflicts, creates/configures one utun and
// records its exact intent. It does not install capture routes: the packet stack
// must be ready before Activate. Every failure closes the utun it created.
func Prepare(ctx context.Context, opts Options) (*Capture, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if opts.Driver == nil {
		opts.Driver = nativeDriver{}
	}
	if !filepath.IsAbs(opts.StatePath) {
		return nil, fmt.Errorf("capture state path must be absolute")
	}
	if err := Recover(opts.StatePath, opts.Driver); err != nil {
		return nil, err
	}
	before, err := opts.Driver.Snapshot()
	if err != nil {
		return nil, err
	}
	for _, r := range before {
		if r.Local && (r.Prefix.Contains(netip.MustParseAddr("198.18.0.1")) || opts.IPv6 && r.Prefix.Contains(netip.MustParseAddr("fd7a:7069:7468::1"))) {
			return nil, &api.Error{Code: api.ErrRouteConflict, Detail: "Pith tunnel address is already in use"}
		}
	}
	plan, err := Build(before, opts.DNS, opts.IPv6, opts.Scopes)
	if err != nil {
		return nil, err
	}
	device, err := opts.Driver.Create(MTU)
	if err != nil {
		return nil, err
	}
	name, err := device.Name()
	if err != nil || !validName(name) {
		device.Close()
		return nil, fmt.Errorf("invalid utun name %q: %v", name, err)
	}
	index, err := opts.Driver.Index(name)
	if err != nil || index <= 0 {
		device.Close()
		return nil, fmt.Errorf("read utun index: %v", err)
	}
	faults := make(chan error, 1)
	c := &Capture{Device: device, Name: name, Plan: plan, Faults: faults, opts: opts, drained: make(chan struct{})}
	c.state = journal{Version: 1, Token: rand.Text(), PID: os.Getpid(), Created: time.Now(), Interface: name, Index: index, Plan: plan}
	go func() {
		defer close(c.drained)
		defer close(faults)
		for event := range device.Events() {
			if event&tun.EventDown != 0 && c.active.Load() && !c.closed.Load() {
				select {
				case faults <- &api.Error{Code: api.ErrNetworkLost, Detail: "Pith tunnel went down"}:
				default:
				}
			}
		}
	}()
	if err := c.writeState(); err != nil {
		return nil, errors.Join(err, c.Close())
	}
	if err := opts.Driver.Configure(ctx, name, opts.IPv6); err != nil {
		return nil, errors.Join(err, c.Close())
	}
	return c, nil
}

func validName(name string) bool {
	if !strings.HasPrefix(name, "utun") || len(name) < 5 {
		return false
	}
	for _, v := range name[4:] {
		if v < '0' || v > '9' {
			return false
		}
	}
	return true
}

// Activate checks for routes added since Prepare and installs the plan without
// replacing any existing route. A partial failure rolls back the whole capture.
func (c *Capture) Activate(ctx context.Context) error {
	c.lifecycle.Lock()
	defer c.lifecycle.Unlock()
	if c.closed.Load() {
		return fmt.Errorf("capture is closed")
	}
	if c.active.Load() {
		return nil
	}
	current, err := c.opts.Driver.Snapshot()
	if err != nil {
		return errors.Join(err, c.closeLocked())
	}
	for _, prefix := range c.Plan.Prefixes {
		for _, r := range current {
			if !r.Scoped && r.Prefix == prefix {
				return errors.Join(&api.Error{Code: api.ErrRouteConflict, Detail: prefix.String() + " via " + r.Interface}, c.closeLocked())
			}
		}
	}
	for _, prefix := range c.Plan.Prefixes {
		if err := ctx.Err(); err != nil {
			return errors.Join(err, c.closeLocked())
		}
		if err := c.opts.Driver.Add(ctx, prefix, c.Name); err != nil {
			return errors.Join(err, c.closeLocked())
		}
	}
	c.active.Store(true)
	return nil
}

// Close removes only matching routes through this utun, then closes its control
// fd even when explicit deletion fails. Closing the utun also removes its routes
// in the kernel, including on a helper crash. It never edits default routes.
func (c *Capture) Close() error {
	c.lifecycle.Lock()
	defer c.lifecycle.Unlock()
	return c.closeLocked()
}

func (c *Capture) closeLocked() error {
	c.closeOnce.Do(func() {
		c.closed.Store(true)
		c.active.Store(false)
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		current, err := c.opts.Driver.Snapshot()
		var failures []error
		if err != nil {
			failures = append(failures, err)
		} else {
			for i := len(c.Plan.Prefixes) - 1; i >= 0; i-- {
				prefix := c.Plan.Prefixes[i]
				if slices.ContainsFunc(current, func(r Route) bool {
					return !r.Scoped && r.Interface == c.Name && r.Index == c.state.Index && r.Prefix == prefix
				}) {
					if err := c.opts.Driver.Delete(ctx, prefix, c.Name); err != nil {
						failures = append(failures, err)
					}
				}
			}
		}
		if err := c.Device.Close(); err != nil {
			failures = append(failures, err)
		}
		<-c.drained
		// A failed kernel/device close leaves the journal for explicit recovery.
		after, err := c.opts.Driver.Snapshot()
		if err != nil {
			failures = append(failures, err)
		} else if ownsRoutes(c.state, after) {
			failures = append(failures, fmt.Errorf("Pith capture routes remain after closing %s", c.Name))
		} else if state, err := readState(c.opts.StatePath); err == nil && state.Token == c.state.Token {
			if err := os.Remove(c.opts.StatePath); err != nil {
				failures = append(failures, err)
			}
		} else if err != nil && !errors.Is(err, os.ErrNotExist) {
			failures = append(failures, err)
		}
		c.closeErr = errors.Join(failures...)
	})
	return c.closeErr
}

func ownsRoutes(state journal, routes []Route) bool {
	return slices.ContainsFunc(routes, func(r Route) bool {
		return !r.Scoped && r.Interface == state.Interface && r.Index == state.Index && slices.Contains(state.Plan.Prefixes, r.Prefix)
	})
}

// Recover discards only stale metadata after confirming that no planned route
// remains on the recorded utun. It never guesses ownership of a reused utun name
// or deletes another VPN's routes. A live-looking journal requires review.
func Recover(path string, driver Driver) error {
	state, err := readState(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	if driver == nil {
		driver = nativeDriver{}
	}
	routes, err := driver.Snapshot()
	if err != nil {
		return err
	}
	if ownsRoutes(state, routes) {
		return &api.Error{Code: api.ErrRouteConflict, Detail: "capture journal still has routes on " + state.Interface}
	}
	return os.Remove(path)
}

// CleanupStale is for explicit emergency recovery after launchd bootout. It
// refuses to touch a live owner and deletes only journaled routes with the same
// utun name AND index. Normal startup uses the more conservative Recover.
func CleanupStale(ctx context.Context, path string, driver Driver) error {
	return cleanupStale(ctx, path, driver, func(pid int) bool {
		err := syscall.Kill(pid, 0)
		return !errors.Is(err, syscall.ESRCH)
	})
}

func cleanupStale(ctx context.Context, path string, driver Driver, alive func(int) bool) error {
	state, err := readState(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	if alive(state.PID) {
		return fmt.Errorf("capture owner %d is still running; stop it before recovery", state.PID)
	}
	if driver == nil {
		driver = nativeDriver{}
	}
	routes, err := driver.Snapshot()
	if err != nil {
		return err
	}
	for i := len(state.Plan.Prefixes) - 1; i >= 0; i-- {
		prefix := state.Plan.Prefixes[i]
		if slices.ContainsFunc(routes, func(r Route) bool {
			return !r.Scoped && r.Interface == state.Interface && r.Index == state.Index && r.Prefix == prefix
		}) {
			if err := driver.Delete(ctx, prefix, state.Interface); err != nil {
				return err
			}
		}
	}
	routes, err = driver.Snapshot()
	if err != nil {
		return err
	}
	if ownsRoutes(state, routes) {
		return errors.New("journaled capture routes remain after recovery")
	}
	current, err := readState(path)
	if err != nil {
		return err
	}
	if current.Token != state.Token {
		return errors.New("capture owner changed during recovery")
	}
	return os.Remove(path)
}

func readState(path string) (journal, error) {
	f, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	if err != nil {
		return journal{}, err
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return journal{}, err
	}
	if !info.Mode().IsRegular() || info.Mode().Perm()&0077 != 0 {
		return journal{}, fmt.Errorf("capture journal must be a private regular file")
	}
	if stat, ok := info.Sys().(*syscall.Stat_t); !ok || stat.Uid != uint32(os.Geteuid()) {
		return journal{}, fmt.Errorf("capture journal has an unexpected owner")
	}
	var state journal
	d := json.NewDecoder(io.LimitReader(f, 1<<20))
	if err := d.Decode(&state); err != nil {
		return journal{}, err
	}
	if state.Version != 1 || state.Token == "" || !validName(state.Interface) || state.Index <= 0 || state.PID <= 0 {
		return journal{}, fmt.Errorf("invalid capture journal")
	}
	for _, prefix := range state.Plan.Prefixes {
		if !prefix.IsValid() || prefix.Bits() == 0 {
			return journal{}, fmt.Errorf("invalid capture prefix in journal")
		}
	}
	return state, nil
}

func (c *Capture) writeState() error {
	f, err := os.CreateTemp(filepath.Dir(c.opts.StatePath), ".pith-capture-*")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	if err := json.NewEncoder(f).Encode(c.state); err != nil {
		f.Close()
		return err
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	if err := os.Rename(f.Name(), c.opts.StatePath); err != nil {
		return err
	}
	dir, err := os.Open(filepath.Dir(c.opts.StatePath))
	if err != nil {
		return err
	}
	defer dir.Close()
	return dir.Sync()
}
