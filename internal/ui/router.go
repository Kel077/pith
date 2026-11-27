package ui

import (
	"context"
	"sync/atomic"
	"time"

	"github.com/Kel077/pith/internal/api"
)

// routers is what the window knows of the routers behind the interfaces.
type routers struct {
	info     map[string]api.Router // by interface
	none     map[string]bool       // interfaces that lead to no router Pith knows
	renewing map[string]bool
	renewals map[string]api.RouterRenewal
	problems map[string]string
	openWrt  openWrtDialog
	saved    []api.OpenWrtRouter

	targets atomic.Pointer[[]routerPollTarget]
}

type routerPollTarget struct {
	link    api.Interface
	openWrt api.OpenWrtRouter
}

func newRouters() routers {
	return routers{info: map[string]api.Router{}, none: map[string]bool{}, renewing: map[string]bool{},
		renewals: map[string]api.RouterRenewal{}, problems: map[string]string{}}
}

// readRouters reads them at once, for tests.
func (a *App) readRouters(ctx context.Context) {
	if targets := a.router.targets.Load(); targets != nil {
		for _, target := range *targets {
			r, err := a.Service.Router(ctx, target.link.Name)
			a.setRouter(target, r, err)
		}
	}
}

func (a *App) setRouter(target routerPollTarget, r api.Router, err error) {
	name := target.link.Name
	if !sameGatewayLink(target.link, a.ifaceByName(name)) || target.openWrt != a.openWrtConfig(name) || a.openWrtPending(name) {
		return
	}
	if target.openWrt.Host != "" {
		if err != nil {
			delete(a.router.info, name)
			a.router.problems[name] = err.Error()
		} else if r.OpenWrt != nil {
			a.router.info[name] = r
			delete(a.router.problems, name)
		}
		return
	}
	switch {
	case api.Code(err) == api.ErrUnsupported:
		a.router.none[name] = true
		delete(a.router.info, name)
		a.routerNames()
	case err == nil:
		a.router.info[name] = r
	}
	// Otherwise keep what was read last.
}

// routerNames tells the poller which interfaces may lead to routers: the
// connected Ethernet and Wi-Fi ones, but those found to lead to none and those
// whose router renews its address, which answers late meanwhile.
func (a *App) routerNames() {
	var targets []routerPollTarget
	for _, i := range a.ifaces {
		cfg := a.openWrtConfig(i.Name)
		switch {
		case !i.Up:
			delete(a.router.info, i.Name)
		case canConfigureOpenWrt(&i) && !a.openWrtPending(i.Name) &&
			(cfg.Host != "" || !a.router.none[i.Name]) && !a.router.renewing[i.Name]:
			targets = append(targets, routerPollTarget{link: i, openWrt: cfg})
		}
	}
	a.router.targets.Store(&targets)
}

// renewRouter has the router behind an interface give its public IPv4
// address up and ask for another, while another interface carries the Mac.
// A router that refuses Pith's password asks for it again.
func (a *App) renewRouter(name string) {
	if a.router.renewing[name] {
		return
	}
	a.router.renewing[name] = true
	cfg := a.openWrtConfig(name)
	a.routerNames()
	delete(a.router.renewals, name)
	delete(a.speeds, name) // of its old address
	var r api.RouterRenewal
	locked := false
	ended := func() {
		delete(a.router.renewing, name)
		a.routerNames()
		select { // read its new state
		case a.hotspot.kick <- struct{}{}:
		default:
		}
	}
	a.async(func(ctx context.Context) (err error) {
		// Three tries, the last a minute without an address, and as long as
		// two minutes more for a router slow to connect again.
		ctx, cancel := context.WithTimeout(ctx, 8*time.Minute)
		defer cancel()
		r, err = a.Service.RenewRouter(ctx, name)
		if locked = api.Code(err) == api.ErrWrongPassword; locked {
			return nil // said in the password sheet, not in a toast
		}
		return err
	}, func() {
		ended()
		if locked {
			a.hotspot.unlock = unlockDialog{open: true, iface: name, router: true}
			return
		}
		if a.openWrtConfig(name) == cfg {
			a.router.renewals[name] = r
		}
	}, func(err error) {
		ended()
		switch api.Code(err) {
		case api.ErrHelperMissing, api.ErrHelperOutdated:
			a.promptInstall("", api.Code(err) == api.ErrHelperOutdated)
		}
	})
}
