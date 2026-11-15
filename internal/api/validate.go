package api

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"net/netip"
)

// ErrorCode identifies an error the interface explains to the user.
type ErrorCode string

const (
	ErrHelperMissing        ErrorCode = "helper_missing"        // InstallHelper first
	ErrHelperOutdated       ErrorCode = "helper_outdated"       // InstallHelper updates it
	ErrHelperUntrusted      ErrorCode = "helper_untrusted"      // helper or bundled identity cannot be verified; never ready, stopped or missing
	ErrHelperNotResponding  ErrorCode = "helper_not_responding" // verified installed artifact has no answering helper; engine state is unknown
	ErrCanceled             ErrorCode = "canceled"              // the user canceled macOS's password dialog
	ErrInvalidConfig        ErrorCode = "invalid_config"        // Detail says what
	ErrNoScene              ErrorCode = "no_scene"              // no scene has that ID
	ErrRouteConflict        ErrorCode = "route_conflict"        // another app holds routes Pith needs (Detail: which)
	ErrNetworkLost          ErrorCode = "network_lost"          // SafeStart undid a start that cut the internet
	ErrUnsupported          ErrorCode = "unsupported"           // Pith can't control that device
	ErrWrongPassword        ErrorCode = "wrong_password"        // a device refused the password Pith has, or Pith has none
	ErrPortInUse            ErrorCode = "port_in_use"           // another listener owns the requested local proxy port
	ErrInterfaceUnavailable ErrorCode = "interface_unavailable" // the selected local proxy exit cannot be resolved or used
	ErrDNSUnavailable       ErrorCode = "dns_unavailable"       // the selected local proxy exit has no usable DNS servers
	ErrNoStandby            ErrorCode = "no_standby"            // no other connected interface has internet to carry the Mac while a router renews its address
	ErrBusy                 ErrorCode = "busy"                  // another interface's public address is being changed: one change runs at a time, so the one carrying the Mac while a router renews keeps working
	ErrUpstreamUnreachable  ErrorCode = "upstream_unreachable"  // the proxy a local proxy goes out through doesn't answer
	ErrUpstreamRefused      ErrorCode = "upstream_refused"      // it answers and refuses this Mac: a shared proxy that doesn't list it as a device, or one that wants a password
	ErrNotAllowed           ErrorCode = "not_allowed"           // a proxy's port doesn't let what connects to it change its address: Proxy.ChangeIP is off there
	ErrInternal             ErrorCode = "internal"
)

// Error is an error of the backend with a code the interface translates.
type Error struct {
	Code ErrorCode `json:"code"`
	// Detail is technical: shown small, in English, never translated.
	Detail string `json:"detail,omitempty"`
}

func (e *Error) Error() string {
	if e.Detail == "" {
		return "pith: " + string(e.Code)
	}
	return "pith: " + string(e.Code) + ": " + e.Detail
}

// Code returns the code of err: "" for nil, ErrInternal for an error that
// is not an *Error.
func Code(err error) ErrorCode {
	if err == nil {
		return ""
	}
	var e *Error
	if errors.As(err, &e) {
		return e.Code
	}
	return ErrInternal
}

// Problem is something wrong with one field of a rule.
type Problem struct {
	Field string      `json:"field"` // "apps", "targets", "via", "protocol" or "action"
	Index int         `json:"index"` // the item of Apps, Targets or Via; -1: the field
	Code  ProblemCode `json:"code"`
}

// ProblemCode identifies a problem of a rule.
type ProblemCode string

const (
	ProblemBadTarget   ProblemCode = "bad_target"   // ParseTarget fails
	ProblemDuplicate   ProblemCode = "duplicate"    // the same app, target or interface twice
	ProblemBadApp      ProblemCode = "bad_app"      // an app without a path
	ProblemNoVia       ProblemCode = "no_via"       // a route rule names no interface
	ProblemBadVia      ProblemCode = "bad_via"      // an interface without a name
	ProblemBadProtocol ProblemCode = "bad_protocol" // not any, tcp or udp
	ProblemBadAction   ProblemCode = "bad_action"   // not route or block
)

// Problems lists what is wrong with r; none for a rule SaveConfig accepts.
func (r Rule) Problems() []Problem {
	var ps []Problem
	add := func(field string, i int, code ProblemCode) {
		ps = append(ps, Problem{Field: field, Index: i, Code: code})
	}
	seen := map[string]bool{}
	for i, a := range r.Apps {
		switch {
		case a.Path == "":
			add("apps", i, ProblemBadApp)
		case seen[a.Path]:
			add("apps", i, ProblemDuplicate)
		}
		seen[a.Path] = true
	}
	clear(seen)
	for i, s := range r.Targets {
		t, err := ParseTarget(s)
		switch {
		case err != nil:
			add("targets", i, ProblemBadTarget)
		case seen[t.String()]:
			add("targets", i, ProblemDuplicate)
		}
		seen[t.String()] = true
	}
	switch r.Protocol {
	case AnyProtocol, TCP, UDP:
	default:
		add("protocol", -1, ProblemBadProtocol)
	}
	switch r.Action {
	case Block:
	case Route:
		if len(r.Via) == 0 {
			add("via", -1, ProblemNoVia)
		}
		clear(seen)
		for i, v := range r.Via {
			switch {
			case v.Name == "":
				add("via", i, ProblemBadVia)
			case seen[v.Name+"|"+v.HardwareAddr]:
				add("via", i, ProblemDuplicate)
			}
			seen[v.Name+"|"+v.HardwareAddr] = true
		}
	default:
		add("action", -1, ProblemBadAction)
	}
	return ps
}

