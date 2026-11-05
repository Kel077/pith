// Package ui is Pith's interface: the menu bar panel and the main window,
// drawn with MyGo's native UI. It knows the backend only as an api.Service
// and the operating system only as a Desktop, so that it runs on the mock
// backend and in tests.
package ui

import (
	"context"
	"slices"
	"sync/atomic"
	"time"

	"github.com/egoist/mygo/ui"
	"github.com/Kel077/pith/internal/api"
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
	PreferredLanguages() []string
	Quit()
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
	status    api.Status
	ifaces    []api.Interface
	conns     []*api.Connection // newest first
	byID      map[uint64]*api.Connection
	traffic   api.Traffic
	probes    map[string]api.Probe
	probing   map[string]bool
	apps      []api.AppRef
	appsAsk   bool
	icons     map[string]*appIcon
	colors    map[string]ui.Color // of the interfaces, by key and BSD name
	colorsFor int                 // how many interfaces colors was made for
	panelH    int                 // the height the panel's content took

	routing   atomic.Bool // routing runs: read off the main thread
	busy      string      // the action running: "start", "stop", "install", "uninstall"
	toasts    []string    // messages for the next frame
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
		icons:   map[string]*appIcon{},
		page:    "rules",
	}
}

// Run loads the configuration and follows the backend until ctx ends.
func (a *App) Run(ctx context.Context) {
	a.ctx = ctx
	go func() {
		cfg, err := a.Service.LoadConfig(ctx)
		a.Post(func() {
			if err != nil {
				a.toast(errorText(err))
				return
			}
			a.setConfig(cfg)
		})
		conns, _ := a.Service.Connections(ctx, api.ConnectionQuery{Limit: 500})
		a.Post(func() { a.setConnections(conns) })
		a.follow(ctx)
	}()
	go a.pollTraffic(ctx)
}

// Sync loads everything at once, without goroutines, for tests.
func (a *App) Sync(ctx context.Context) {
	a.ctx = ctx
	a.syncIcons = true
	if cfg, err := a.Service.LoadConfig(ctx); err == nil {
		a.setConfig(cfg)
	}
	if st, err := a.Service.Status(ctx); err == nil {
		a.status = st
		a.routing.Store(st.State == api.Running)
	}
	if ifs, err := a.Service.Interfaces(ctx); err == nil {
		a.ifaces = ifs
	}
	if conns, err := a.Service.Connections(ctx, api.ConnectionQuery{Limit: 500}); err == nil {
		a.setConnections(conns)
	}
	if t, err := a.Service.Traffic(ctx, api.TrafficQuery{Since: a.Now().Add(-2 * time.Minute), Step: time.Second}); err == nil {
		a.traffic = t
	}
	if apps, err := a.Desktop.InstalledApps(); err == nil {
		a.apps = apps
	}
}

// follow applies the backend's events, a batch per frame, and subscribes
// again when the stream ends, as when the helper restarts.
func (a *App) follow(ctx context.Context) {
	for ctx.Err() == nil {
		ch, err := a.Service.Subscribe(ctx)
		if err != nil {
			st, serr := a.Service.Status(ctx)
			a.Post(func() {
				if serr == nil {
					a.status = st
				}
			})
			wait(ctx, 5*time.Second)
			continue
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
		tr, err := a.Service.Traffic(ctx, api.TrafficQuery{Since: a.Now().Add(-2 * time.Minute), Step: time.Second})
		if err == nil {
			a.Post(func() { a.traffic = tr })
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
		a.status = *ev.Status
		a.routing.Store(a.status.State == api.Running)
		a.updateTray()
	case ev.Interfaces != nil:
		a.ifaces = ev.Interfaces
		a.colors = nil
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
	a.loaded = true
	if a.scene(a.sceneID) == nil {
		a.sceneID = cfg.Selected
	}
	lang = resolveLanguage(cfg.Settings.Language, a.Desktop.PreferredLanguages())
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
		return tr("Default", "默认")
	}
	return s.Name
}

// change changes the configuration and saves it; the backend applies the
// rules of the running scene at once.
func (a *App) change(fn func(cfg *api.Config)) {
	fn(&a.cfg)
	cfg := cloneConfig(a.cfg)
	a.async(func(ctx context.Context) error { return a.Service.SaveConfig(ctx, cfg) }, nil, func(error) {
		// The backend refused it: show what it has.
		if saved, err := a.Service.LoadConfig(a.ctx); err == nil {
			a.Post(func() { a.setConfig(saved) })
		}
	})
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
		a.install = installPrompt{open: true, then: sceneID, update: a.status.Helper.Outdated}
		return
	}
	if a.cfg.Selected != sceneID {
		a.change(func(cfg *api.Config) { cfg.Selected = sceneID })
	}
	a.busy = "start"
	a.async(func(ctx context.Context) error { return a.Service.Start(ctx, sceneID) },
		func() { a.busy = "" },
		func(err error) {
			a.busy = ""
			switch api.Code(err) {
			case api.ErrHelperMissing, api.ErrHelperOutdated:
				a.install = installPrompt{open: true, then: sceneID, update: api.Code(err) == api.ErrHelperOutdated}
			}
		})
}

func (a *App) stop() {
	if a.busy != "" {
		return
	}
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
			a.status.Helper = api.HelperStatus{}
			a.status.State = api.Stopped
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

// iface returns the interface a rule names, by its hardware address first.
func (a *App) iface(ref api.InterfaceRef) *api.Interface {
	if ref.HardwareAddr != "" {
		for i := range a.ifaces {
			if a.ifaces[i].HardwareAddr == ref.HardwareAddr {
				return &a.ifaces[i]
			}
		}
	}
	for i := range a.ifaces {
		if a.ifaces[i].Name == ref.Name {
			return &a.ifaces[i]
		}
	}
	return nil
}

// ifaceByName returns the interface with a BSD name, or nil.
func (a *App) ifaceByName(name string) *api.Interface {
	return a.iface(api.InterfaceRef{Name: name})
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

// ifaceKey is the key of the interface with a BSD name, for its color.
func (a *App) ifaceKey(name string) string {
	if i := a.ifaceByName(name); i != nil {
		return refOf(i).Key()
	}
	return name
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
		msg = tr("The helper is not installed.", "后台助手还没有安装。")
	case api.ErrHelperOutdated:
		msg = tr("The helper needs an update.", "后台助手需要更新。")
	case api.ErrCanceled:
		msg = tr("Canceled.", "已取消。")
	case api.ErrInvalidConfig:
		msg = tr("These settings can't be saved.", "这些设置无法保存。")
	case api.ErrNoScene:
		msg = tr("That scene no longer exists.", "这个场景已经不存在了。")
	case api.ErrRouteConflict:
		msg = tr("Another VPN or network tool holds the routes Pith needs.", "另一个 VPN 或网络工具占用了橘核需要的路由。")
	case api.ErrNetworkLost:
		msg = tr("Routing cut the internet, so Pith undid it.", "启动分流后网络断开了，橘核已自动撤销。")
	default:
		msg = tr("Something went wrong.", "出了点问题。")
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
