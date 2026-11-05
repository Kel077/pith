// Package ui is Pith's interface: the menu bar panel and the main window,
// drawn with MyGo's native UI. It knows the backend only as an api.Service
// and the operating system only as a Desktop, so that it runs on the mock
// backend and in tests.
package ui

import (
	"cmp"
	"context"
	"slices"
	"sync"
	"sync/atomic"
	"time"

	"github.com/egoist/mygo/ui"

	"github.com/Kel077/pith/internal/api"
	"github.com/Kel077/pith/locales"
)

// Desktop is what the interface asks of the operating system.
type Desktop interface {
	// InstalledApps lists the apps installed, sorted by name.
	InstalledApps() ([]api.AppRef, error)
	// Icon returns the icon Finder shows for a file, as a PNG of size×size.
	Icon(path string, size int) ([]byte, error)
	// ChooseProgram asks for an app or a command-line tool: "" if canceled.
	ChooseProgram() (string, error)
	// Save asks where to save data, suggesting name; canceling is no error.
	Save(name string, data []byte) error
	// Open asks for a file to read: nil, nil if canceled.
	Open() ([]byte, error)
	OpenURL(url string)
	Reveal(path string)
	SetOpenAtLogin(on bool) error
	// SetLanguage shows the app in a language of its catalogs ("en",
	// "zh-Hans"), or the system's for "".
	SetLanguage(tag string)
	Quit()
	// ReadState and WriteState keep a little state of the interface's
	// between launches, by name.
	ReadState(name string) string
	WriteState(name, value string)
}

// Options configure an App.
type Options struct {
	Service api.Service
	Desktop Desktop
	// Post runs fn on the main thread and draws every window again.
	Post    func(fn func())
	Version string
	// Now is the clock; nil for time.Now.
	Now func() time.Time
	// Tray, when set, shows the menu bar icon's state: whether routing
	// runs, and the text beside the icon.
	Tray func(running bool, title string)
	// OpenWindow shows the main window, and ClosePanel hides the menu bar
	// panel.
	OpenWindow, ClosePanel func()
	// ResizePanel gives the menu bar panel the height its content needs.
	ResizePanel func(height int)
	// Page is the page the window opens on, and Language the language of
	// the interface instead of the setting's: for screenshots.
	Page, Language string
}

// maxConns is how many connections the activity keeps.
const maxConns = 2000

// App is the interface's state. All of it is read and changed on the main
// thread: by the views, their callbacks, and the functions given to post.
type App struct {
	Options
	ctx context.Context

	cfg       api.Config
	loaded    bool
	loadErr   error // the configuration could not be read: nothing is saved over it
	status    api.Status
	ifaces    []api.Interface
	conns     []*api.Connection // newest first
	byID      map[uint64]*api.Connection
	traffic   api.Traffic
	trafficAt time.Time // when traffic was read
	probes    map[string]api.Probe
	probing   map[string]bool
	hotspot   hotspots
	apps      []api.AppRef
	appsAsk   bool
	icons     map[string]*appIcon
	colors    map[string]ui.Color // of the interfaces, by key and BSD name
	colorsFor int                 // how many interfaces colors was made for
	panelH    int                 // the height the panel's content took

	langSet   string      // the language last asked of the desktop
	routing   atomic.Bool // routing runs: read off the main thread
	gen       uint64      // changes made to cfg; the saver writes the newest
	saves     saver
	wasOn     bool     // the user left routing on: Pith resumes it on launch if asked
	resumed   bool     // the first status after launch was seen
	busy      string   // the action running: "start", "stop", "install", "uninstall"
	toasts    []string // messages for the next frame
	trayState string

	page     string // "rules", "activity", "network", "settings"
	sceneID  string // the scene the rules page shows
	rules    rulesPage
	activity activityPage
	network  networkPage
	settings settingsPage
	editor   *editor
	checker  *checker
	install  installPrompt
	rename   renameDialog
	confirm  *confirmation
	undo     *undoAction

	syncIcons bool // tests: load icons at once
}

// saver writes the configuration off the main thread, one save at a time,
// always the newest snapshot: older ones that wait are dropped.
type saver struct {
	mu      sync.Mutex
	next    *api.Config
	nextGen uint64
	running bool
}

// appIcon is an app's icon, loading or loaded.
type appIcon struct {
	bmp     *ui.Bitmap
	loading bool
}

