package attribution

import (
	"context"
	"errors"
	"net/netip"
	"testing"

	"github.com/Kel077/pith/internal/api"
)

func TestFreshLookupDetectsSocketReusedInsideCacheWindow(t *testing.T) {
	r := New()
	local, remote := netip.MustParseAddrPort("192.0.2.1:50000"), netip.MustParseAddrPort("192.0.2.2:443")
	generation := uint64(1)
	reads := 0
	r.read = func(api.Protocol) ([]Socket, error) {
		reads++
		return []Socket{{Local: local, Remote: remote, PID: 42, Generation: generation}}, nil
	}
	r.identify = func(int) (identity, error) { return identity{unique: 42}, nil }
	r.path = func(int) (string, error) { return "/usr/bin/example", nil }
	first, err := r.Lookup(context.Background(), api.UDP, local, remote)
	if err != nil || first.SocketGeneration != 1 {
		t.Fatal(first, err)
	}
	generation++
	next, err := r.LookupFresh(context.Background(), api.UDP, local, remote)
	if err != nil || next.SocketGeneration != 2 || reads != 2 {
		t.Fatal("fresh socket-lifetime lookup trusted cached PCB identity", next, err, reads)
	}
}

func TestFreshLookupRejectsSameOwnerSharedPortLifetimes(t *testing.T) {
	r := New()
	local, remote := netip.MustParseAddrPort("192.0.2.1:50000"), netip.MustParseAddrPort("192.0.2.2:443")
	r.read = func(api.Protocol) ([]Socket, error) {
		return []Socket{
			{Local: local, Remote: remote, PID: 42, EffectivePID: 84, Generation: 1},
			{Local: local, Remote: remote, PID: 42, EffectivePID: 84, Generation: 2},
		}, nil
	}
	got, err := r.LookupFresh(context.Background(), api.UDP, local, remote)
	if !errors.Is(err, ErrAmbiguous) || got.SocketGeneration != 0 {
		t.Fatal("shared owner was mistaken for a unique socket lifetime", got, err)
	}
}
