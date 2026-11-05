package capture

import (
	"context"
	"errors"
	"net/netip"
	"os"
	"slices"
	"testing"
)

type exclusionDriver struct {
	*fakeDriver
	exclusions []netip.Prefix
	err        error
	reads      int
}

func (d *exclusionDriver) AddressExclusions() ([]netip.Prefix, error) {
	d.reads++
	return slices.Clone(d.exclusions), d.err
}

func TestPrepareAddressExclusions(t *testing.T) {
	v4 := ipv4Candidates[0].Addr().Next()
	v6 := ipv6Candidates[0].Addr().Next()
	for _, test := range []struct {
		name       string
		parents    []netip.Prefix
		exclusions []netip.Prefix
		assigned   []netip.Prefix
		want       TunnelAddresses
	}{
		{
			name:       "default parents",
			parents:    []netip.Prefix{prefix("0.0.0.0/0"), prefix("::/0")},
			exclusions: []netip.Prefix{netip.PrefixFrom(v4, 32), netip.PrefixFrom(v6, 128)},
			want:       TunnelAddresses{IPv4: ipv4Candidates[1], IPv6: ipv6Candidates[1]},
		},
		{
			name:       "split parents",
			parents:    []netip.Prefix{prefix("0.0.0.0/1"), prefix("128.0.0.0/1"), prefix("::/1"), prefix("8000::/1")},
			exclusions: []netip.Prefix{netip.PrefixFrom(v4, 32), netip.PrefixFrom(v6, 128)},
			want:       TunnelAddresses{IPv4: ipv4Candidates[1], IPv6: ipv6Candidates[1]},
		},
		{
			name:    "empty cache",
			parents: []netip.Prefix{prefix("0.0.0.0/0"), prefix("::/0")},
			want:    TunnelAddresses{IPv4: ipv4Candidates[0], IPv6: ipv6Candidates[0]},
		},
		{
			name:       "assigned prefixes retained",
			parents:    []netip.Prefix{prefix("0.0.0.0/0"), prefix("::/0")},
			exclusions: []netip.Prefix{netip.PrefixFrom(v4, 32)},
			assigned:   []netip.Prefix{ipv4Candidates[1]},
			want:       TunnelAddresses{IPv4: ipv4Candidates[2], IPv6: ipv6Candidates[0]},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			var routes []Route
			for _, p := range test.parents {
				routes = append(routes, Route{Prefix: p, Interface: "test0", Index: 1})
			}
			d := &exclusionDriver{fakeDriver: &fakeDriver{routes: slices.Clone(routes), assigned: test.assigned}, exclusions: test.exclusions}
			opts := fakeOptions(t, d.fakeDriver)
			opts.Driver, opts.IPv6 = d, true
			opts.Scopes = []netip.Prefix{prefix("192.0.2.10/32"), prefix("2001:db8::10/128")}
			c, err := Prepare(context.Background(), opts)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() {
				if err := c.Close(); err != nil {
					t.Error(err)
				}
			})
			if d.reads != 1 || c.Plan.Addresses != test.want || d.configured != test.want {
				t.Fatalf("reads=%d plan=%v configured=%v; want %v", d.reads, c.Plan.Addresses, d.configured, test.want)
			}
			state, err := readState(opts.StatePath)
			if err != nil {
				t.Fatal(err)
			}
			if !slices.Equal(c.Plan.Before, routes) || !slices.Equal(state.Plan.Before, routes) || !slices.Equal(c.Plan.Prefixes, opts.Scopes) || !slices.Equal(state.Plan.Prefixes, opts.Scopes) || state.Plan.Addresses != test.want {
				t.Fatalf("address exclusions changed routing policy or journal: plan=%+v journal=%+v", c.Plan, state.Plan)
			}
			for _, remote := range []netip.Addr{v4, v6} {
				if got := SystemInterface(c.Plan.Before, remote); got != "test0" {
					t.Fatalf("system interface = %q; want saved parent interface", got)
				}
			}
			if err := c.Activate(context.Background()); err != nil {
				t.Fatal(err)
			}
			if err := c.Close(); err != nil {
				t.Fatal(err)
			}
			if !slices.Equal(d.routes, routes) {
				t.Fatalf("cleanup changed parent routes: %v", d.routes)
			}
			if d.reads != 1 {
				t.Fatal("transient endpoints were read outside address selection")
			}
		})
	}
}

func TestPrepareAddressExclusionsFailureBeforeCreate(t *testing.T) {
	failure := errors.New("transient endpoint read failed")
	d := &exclusionDriver{fakeDriver: &fakeDriver{routes: baseRoutes()}, err: failure}
	opts := fakeOptions(t, d.fakeDriver)
	opts.Driver = d
	c, err := Prepare(context.Background(), opts)
	if !errors.Is(err, failure) || c != nil || d.reads != 1 {
		t.Fatalf("capture=%v reads=%d error=%v", c, d.reads, err)
	}
	if d.device != nil || len(d.operations) != 0 {
		t.Fatalf("failed observation caused a mutation: %v", d.operations)
	}
	if _, err := os.Stat(opts.StatePath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("failed observation wrote a journal: %v", err)
	}
}