// New makes the interface's state. Call Run to load it.
func New(o Options) *App {
	if o.Now == nil {
		o.Now = time.Now
	}
	return &App{
		Options: o,
		ctx:     context.Background(),
		cfg:     api.DefaultConfig(),
		status:  api.Status{State: api.Stopped},
		byID:    map[uint64]*api.Connection{},
		probes:  map[string]api.Probe{},
		probing: map[string]bool{},
		hotspot: newHotspots(),
		icons:   map[string]*appIcon{},
		page:    cmp.Or(o.Page, "rules"),
	}
}

// Run loads the configuration and follows the backend until ctx ends.
func (a *App) Run(ctx context.Context) {
	a.ctx = ctx
	a.wasOn = a.Desktop.ReadState("routing") == "on"
	go func() {
		cfg, err := a.Service.LoadConfig(ctx)
		a.Post(func() {
			if err != nil {
				a.loadErr = err
				return
			}
			a.setConfig(cfg)
		})
		// Interfaces need no helper: rules can be made before it is installed.
		if ifs, err := a.Service.Interfaces(ctx); err == nil {
			a.Post(func() { a.setIfaces(ifs) })
		}
		a.follow(ctx)
	}()
	go a.pollTraffic(ctx)
	go a.pollHotspots(ctx)
}

// Sync loads everything at once, without goroutines, for tests.
func (a *App) Sync(ctx context.Context) {
	a.ctx = ctx
	a.syncIcons = true
	if cfg, err := a.Service.LoadConfig(ctx); err == nil {
		a.setConfig(cfg)
	}
	if st, err := a.Service.Status(ctx); err == nil {
		a.setStatus(st)
	}
	if ifs, err := a.Service.Interfaces(ctx); err == nil {
		a.setIfaces(ifs)
		a.readHotspots(ctx)
	}
	if conns, err := a.Service.Connections(ctx, api.ConnectionQuery{Limit: 500}); err == nil {
		a.setConnections(conns)
	}
	if t, err := a.Service.Traffic(ctx, api.TrafficQuery{Since: a.Now().Add(-2 * time.Minute), Step: time.Second}); err == nil {
		a.traffic, a.trafficAt = t, a.Now()
	}
	if apps, err := a.Desktop.InstalledApps(); err == nil {
		a.apps = apps
	}
}

// follow applies the backend's events, a batch per frame, and subscribes
// again when the stream ends, as when the helper restarts. Without a
// helper, it reads the status and the interfaces every few seconds.
func (a *App) follow(ctx context.Context) {
	for ctx.Err() == nil {
		ch, err := a.Service.Subscribe(ctx)
		if err != nil {
			st, serr := a.Service.Status(ctx)
			ifs, ierr := a.Service.Interfaces(ctx)
			a.Post(func() {
				if serr == nil {
					a.setStatus(st)
				}
				if ierr == nil {
					a.setIfaces(ifs)
				}
			})
			wait(ctx, 5*time.Second)
			continue
		}
		// The log as the helper has it now, before the events that follow:
		// those of a helper that restarted, or started after Pith.
		if conns, err := a.Service.Connections(ctx, api.ConnectionQuery{Limit: 500}); err == nil {
			a.Post(func() { a.setConnections(conns) })
		}
		for ev := range ch {
			batch := []api.Event{ev}
			timer := time.NewTimer(40 * time.Millisecond)
		collect:
			for {
				select {
				case ev, ok := <-ch:
					if !ok {
						break collect
					}
					batch = append(batch, ev)
				case <-timer.C:
					break collect
				}
			}
			timer.Stop()
			a.Post(func() {
				for _, ev := range batch {
					a.apply(ev)
				}
			})
		}
		wait(ctx, time.Second)
	}
}

// pollTraffic reads the traffic of the last two minutes each second while
// routing runs, for the charts.
func (a *App) pollTraffic(ctx context.Context) {
	t := time.NewTicker(time.Second)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		if !a.routing.Load() {
			continue
		}
		at := a.Now()
		tr, err := a.Service.Traffic(ctx, api.TrafficQuery{Since: at.Add(-2 * time.Minute), Step: time.Second})
		if err == nil {
			a.Post(func() { a.traffic, a.trafficAt = tr, at })
		}
	}
}

func contextWithTimeout(d time.Duration) (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.Background(), d)
}

func wait(ctx context.Context, d time.Duration) {
	select {
	case <-ctx.Done():
	case <-time.After(d):
	}
}

