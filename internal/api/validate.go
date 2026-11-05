package api

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
)

// ErrorCode identifies an error the interface explains to the user.
type ErrorCode string

const (
	ErrHelperMissing  ErrorCode = "helper_missing"  // InstallHelper first
	ErrHelperOutdated ErrorCode = "helper_outdated" // InstallHelper updates it
	ErrCanceled       ErrorCode = "canceled"        // the user canceled macOS's password dialog
	ErrInvalidConfig  ErrorCode = "invalid_config"  // Detail says what
	ErrNoScene        ErrorCode = "no_scene"        // no scene has that ID
	ErrRouteConflict  ErrorCode = "route_conflict"  // another app holds routes Pith needs (Detail: which)
	ErrNetworkLost    ErrorCode = "network_lost"    // SafeStart undid a start that cut the internet
	ErrUnsupported    ErrorCode = "unsupported"     // Pith can't control that device
	ErrWrongPassword  ErrorCode = "wrong_password"  // a device refused the password Pith has, or Pith has none
	ErrInternal       ErrorCode = "internal"
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
// and rules without problems.
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
	return nil
}

// NewID returns a new random ID for a scene or a rule.
func NewID() string {
	var b [6]byte
	rand.Read(b[:])
	return hex.EncodeToString(b[:])
}
