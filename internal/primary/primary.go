// Package primary keeps an interface from being macOS's primary for a while,
// so that macOS routes through another meanwhile: the helper does so while
// the router behind the interface renews its public address.
//
// configd's IPMonitor elects the primary service and ranks each service by the
// PrimaryRank of its Setup: and State: entities, "Never" making it ineligible
// (get_rank_changes in configd's Plugins/IPMonitor/ip_plugin.c). The service
// keeps its addresses and scoped routes, so sockets bound to its interface
// still work. Pith adds the State: entity as a temporary key, with scutil's
// `add key temporary`, which calls SCDynamicStoreAddTemporaryValue: configd
// removes it when the session that added it closes (configd.tproj's
// _configclose.c), and the session closes when scutil exits, as it does at the
// end of its input. So the rank comes back however the helper ends, even
// killed: nothing is written to the system's preferences.
package primary

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"
)

// session is an interactive scutil: show prints a key's dictionary, or
// those of the keys a pattern matches; the other commands print nothing but
// their errors. Its done channel closes when the session ends.
type session interface {
	show(ctx context.Context, key string, pattern bool) (dict, error)
	send(commands ...string) error
	close() error
	done() <-chan struct{}
}

// dict is a dictionary scutil printed: strings, and nested dictionaries for
// dictionaries and arrays. A nil dict is a key that doesn't exist.
type dict map[string]any

func (d dict) dict(key string) dict  { v, _ := d[key].(dict); return v }
func (d dict) str(key string) string { v, _ := d[key].(string); return v }

// keys are those Yield reads to find the services and the primary.
const keys = `^(Setup|State):/Network/(Service/[^/]+(/(Interface|IPv4|IPv6))?|Global/IPv[46])$`

// Tests replace these.
var (
	newSession = startScutil
	// poll is how often Yield reads the primary while macOS elects another,
	// and switchTime bounds how long that takes.
	poll       = 100 * time.Millisecond
	switchTime = 10 * time.Second
)

// Yield makes the network service of iface ineligible to be macOS's primary
// until ctx ends. Once another interface is primary it calls ready with that
// interface's name, then waits for ctx, and gives the service its rank back
// before it returns. An error before ready means nothing changed or the
// change was undone; an error after it, that the session ended early.
func Yield(ctx context.Context, iface string, ready func(primary string)) (err error) {
	if !validName(iface) {
		return fmt.Errorf("primary: invalid interface name %q", iface)
	}
	s, err := newSession(ctx)
	if err != nil {
		return err
	}
	defer func() {
		if closeErr := s.close(); closeErr != nil {
			err = errors.Join(err, closeErr)
		}
	}()
	store, err := s.show(ctx, keys, true)
	if err != nil {
		return err
	}
	service, err := yieldable(store, iface)
	if err != nil {
		return err
	}
	key := "State:/Network/Service/" + service
	if err := s.send("d.init", "d.add PrimaryRank Never", "add "+key+" temporary"); err != nil {
		return err
	}
	added, err := s.show(ctx, key, false)
	if err != nil {
		return err
	}
	if added.str("PrimaryRank") != "Never" {
		return fmt.Errorf("primary: configd did not take the rank of %s's service", iface)
	}
	deadline := time.Now().Add(switchTime)
	for {
		global, err := s.show(ctx, "State:/Network/Global/IPv4", false)
		if err != nil {
			return err
		}
		if p := global.str("PrimaryInterface"); p != "" && p != iface {
			ready(p)
			break
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("primary: macOS kept %s primary", iface)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-s.done():
			return errors.New("primary: scutil ended")
		case <-time.After(poll):
		}
	}
	select {
	case <-ctx.Done():
		return nil
	case <-s.done():
		return errors.New("primary: scutil ended, and the interface could be primary again")
	}
}

// yieldable finds the network service of iface, and checks that another
// service could be primary and that the State: entity Pith adds is not there:
// one that is belongs to someone else.
func yieldable(store dict, iface string) (string, error) {
	service, other := "", false
	for key := range store {
		id, ok := strings.CutPrefix(key, "State:/Network/Service/")
		if !ok {
			continue
		}
		id, entity, ok := strings.Cut(id, "/")
		if !ok || entity != "IPv4" && entity != "IPv6" {
			continue
		}
		v := store.dict(key)
		switch name := v.str("InterfaceName"); {
		case name == iface:
			service = id
		case name != "" && v.str("Router") != "":
			other = true
		}
	}
	switch {
	case service == "":
		return "", fmt.Errorf("primary: %s has no active network service", iface)
	case !validService(service):
		return "", fmt.Errorf("primary: unexpected service ID %q", service)
	case !other:
		return "", fmt.Errorf("primary: no other interface has a router to be primary")
	case store["State:/Network/Service/"+service] != nil:
		return "", fmt.Errorf("primary: the state of %s's service is set already", iface)
	}
	return service, nil
}

// validService is a service ID as System Settings makes them, a UUID, so it
// can go in a scutil command.
func validService(id string) bool {
	if id == "" || len(id) > 64 {
		return false
	}
	for _, c := range id {
		if !(c >= 'A' && c <= 'Z' || c >= 'a' && c <= 'z' || c >= '0' && c <= '9' || c == '-') {
			return false
		}
	}
	return true
}

// validName is a BSD interface name, so it can go in a scutil command.
func validName(name string) bool {
	if name == "" || len(name) > 15 {
		return false
	}
	for _, c := range name {
		if !(c >= 'a' && c <= 'z' || c >= '0' && c <= '9') {
			return false
		}
	}
	return true
}