// apply applies one event of the backend.
func (a *App) apply(ev api.Event) {
	switch {
	case ev.Status != nil:
		a.setStatus(*ev.Status)
	case ev.Interfaces != nil:
		a.setIfaces(ev.Interfaces)
	case ev.Connection != nil:
		c := *ev.Connection
		if old, ok := a.byID[c.ID]; ok {
			*old = c
			return
		}
		cp := &c
		a.byID[c.ID] = cp
		a.conns = slices.Insert(a.conns, 0, cp)
		if len(a.conns) > maxConns {
			for _, gone := range a.conns[maxConns:] {
				delete(a.byID, gone.ID)
			}
			a.conns = a.conns[:maxConns]
		}
	}
}

// setStatus takes the backend's status: the menu bar icon and the traffic
// poll follow it, and the first one after launch may resume routing.
func (a *App) setStatus(st api.Status) {
	a.status = st
	a.routing.Store(st.State == api.Running)
	a.updateTray()
	a.resume()
}

func (a *App) setIfaces(ifs []api.Interface) {
	a.ifaces = ifs
	a.colors = nil
	// Another device may sit behind an interface now: ask again.
	clear(a.hotspot.none)
	a.hotspotNames()
}

func (a *App) setConnections(conns []api.Connection) {
	a.conns = a.conns[:0]
	clear(a.byID)
	for i := range conns {
		c := conns[i]
		a.conns = append(a.conns, &c)
		a.byID[c.ID] = &c
	}
}

func (a *App) setConfig(cfg api.Config) {
	a.cfg = cfg
	a.loaded, a.loadErr = true, nil
	if a.scene(a.sceneID) == nil {
		a.sceneID = cfg.Selected
	}
	if tag := cmp.Or(a.Language, cfg.Settings.Language); tag != a.langSet {
		a.langSet = tag
		a.Desktop.SetLanguage(tag)
	}
	a.updateTray()
}

// ---- configuration ----------------------------------------------------------

// scene returns the scene with an ID, or nil.
func (a *App) scene(id string) *api.Scene {
	for i := range a.cfg.Scenes {
		if a.cfg.Scenes[i].ID == id {
			return &a.cfg.Scenes[i]
		}
	}
	return nil
}

// sceneName is the name the interface shows for a scene.
func sceneName(s *api.Scene) string {
	if s == nil {
		return ""
	}
	if s.Name == "" {
		return locales.SceneDefaultName()
	}
	return s.Name
}

// change changes the configuration and saves it; the backend applies the
// rules of the running scene at once.
func (a *App) change(fn func(cfg *api.Config)) {
	if !a.loaded {
		return // never save over a configuration that could not be read
	}
	fn(&a.cfg)
	a.gen++
	a.save(a.gen, cloneConfig(a.cfg))
}

// save writes a snapshot of the configuration: in order, one at a time,
// skipping snapshots a newer one replaced before their turn.
func (a *App) save(gen uint64, cfg api.Config) {
	if a.syncIcons { // tests save at once
		if err := a.Service.SaveConfig(a.ctx, cfg); err != nil {
			saved, lerr := a.Service.LoadConfig(a.ctx)
			a.saveFailed(gen, err, saved, lerr)
		}
		return
	}
	s := &a.saves
	s.mu.Lock()
	s.next, s.nextGen = &cfg, gen
	if s.running {
		s.mu.Unlock()
		return
	}
	s.running = true
	s.mu.Unlock()
	go func() {
		for {
			s.mu.Lock()
			next, gen := s.next, s.nextGen
			s.next = nil
			if next == nil {
				s.running = false
				s.mu.Unlock()
				return
			}
			s.mu.Unlock()
			if err := a.Service.SaveConfig(a.ctx, *next); err != nil {
				saved, lerr := a.Service.LoadConfig(a.ctx)
				a.Post(func() { a.saveFailed(gen, err, saved, lerr) })
			}
		}
	}()
}

// saveFailed shows why a save failed and, unless newer changes are on their
// way, what the backend has instead.
func (a *App) saveFailed(gen uint64, err error, saved api.Config, loadErr error) {
	a.toast(errorText(err))
	if gen == a.gen && loadErr == nil {
		a.setConfig(saved)
	}
}

// async runs fn off the main thread; then done, or failed and a toast of
// its error, on the main thread.
func (a *App) async(fn func(ctx context.Context) error, done func(), failed func(error)) {
	if a.syncIcons { // tests run it at once
		if err := fn(a.ctx); err != nil {
			a.toast(errorText(err))
			if failed != nil {
				failed(err)
			}
		} else if done != nil {
			done()
		}
		return
	}
	go func() {
		err := fn(a.ctx)
		a.Post(func() {
			if err != nil {
				if api.Code(err) != api.ErrCanceled {
					a.toast(errorText(err))
				}
				if failed != nil {
					failed(err)
				}
				return
			}
			if done != nil {
				done()
			}
		})
	}()
}

