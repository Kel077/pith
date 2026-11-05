package engine

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/Kel077/pith/internal/api"
	"github.com/Kel077/pith/internal/engine/capture"
	"github.com/Kel077/pith/internal/engine/forward"
)

func TestRouteDiagnosticsSurviveFailedSafeStartAndStop(t *testing.T) {
	const diagnostic = "RTM type=1 flags=0x801 index=42 addrs=[0:family=30/len=28]: non-default destination without HOST or netmask"
	var d *testDriver
	e, d := newTestEngine(t, func(ctx context.Context, _ *capture.Capture, _ *forward.Forwarder) error {
		d.changes <- capture.Change{Index: 42, Unparsed: true, Diagnostic: diagnostic}
		<-ctx.Done()
		return ctx.Err()
	})
	ctx := context.Background()
	cfg := api.DefaultConfig()
	if err := e.Start(ctx, cfg.Scenes[0], cfg.Settings); api.Code(err) != api.ErrNetworkLost {
		t.Fatal(err)
	}
	status, _ := e.Status(ctx)
	if status.State != api.Failed || len(status.RouteDiagnostics) != 1 || !strings.Contains(status.RouteDiagnostics[0], diagnostic) || !strings.Contains(status.RouteDiagnostics[0], "owned interface") || !strings.Contains(status.Error.Detail, diagnostic) {
		t.Fatal("startup lost the route fault metadata", status)
	}
	for range 2 {
		if err := e.Stop(ctx); err != nil {
			t.Fatal(err)
		}
		stopped, _ := e.Status(ctx)
		if stopped.State != api.Stopped || !slices.Equal(stopped.RouteDiagnostics, status.RouteDiagnostics) {
			t.Fatal("Stop discarded failed-start evidence", stopped)
		}
	}
	e.opts.Verify = func(context.Context, *capture.Capture, *forward.Forwarder) error { return nil }
	if err := e.Start(ctx, cfg.Scenes[0], cfg.Settings); err != nil {
		t.Fatal(err)
	}
	status, _ = e.Status(ctx)
	if len(status.RouteDiagnostics) != 0 {
		t.Fatal("next capture retained the previous attempt's diagnostics")
	}
}

func TestUnrelatedStartupDiagnosticKeepsCaptureRunning(t *testing.T) {
	e, d := newTestEngine(t, nil)
	e.opts.Verify = func(ctx context.Context, _ *capture.Capture, _ *forward.Forwarder) error {
		d.changes <- capture.Change{Index: 50, Scoped: true, Unparsed: true, Diagnostic: "RTM type=1 flags=0x1000001 index=50 addrs=[0:family=30/len=28]: synthetic external format"}
		ticker := time.NewTicker(time.Millisecond)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-ticker.C:
				status, _ := e.Status(ctx)
				if len(status.RouteDiagnostics) > 0 {
					return nil
				}
			}
		}
	}
	cfg := api.DefaultConfig()
	ctx := context.Background()
	if err := e.Start(ctx, cfg.Scenes[0], cfg.Settings); err != nil {
		t.Fatal(err)
	}
	status, _ := e.Status(ctx)
	if status.State != api.Running || len(status.RouteDiagnostics) != 1 || !strings.Contains(status.RouteDiagnostics[0], "external scoped route") {
		t.Fatal("Running lost the startup observation", status)
	}
	if err := e.Reload(ctx, cfg.Scenes[0], cfg.Settings); err != nil {
		t.Fatal(err)
	}
	if err := e.Stop(ctx); err != nil {
		t.Fatal(err)
	}
	stopped, _ := e.Status(ctx)
	if !slices.Equal(stopped.RouteDiagnostics, status.RouteDiagnostics) {
		t.Fatal("Reload or Stop discarded diagnostics", stopped)
	}
}

func TestRouteDiagnosticsBoundAndCopy(t *testing.T) {
	e, _ := newTestEngine(t, nil)
	for i := range 35 {
		summary := fmt.Sprintf("RTM type=1 flags=0x0 index=%d addrs=[]: synthetic format", i)
		e.routeDiagnostic(summary)
		e.routeDiagnostic(summary)
	}
	status, _ := e.Status(context.Background())
	if len(status.RouteDiagnostics) != 32 || !strings.Contains(status.RouteDiagnostics[0], "index=3 ") {
		t.Fatal("diagnostic ring is unbounded or contains duplicates", status.RouteDiagnostics)
	}
	status.RouteDiagnostics[0] = "mutated copy"
	copy, _ := e.Status(context.Background())
	if copy.RouteDiagnostics[0] == status.RouteDiagnostics[0] {
		t.Fatal("caller can mutate retained evidence")
	}
}
