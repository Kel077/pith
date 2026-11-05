package client

import (
	"context"
	"fmt"
	"net/netip"

	"github.com/Kel077/pith/internal/api"
	"github.com/Kel077/pith/internal/rules"
)

// Check uses the first system-resolver address, unless Address is explicit, and
// returns the complete address list for the UI. It predicts link availability;
// only an actual connection can prove that a destination is reachable.
func (c *Client) Check(ctx context.Context, in api.CheckInput) (api.CheckResult, error) {
	cfg, err := c.LoadConfig(ctx)
	if err != nil {
		return api.CheckResult{}, err
	}
	if in.SceneID == "" {
		in.SceneID = cfg.Selected
	}
	var scene *api.Scene
	for i := range cfg.Scenes {
		if cfg.Scenes[i].ID == in.SceneID {
			scene = &cfg.Scenes[i]
			break
		}
	}
	if scene == nil {
		return api.CheckResult{}, &api.Error{Code: api.ErrNoScene, Detail: in.SceneID}
	}
	proto := in.Protocol
	if proto == api.AnyProtocol || proto == "" {
		proto = api.TCP
	}
	if proto != api.TCP && proto != api.UDP {
		return api.CheckResult{}, fmt.Errorf("invalid protocol %q", in.Protocol)
	}
	target, err := api.ParseTarget(in.Target)
	if err != nil || target.Subdomains || (target.Prefix.IsValid() && !target.Prefix.IsSingleIP()) {
		return api.CheckResult{}, fmt.Errorf("check needs a concrete host or address")
	}
	f := rules.Flow{App: in.App, Domain: target.Domain, Protocol: proto}
	result := api.CheckResult{}
	switch {
	case in.Address != "":
		f.Address, err = netip.ParseAddr(in.Address)
		if err != nil {
			return result, fmt.Errorf("invalid check address: %w", err)
		}
		f.Address = f.Address.WithZone("").Unmap()
		if target.IsDomain() {
			result.Addresses = []string{f.Address.String()}
		}
	case target.IsDomain():
		addresses, err := c.lookup(ctx, "ip", target.Domain)
		if err != nil {
			return result, fmt.Errorf("resolve check target: %w", err)
		}
		seen := make(map[netip.Addr]bool)
		for _, address := range addresses {
			address = address.WithZone("").Unmap()
			if !address.IsValid() || seen[address] {
				continue
			}
			seen[address] = true
			result.Addresses = append(result.Addresses, address.String())
			if !f.Address.IsValid() {
				f.Address = address
			}
		}
		if !f.Address.IsValid() {
			return result, fmt.Errorf("check target has no IP addresses")
		}
	default:
		f.Address = target.Prefix.Addr()
	}
	set, err := rules.New(*scene)
	if err != nil {
		return result, err
	}
	d, checks := set.Explain(f)
	result.RuleID, result.Rules = d.RuleID, checks
	interfaces, err := c.interfaces(ctx)
	if err != nil {
		return result, err
	}
	result.Outcome, result.Interface = d.Predict(interfaces)
	return result, ctx.Err()
}