func (a *App) toast(msg string) { a.toasts = append(a.toasts, msg) }

// flushToasts shows the messages left for this frame.
func (a *App) flushToasts(c *ui.Context) {
	for _, m := range a.toasts {
		c.Toast(m)
	}
	a.toasts = a.toasts[:0]
}

// ---- routing ----------------------------------------------------------------

func (a *App) isRunning() bool { return a.status.State == api.Running }

// toggle starts routing with the selected scene, or stops it.
func (a *App) toggle() {
	if a.isRunning() || a.status.State == api.Starting {
		a.stop()
		return
	}
	a.start(a.cfg.Selected)
}

// start routes with a scene, after installing the helper if need be.
func (a *App) start(sceneID string) {
	if a.busy != "" {
		return
	}
	if !a.status.Helper.Installed || a.status.Helper.Outdated {
		a.promptInstall(sceneID, a.status.Helper.Outdated)
		return
	}
	if a.cfg.Selected != sceneID {
		a.change(func(cfg *api.Config) { cfg.Selected = sceneID })
	}
	a.busy = "start"
	a.async(func(ctx context.Context) error { return a.Service.Start(ctx, sceneID) },
		func() {
			a.busy = ""
			a.setWasOn(true)
		},
		func(err error) {
			a.busy = ""
			switch api.Code(err) {
			case api.ErrHelperMissing, api.ErrHelperOutdated:
				a.promptInstall(sceneID, api.Code(err) == api.ErrHelperOutdated)
			}
		})
}

// promptInstall asks to install or update the helper, in the main window.
func (a *App) promptInstall(then string, update bool) {
	a.install = installPrompt{open: true, then: then, update: update}
	if a.OpenWindow != nil {
		a.OpenWindow()
	}
}

// setWasOn records whether the user left routing on, for resuming it.
func (a *App) setWasOn(on bool) {
	if a.wasOn == on {
		return
	}
	a.wasOn = on
	a.Desktop.WriteState("routing", map[bool]string{true: "on", false: "off"}[on])
}

// resume starts routing again as Pith opens, when the user asked for it
// and routing ran when Pith last quit or the Mac shut down.
func (a *App) resume() {
	if a.resumed || !a.loaded {
		return
	}
	a.resumed = true
	s := a.status
	if a.cfg.Settings.ResumeOnLaunch && a.wasOn && s.State == api.Stopped && s.Helper.Running && !s.Helper.Outdated {
		a.start(a.cfg.Selected)
	}
}

func (a *App) stop() {
	if a.busy != "" {
		return
	}
	a.setWasOn(false)
	a.busy = "stop"
	a.async(func(ctx context.Context) error { return a.Service.Stop(ctx) }, func() { a.busy = "" }, func(error) { a.busy = "" })
}

// installHelper installs the helper, then starts the scene waiting for it.
func (a *App) installHelper(then string) {
	a.busy = "install"
	a.async(func(ctx context.Context) error { return a.Service.InstallHelper(ctx) },
		func() {
			a.busy = ""
			a.status.Helper.Installed, a.status.Helper.Outdated = true, false
			if then != "" {
				a.start(then)
			}
		},
		func(error) { a.busy = "" })
}

func (a *App) uninstallHelper() {
	a.busy = "uninstall"
	a.async(func(ctx context.Context) error { return a.Service.UninstallHelper(ctx) },
		func() {
			a.busy = ""
			a.setStatus(api.Status{State: api.Stopped})
		},
		func(error) { a.busy = "" })
}

// updateTray tells the menu bar icon what to show.
func (a *App) updateTray() {
	if a.Tray == nil {
		return
	}
	title := ""
	if a.isRunning() && a.cfg.Settings.MenuBarSpeed {
		title = "↓" + rate(a.status.ReceiveRate)
	}
	state := string(a.status.State) + "|" + title
	if state == a.trayState {
		return
	}
	a.trayState = state
	a.Tray(a.isRunning(), title)
}

// ---- icons and apps ---------------------------------------------------------

// icon returns the icon of an app or a tool, loading it the first time.
func (a *App) icon(path string) *ui.Bitmap {
	if path == "" {
		return nil
	}
	e := a.icons[path]
	if e == nil {
		e = &appIcon{loading: true}
		a.icons[path] = e
		load := func() *ui.Bitmap {
			png, err := a.Desktop.Icon(path, 64)
			if err != nil {
				return nil
			}
			bmp, err := ui.DecodeBitmap(png)
			if err != nil {
				return nil
			}
			return bmp
		}
		if a.syncIcons {
			e.bmp, e.loading = load(), false
		} else {
			go func() {
				bmp := load()
				a.Post(func() { e.bmp, e.loading = bmp, false })
			}()
		}
	}
	return e.bmp
}

