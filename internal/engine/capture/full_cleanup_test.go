package capture

import (
	"context"
	"net/netip"
	"os"
	"slices"
	"testing"

	"github.com/Kel077/pith/internal/captureprefix"
)

func TestFullCaptureJournalAndEveryCleanupPath(t *testing.T) {
	for _, ipv6 := range []bool{false, true} {
		for _, mode := range []string{"stop", "crash", "emergency"} {
			d := &fakeDriver{routes: baseRoutes()}
			opts := fakeOptions(t, d)
			opts.IPv6 = ipv6
			c, err := Prepare(context.Background(), opts)
			if err != nil {
				t.Fatal(err)
			}
			defer c.Close()
			prefixes := captureprefix.Full(ipv6)
			state, err := readState(opts.StatePath)
			if err != nil || !slices.Equal(state.Plan.Prefixes, prefixes) {
				t.Fatal("journal did not freeze every split prefix", err)
			}
			var identityPrefixes []netip.Prefix
			for _, raw := range c.Identity().Prefixes {
				identityPrefixes = append(identityPrefixes, netip.MustParsePrefix(raw))
			}
			if !slices.Equal(identityPrefixes, prefixes) {
				t.Fatal("controller identity lost split prefixes")
			}
			if err := c.Activate(context.Background()); err != nil {
				t.Fatal(err)
			}
			// Cleanup must distinguish ownership even if another interface takes
			// every same prefix while capture is active.
			want := baseRoutes()
			for _, prefix := range prefixes {
				foreign := Route{Prefix: prefix, Interface: "utun70", Index: 70}
				d.routes = append(d.routes, foreign)
				want = append(want, foreign)
			}
			switch mode {
			case "stop":
				err = c.Close()
			case "crash":
				err = d.device.Close() // Model kernel fd teardown, with no explicit Delete.
				if err == nil {
					err = Recover(opts.StatePath, d)
				}
			case "emergency":
				err = cleanupStale(context.Background(), opts.StatePath, d, func(int) bool { return false })
			}
			if err != nil || !slices.Equal(d.routes, want) {
				t.Fatal("cleanup left capture or changed foreign routing", mode, ipv6, err, d.routes)
			}
			if _, err := os.Stat(opts.StatePath); !os.IsNotExist(err) {
				t.Fatal("completed cleanup left ownership metadata", err)
			}
		}
	}
}

func TestEverySplitPrefixHasAnActivationConflictCheck(t *testing.T) {
	for _, prefix := range captureprefix.Full(true) {
		d := &fakeDriver{routes: baseRoutes()}
		opts := fakeOptions(t, d)
		opts.IPv6 = true
		c, err := Prepare(context.Background(), opts)
		if err != nil {
			t.Fatal(err)
		}
		foreign := Route{Prefix: prefix, Interface: "utun70", Index: 70}
		d.routes = append(d.routes, foreign)
		if err := c.Activate(context.Background()); err == nil || d.adds != 0 || !slices.Contains(d.routes, foreign) {
			t.Fatal("activation failed to preserve conflicting route", prefix, err)
		}
		if err := c.Close(); err != nil {
			t.Fatal(err)
		}
	}
}
