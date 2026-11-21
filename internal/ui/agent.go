package ui

import (
	"context"
	"errors"

	"github.com/Kel077/pith/internal/api"
)

// Agent is the backend as a program that works Pith beside its user has it
// (internal/mcp). What it reads of the configuration and what it changes
// goes through the interface's own copy, on the main thread, so that its
// changes show at once and neither side saves over the other's; starting and
// stopping keep what the interface remembers of routing; the interfaces
// leave out Pith's own tunnel, as the interface does. Everything else is the
// Service's. Its methods are called off the main thread.
type Agent struct {
	api.Service
	app *App
}

// Agent returns the backend for a program that works Pith beside its user.
func (a *App) Agent() *Agent { return &Agent{Service: a.Service, app: a} }

// onMain runs fn on the main thread and returns what it returns, or ctx's
// error when ctx ends first: fn doesn't run after that.
func onMain[T any](ctx context.Context, a *App, fn func() (T, error)) (T, error) {
	type result struct {
		v   T
		err error
	}
	done := make(chan result, 1)
	a.Post(func() {
		if err := ctx.Err(); err != nil {
			done <- result{err: err}
			return
		}
		v, err := fn()
		done <- result{v, err}
	})
	select {
	case r := <-done:
		return r.v, r.err
	case <-ctx.Done():
		var zero T
		return zero, ctx.Err()
	}
}

// unread is why there is no configuration to work on.
func (a *App) unread() error {
	if a.loadErr != nil {
		return a.loadErr
	}
	return errors.New("Pith hasn't read its configuration yet")
}

// LoadConfig returns the configuration as the interface has it, changes on
// their way to the disk included.
func (g *Agent) LoadConfig(ctx context.Context) (api.Config, error) {
	return onMain(ctx, g.app, func() (api.Config, error) {
		if !g.app.loaded {
			return api.Config{}, g.app.unread()
		}
		return cloneConfig(g.app.cfg), nil
	})
}

// Edit changes the configuration as fn has it, shows it, and returns it
// once it is saved. When fn fails, or leaves a configuration that isn't
// valid, nothing changes. When saving fails, the interface goes back to
// what is saved, as after a change of its user's.
func (g *Agent) Edit(ctx context.Context, fn func(cfg *api.Config) error) (api.Config, error) {
	type edit struct {
		cfg   api.Config
		saved <-chan error
	}
	e, err := onMain(ctx, g.app, func() (edit, error) {
		a := g.app
		if !a.loaded {
			return edit{}, a.unread()
		}
		cfg := cloneConfig(a.cfg)
		if err := fn(&cfg); err != nil {
			return edit{}, err
		}
		if err := cfg.Validate(); err != nil {
			return edit{}, err
		}
		a.setConfig(cloneConfig(cfg))
		a.gen++
		return edit{cfg: cfg, saved: a.saveWait(a.gen, cloneConfig(cfg))}, nil
	})
	if err != nil {
		return api.Config{}, err
	}
	select {
	case err := <-e.saved:
		return e.cfg, err
	case <-ctx.Done():
		return api.Config{}, ctx.Err()
	}
}

// SaveConfig replaces the whole configuration, as Edit does.
func (g *Agent) SaveConfig(ctx context.Context, cfg api.Config) error {
	_, err := g.Edit(ctx, func(c *api.Config) error {
		*c = cloneConfig(cfg)
		return nil
	})
	return err
}

// Start routes with a scene and selects it, as the menu bar's switch does:
// the interface remembers that its user left routing on.
func (g *Agent) Start(ctx context.Context, sceneID string) error {
	cfg, err := g.LoadConfig(ctx)
	if err != nil {
		return err
	}
	if cfg.Selected != sceneID {
		if _, err := g.Edit(ctx, func(c *api.Config) error {
			for _, s := range c.Scenes {
				if s.ID == sceneID {
					c.Selected = sceneID
					return nil
				}
			}
			return &api.Error{Code: api.ErrNoScene}
		}); err != nil {
			return err
		}
	}
	if err := g.Service.Start(ctx, sceneID); err != nil {
		return err
	}
	g.app.Post(func() { g.app.setWasOn(true) })
	return nil
}

// Stop stops routing, and the interface no longer resumes it at launch.
func (g *Agent) Stop(ctx context.Context) error {
	g.app.Post(func() { g.app.setWasOn(false) })
	return g.Service.Stop(ctx)
}

// Apps lists the apps installed, sorted by name.
func (g *Agent) Apps(context.Context) ([]api.AppRef, error) { return g.app.Desktop.InstalledApps() }

// Interfaces lists the interfaces but for the tunnel Pith captures through,
// as the interface does: it isn't a network to show or to route through.
func (g *Agent) Interfaces(ctx context.Context) ([]api.Interface, error) {
	ifs, err := g.Service.Interfaces(ctx)
	if err != nil {
		return nil, err
	}
	st, err := onMain(ctx, g.app, func() (api.Status, error) { return g.app.status, nil })
	if err != nil {
		return nil, err
	}
	return withoutOwnTunnel(ifs, st), nil
}