// installedApps returns the apps installed, loading them the first time.
func (a *App) installedApps() []api.AppRef {
	if a.apps == nil && !a.appsAsk {
		a.appsAsk = true
		go func() {
			apps, err := a.Desktop.InstalledApps()
			a.Post(func() {
				if err != nil {
					a.toast(errorText(err))
					apps = []api.AppRef{}
				}
				a.apps = apps
			})
		}()
	}
	return a.apps
}

// ---- interfaces -------------------------------------------------------------

// iface returns the interface a rule names, or nil.
func (a *App) iface(ref api.InterfaceRef) *api.Interface { return ref.Resolve(a.ifaces) }

// ifaceByName returns the interface with a BSD name, or nil.
func (a *App) ifaceByName(name string) *api.Interface {
	for i := range a.ifaces {
		if a.ifaces[i].Name == name {
			return &a.ifaces[i]
		}
	}
	return nil
}

// visibleIfaces are the interfaces worth showing: those connected, and
// those System Settings lists as network services. Ports without a service
// (Thunderbolt ports, AirDrop's links) and idle tunnels stay out.
func (a *App) visibleIfaces() []*api.Interface {
	var out []*api.Interface
	for k := range a.ifaces {
		i := &a.ifaces[k]
		if i.Up || i.Label != "" && i.Label != i.Name && i.Kind != api.KindVPN {
			out = append(out, i)
		}
	}
	return out
}

// ifaceLabel is the name the interface shows for an interface: the alias
// the user gave it, its label, or the label saved in the rule.
func (a *App) ifaceLabel(ref api.InterfaceRef) string {
	if alias := a.cfg.Aliases[ref.Key()]; alias != "" {
		return alias
	}
	if i := a.iface(ref); i != nil {
		if alias := a.cfg.Aliases[refOf(i).Key()]; alias != "" {
			return alias
		}
		if i.Label != "" {
			return i.Label
		}
		return i.Name
	}
	if ref.Label != "" {
		return ref.Label
	}
	return ref.Name
}

// refOf is the reference a rule saves to an interface.
func refOf(i *api.Interface) api.InterfaceRef {
	return api.InterfaceRef{Name: i.Name, Label: i.Label, HardwareAddr: i.HardwareAddr}
}

// ---- errors -----------------------------------------------------------------

// errorText explains an error of the backend to the user.
func errorText(err error) string {
	var detail string
	if e, ok := err.(*api.Error); ok {
		detail = e.Detail
	}
	msg := ""
	switch api.Code(err) {
	case api.ErrHelperMissing:
		msg = locales.TheHelperIsNotInstalled()
	case api.ErrHelperOutdated:
		msg = locales.TheHelperNeedsAnUpdate()
	case api.ErrCanceled:
		msg = locales.Canceled()
	case api.ErrInvalidConfig:
		msg = locales.TheseSettingsCanTBeSaved()
	case api.ErrNoScene:
		msg = locales.ThatSceneNoLongerExists()
	case api.ErrRouteConflict:
		msg = locales.AnotherVpnOrNetworkToolHolds()
	case api.ErrNetworkLost:
		msg = locales.RoutingCutTheInternetSoPith()
	case api.ErrUnsupported:
		msg = locales.PithCanTControlThisDevice()
	case api.ErrWrongPassword:
		msg = locales.TheHotspotRefusedItsPassword()
	default:
		msg = locales.SomethingWentWrong()
		if detail == "" {
			detail = err.Error()
		}
	}
	if detail != "" {
		return msg + " (" + detail + ")"
	}
	return msg
}

func cloneConfig(c api.Config) api.Config {
	out := c
	out.Scenes = make([]api.Scene, len(c.Scenes))
	for i, s := range c.Scenes {
		out.Scenes[i] = s
		out.Scenes[i].Rules = make([]api.Rule, len(s.Rules))
		for j, r := range s.Rules {
			out.Scenes[i].Rules[j] = cloneRule(r)
		}
	}
	if c.Aliases != nil {
		out.Aliases = make(map[string]string, len(c.Aliases))
		for k, v := range c.Aliases {
			out.Aliases[k] = v
		}
	}
	return out
}

func cloneRule(r api.Rule) api.Rule {
	r.Apps = slices.Clone(r.Apps)
	r.Targets = slices.Clone(r.Targets)
	r.Via = slices.Clone(r.Via)
	return r
}
