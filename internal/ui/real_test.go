package ui

import (
	"context"
	"flag"
	"testing"
	"time"

	"github.com/egoist/mygo/ui"

	"github.com/Kel077/pith/internal/client"
)

var real = flag.Bool("real", false, "with -snapshots, also render the pages from this Mac's real backend (read-only)")

// TestRealSnapshots renders the network page from the real client: this
// Mac's interfaces, read without root and without writing anything.
func TestRealSnapshots(t *testing.T) {
	if *snapshots == "" || !*real {
		t.Skip("pass -snapshots=DIR -real")
	}
	cl, err := client.New(client.Options{ConfigDir: t.TempDir(), Version: "dev"})
	if err != nil {
		t.Fatal(err)
	}
	a := New(Options{Service: cl, Desktop: &fakeDesktop{}, Post: func(fn func()) { fn() }, Version: "dev", Now: time.Now})
	a.Sync(context.Background())
	for _, page := range []string{"network", "rules"} {
		a.page = page
		tt := ui.NewTester(a.WindowView, 1040, 680)
		tt.SetScale(2)
		tt.Frame()
		save(t, tt.Image(), "real-"+page)
	}
	a.openEditor(a.sceneID, -1)
	tt := ui.NewTester(a.WindowView, 1040, 780)
	tt.SetScale(2)
	tt.Frame()
	save(t, tt.Image(), "real-editor")
}
