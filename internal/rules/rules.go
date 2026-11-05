// Package rules compiles and evaluates the rules shared by Check and the engine.
package rules

import (
	"fmt"
	"net/netip"
	"path/filepath"
	"slices"
	"strings"

	"github.com/Kel077/pith/internal/api"
)

// Flow describes one connection after process and destination attribution.
type Flow struct {
	App      api.AppRef
	Process  api.Process
	Domain   string
	Address  netip.Addr
	Protocol api.Protocol
}

// Decision contains only the routing policy. Dialing decides whether each
// candidate is reachable; an interface being Up alone cannot establish that.
type Decision struct {
	RuleID   string
	Action   api.Action
	Via      []api.InterfaceRef
	Fallback bool
}

type compiled struct {
	rule    api.Rule
	targets []api.Target
}

// Set is immutable and can be shared by concurrent flows. Build a new Set to
// hot-reload rules; established connections keep their original Decision.
type Set struct{ rules []compiled }

func New(scene api.Scene) (*Set, error) {
	s := &Set{}
	seen := make(map[string]bool)
	for _, r := range scene.Rules {
		if r.ID == "" || seen[r.ID] || len(r.Problems()) != 0 {
			return nil, &api.Error{Code: api.ErrInvalidConfig, Detail: fmt.Sprintf("invalid rule %q", r.ID)}
		}
		seen[r.ID] = true
		r.Apps, r.Targets, r.Via = slices.Clone(r.Apps), slices.Clone(r.Targets), slices.Clone(r.Via)
		c := compiled{rule: r}
		for _, raw := range r.Targets {
			target, err := api.ParseTarget(raw)
			if err != nil {
				return nil, err
			}
			c.targets = append(c.targets, target)
		}
		s.rules = append(s.rules, c)
	}
	return s, nil
}

func (c compiled) reason(f Flow) api.CheckReason {
	if !c.rule.Enabled {
		return api.CheckDisabled
	}
	if len(c.rule.Apps) > 0 && !slices.ContainsFunc(c.rule.Apps, func(a api.AppRef) bool { return MatchApp(a, f.App, f.Process.Path) }) {
		return api.CheckApp
	}
	if len(c.targets) > 0 && !slices.ContainsFunc(c.targets, func(t api.Target) bool { return t.MatchHost(f.Domain) || t.MatchAddr(f.Address) }) {
		return api.CheckTarget
	}
	if c.rule.Protocol != api.AnyProtocol && c.rule.Protocol != f.Protocol {
		return api.CheckProtocol
	}
	return api.CheckMatched
}

func decision(r api.Rule) Decision {
	return Decision{RuleID: r.ID, Action: r.Action, Via: slices.Clone(r.Via), Fallback: r.Fallback}
}

// Match chooses the first matching block rule, otherwise the first matching
// route rule. A zero Decision means normal system routing.
func (s *Set) Match(f Flow) Decision {
	var first *api.Rule
	for i := range s.rules {
		c := &s.rules[i]
		if c.reason(f) != api.CheckMatched {
			continue
		}
		if c.rule.Action == api.Block {
			return decision(c.rule)
		}
		if first == nil {
			first = &c.rule
		}
	}
	if first != nil {
		return decision(*first)
	}
	return Decision{}
}

// Explain includes every rule in its original order, even when a later block
// rule shadows an earlier route rule.
func (s *Set) Explain(f Flow) (Decision, []api.RuleCheck) {
	d := s.Match(f)
	checks := make([]api.RuleCheck, 0, len(s.rules))
	for _, c := range s.rules {
		reason := c.reason(f)
		if reason == api.CheckMatched && c.rule.ID != d.RuleID {
			reason = api.CheckShadowed
		}
		checks = append(checks, api.RuleCheck{RuleID: c.rule.ID, Result: reason})
	}
	return d, checks
}

// OuterBundle returns the outermost .app component, including that component.
// It intentionally does no I/O; process lookup supplies the executable path.
func OuterBundle(path string) string {
	if !filepath.IsAbs(path) {
		return ""
	}
	clean := filepath.Clean(path)
	parts := strings.Split(clean, string(filepath.Separator))
	for i, part := range parts {
		if strings.HasSuffix(strings.ToLower(part), ".app") {
			return strings.Join(parts[:i+1], string(filepath.Separator))
		}
	}
	return ""
}

func MatchApp(ref, app api.AppRef, processPath string) bool {
	if ref.BundleID != "" && ref.BundleID == app.BundleID {
		return true
	}
	if ref.Path == "" || !filepath.IsAbs(ref.Path) {
		return false
	}
	want := filepath.Clean(ref.Path)
	for _, path := range []string{app.Path, processPath} {
		if !filepath.IsAbs(path) {
			continue
		}
		path = filepath.Clean(path)
		if path == want || (strings.HasSuffix(strings.ToLower(want), ".app") && strings.HasPrefix(path, want+string(filepath.Separator))) {
			return true
		}
	}
	return false
}

// ResolveInterface shares the API's exact name/MAC and unique-MAC semantics,
// including bridges and adapter ports that share a hardware address.
func ResolveInterface(ref api.InterfaceRef, interfaces []api.Interface) (api.Interface, bool) {
	if iface := ref.Resolve(interfaces); iface != nil {
		return *iface, true
	}
	return api.Interface{}, false
}

// Predict uses link availability, not an active connectivity test. The engine
// may choose a later interface if connecting through an Up interface fails.
func (d Decision) Predict(interfaces []api.Interface) (api.Outcome, string) {
	if d.Action == api.Block {
		return api.Blocked, ""
	}
	if d.RuleID != "" {
		for i, ref := range d.Via {
			if iface, ok := ResolveInterface(ref, interfaces); ok && iface.Up {
				if i == 0 {
					return api.Routed, iface.Name
				}
				return api.Fallback, iface.Name
			}
		}
		if !d.Fallback {
			return api.Unreachable, ""
		}
	}
	for _, iface := range interfaces {
		if iface.Primary && iface.Up {
			return api.System, iface.Name
		}
	}
	return api.System, ""
}
