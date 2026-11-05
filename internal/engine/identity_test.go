package engine

import (
	"context"
	"errors"
	"reflect"
	"testing"

	"github.com/Kel077/pith/internal/api"
	"github.com/Kel077/pith/internal/engine/capture"
	"github.com/Kel077/pith/internal/engine/forward"
)

func TestLastCaptureIdentitySurvivesCleanup(t *testing.T) {
	for _, fail := range []bool{false, true} {
		t.Run(map[bool]string{false: "stop", true: "safe-start-failure"}[fail], func(t *testing.T) {
			e, _ := newTestEngine(t, func(context.Context, *capture.Capture, *forward.Forwarder) error {
				if fail {
					return errors.New("synthetic probe failure")
				}
				return nil
			})
			ctx := context.Background()
			cfg := api.DefaultConfig()
			st, _ := e.Status(ctx)
			if st.LastCapture != nil {
				t.Fatal("idle construction claimed a capture")
			}
			err := e.Start(ctx, cfg.Scenes[0], cfg.Settings)
			if (err != nil) != fail {
				t.Fatal(err)
			}
			st, _ = e.Status(ctx)
			id := st.LastCapture
			if id == nil || id.Token == "" || id.BootSession == "" || id.Interface == "" || id.Index <= 0 || !reflect.DeepEqual(id.Prefixes, []string{"192.0.2.10/32"}) {
				t.Fatal("missing prepared capture identity", id)
			}
			if err := e.Stop(ctx); err != nil {
				t.Fatal(err)
			}
			after, _ := e.Status(ctx)
			if after.State != api.Stopped || !reflect.DeepEqual(after.LastCapture, id) {
				t.Fatal("Stop lost capture history")
			}
			after.LastCapture.Prefixes[0] = "203.0.113.10/32"
			after.LastCapture.Token = "changed-copy"
			after, _ = e.Status(ctx)
			if !reflect.DeepEqual(after.LastCapture, id) {
				t.Fatal("caller mutated engine identity")
			}
			_ = e.Start(ctx, cfg.Scenes[0], cfg.Settings)
			after, _ = e.Status(ctx)
			if after.LastCapture == nil || after.LastCapture.Token == id.Token {
				t.Fatal("successive captures shared an identity")
			}
		})
	}
}
