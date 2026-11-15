package client

import (
	"context"
	"sync"
	"testing"

	"github.com/Kel077/pith/internal/api"
)

func TestProxyOptionsWireTheClientsExitHooks(t *testing.T) {
	gate, after := make(chan struct{}), make(chan struct{}, 2)
	open := sync.OnceFunc(func() { close(gate) })
	defer open()
	c, _ := hotspotExitClient(t, gate, after)
	opts := c.proxyOptions()
	if opts.Info == nil || opts.ChangeIP == nil || opts.Changing == nil {
		t.Fatal("production proxy options omit an exit hook")
	}
	ref := api.InterfaceRef{Name: "example0"}
	if opts.Changing(ref) {
		t.Fatal("an idle interface was changing")
	}
	before, err := opts.Info(context.Background(), ref, -1)
	if err != nil || !before.Online || !before.CanChangeIP || before.Interface != "M2000" || before.PublicIPv4 != "198.51.100.10" {
		t.Fatal("the production Info hook didn't call the client", before, err)
	}
	done := make(chan struct{})
	var result api.ProxyIPChange
	go func() { defer close(done); result, err = opts.ChangeIP(context.Background(), ref) }()
	awaitExit(t, after)
	if !opts.Changing(ref) {
		t.Fatal("the production Changing hook didn't call the client")
	}
	open()
	awaitExit(t, done)
	if err != nil || result.BeforeIPv4 != before.PublicIPv4 || result.PublicIPv4 != "198.51.100.20" || opts.Changing(ref) {
		t.Fatal("the production ChangeIP hook didn't call the client", result, err)
	}
}
