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

	names atomic.Pointer[[]string] // the interfaces to read, for the poller
}

func newRouters() routers {
	return routers{info: map[string]api.Router{}, none: map[string]bool{}, renewing: map[string]bool{},
		renewals: map[string]api.RouterRenewal{}}
}

// readRouters reads them at once, for tests.
func (a *App) readRouters(ctx context.Context) {
	if names := a.router.names.Load(); names != nil {
		for _, name := range *names {
			r, err := a.Service.Router(ctx, name)
			a.setRouter(name, r, err)
		}
	}
}

func (a *App) setRouter(name string, r api.Router, err error) {
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
	var names []string
	for _, i := range a.ifaces {
		switch {
		case !i.Up:
			delete(a.router.info, i.Name)
		case (i.Kind == api.KindEthernet || i.Kind == api.KindWiFi) && !a.router.none[i.Name] && !a.router.renewing[i.Name]:
			names = append(names, i.Name)
		}
	}
	a.router.names.Store(&names)
}

// renewRouter has the router behind an interface give its public IPv4
// address up and ask for another, while another interface carries the Mac.
// A router that refuses Pith's password asks for it again.
func (a *App) renewRouter(name string) {
	if a.router.renewing[name] {
		return
	}
	a.router.renewing[name] = true
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
		a.router.renewals[name] = r
	}, func(err error) {
		ended()
		switch api.Code(err) {
		case api.ErrHelperMissing, api.ErrHelperOutdated:
			a.promptInstall("", api.Code(err) == api.ErrHelperOutdated)
		}
	})
}