// Validate checks what SaveConfig requires: the current version, at least
// one scene, IDs that are set and unique, a selected scene that exists,
// rules without problems, bounded unique local proxy ports, each with an
// interface, an upstream proxy or a scene that exists, and bounded unique
// devices.
// Physical-interface eligibility is checked by the client when Via resolves;
// an offline or not-yet-present adapter does not invalidate saved configuration.
func (c Config) Validate() error {
	bad := func(format string, args ...any) error {
		return &Error{Code: ErrInvalidConfig, Detail: fmt.Sprintf(format, args...)}
	}
	if c.Version != ConfigVersion {
		return bad("version %d, want %d", c.Version, ConfigVersion)
	}
	if len(c.Scenes) == 0 {
		return bad("no scene")
	}
	scenes, rules := map[string]bool{}, map[string]bool{}
	for _, s := range c.Scenes {
		if s.ID == "" || scenes[s.ID] {
			return bad("scene ID %q is empty or not unique", s.ID)
		}
		scenes[s.ID] = true
		for i, r := range s.Rules {
			if r.ID == "" || rules[r.ID] {
				return bad("scene %q: rule %d: ID %q is empty or not unique", s.ID, i+1, r.ID)
			}
			rules[r.ID] = true
			if ps := r.Problems(); len(ps) > 0 {
				p := ps[0]
				return bad("scene %q: rule %d: %s %d: %s", s.ID, i+1, p.Field, p.Index, p.Code)
			}
		}
	}
	if !scenes[c.Selected] {
		return bad("selected scene %q does not exist", c.Selected)
	}
	if c.Settings.LogDays < 0 {
		return bad("logDays %d is negative", c.Settings.LogDays)
	}
	if len(c.Proxies) > MaxProxies {
		return bad("proxies: %d entries exceed the limit of %d", len(c.Proxies), MaxProxies)
	}
	ports := make(map[int]bool, len(c.Proxies))
	for i, proxy := range c.Proxies {
		if proxy.Port < 1024 || proxy.Port > 65535 {
			return bad("proxy %d: port %d is outside 1024..65535", i+1, proxy.Port)
		}
		if ports[proxy.Port] {
			return bad("proxy %d: port %d is not unique", i+1, proxy.Port)
		}
		ports[proxy.Port] = true
		if proxy.Upstream == "" && proxy.Scene == "" {
			if proxy.Via.Name == "" {
				return bad("proxy %d: via name is empty", i+1)
			}
			continue
		}
		if proxy.Via != (InterfaceRef{}) || proxy.Upstream != "" && proxy.Scene != "" {
			return bad("proxy %d: more than one of an interface, an upstream proxy and a scene", i+1)
		}
		if proxy.Scene != "" {
			if !scenes[proxy.Scene] {
				return bad("proxy %d: scene %q does not exist", i+1, proxy.Scene)
			}
			if proxy.ChangeIP {
				return bad("proxy %d: a proxy by a scene's rules has no one address to change", i+1)
			}
			continue
		}
		host, port, err := ParseUpstream(proxy.Upstream)
		if err != nil {
			return bad("proxy %d: upstream %q is not the host:port of a proxy", i+1, proxy.Upstream)
		}
		addr, err := netip.ParseAddr(host)
		own := host == "localhost" || err == nil && addr.Unmap().IsLoopback()
		// A port that goes out through itself would open connections to itself
		// until it is full.
		if own && port == proxy.Port {
			return bad("proxy %d: its upstream is itself", i+1)
		}
		// Through a proxy on this Mac, other devices could reach what listens
		// for this Mac alone: Pith can't answer for another proxy's care.
		if own && proxy.Shared {
			return bad("proxy %d: a shared proxy can't go out through a proxy on this Mac", i+1)
		}
	}
	if len(c.Devices) > MaxDevices {
		return bad("devices: %d entries exceed the limit of %d", len(c.Devices), MaxDevices)
	}
	devices := make(map[netip.Prefix]bool, len(c.Devices))
	for i, device := range c.Devices {
		prefix, err := device.Prefix()
		if err != nil {
			return bad("device %d: %q is not an IPv4 address of another device, or a range of at least 8 bits", i+1, device.Address)
		}
		if devices[prefix] {
			return bad("device %d: %s is not unique", i+1, prefix)
		}
		devices[prefix] = true
	}
	return nil
}

// NewID returns a new random ID for a scene or a rule.
func NewID() string {
	var b [6]byte
	rand.Read(b[:])
	return hex.EncodeToString(b[:])
}
