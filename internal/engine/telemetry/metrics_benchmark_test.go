package telemetry

import (
	"testing"

	"github.com/Kel077/pith/internal/api"
)

// Exercise the real counter/bucket updates without disk or synthetic no-op
// instrumentation. Flush/disk work is separate and already coalesced by design.
func BenchmarkTrafficAddBytes(b *testing.B) {
	store, err := Open(Options{})
	if err != nil {
		b.Fatal(err)
	}
	id := store.Add(api.Connection{App: api.AppRef{Path: "/Applications/Example.app"}, Interface: "example0"})
	b.ReportAllocs()
	for b.Loop() {
		store.AddBytes(id, 0, 32<<10)
	}
}
