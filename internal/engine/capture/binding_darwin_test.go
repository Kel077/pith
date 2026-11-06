//go:build darwin

package capture

import (
	"net"
	"testing"

	"github.com/Kel077/pith/internal/api"
)

// Hiding optional capabilities exercises the previous name-based fallback in
// the same Guard.Check path, without changing any production method at runtime.
type nameOnlyBindingDriver struct{ Driver }

func BenchmarkBoundInterfaceCheck(b *testing.B) {
	ifaces, err := net.Interfaces()
	if err != nil {
		b.Fatal(err)
	}
	var loopback net.Interface
	for _, iface := range ifaces {
		if iface.Flags&(net.FlagLoopback|net.FlagUp) == net.FlagLoopback|net.FlagUp {
			loopback = iface
			break
		}
	}
	if loopback.Index == 0 {
		b.Skip("no loopback interface available for read-only identity checks")
	}
	for _, mode := range []string{"legacy-name", "pinned-index"} {
		b.Run(mode, func(b *testing.B) {
			var driver Driver = nativeDriver{}
			if mode == "legacy-name" {
				driver = nameOnlyBindingDriver{driver}
			}
			g, err := NewGuard(&Capture{opts: Options{Driver: driver}}, []api.Interface{{Name: loopback.Name, Up: true}})
			if err != nil {
				b.Fatal(err)
			}
			binding, err := g.Binding(loopback.Name)
			if err != nil {
				b.Fatal(err)
			}
			b.ReportAllocs()
			for b.Loop() {
				if err := binding.Check(); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}
