package ui

import (
	"testing"

	"github.com/Kel077/pith/internal/mock"
)

// A new proxy takes the next free port and a connected interface no proxy
// uses yet; a port that is out of range or another proxy's is refused.
func TestProxyAddAndPort(t *testing.T) {
	a, _ := testApp(t, mock.Options{Sample: true, HelperInstalled: true})
	exits, _ := a.proxyExits(a.cfg.Proxies[0].Via)
	a.addProxy(exits)
	if n := len(a.cfg.Proxies); n != 4 {
		t.Fatalf("%d proxies", n)
	}
	if p := a.cfg.Proxies[3]; p.Port != 1083 || p.Via.Name != "en7" || !p.Enabled {
		t.Errorf("added %+v, want 1083 through the iPhone, on", p)
	}
	for _, bad := range []string{"1081", "80", "70000", "x"} {
		a.setProxyPort(0, bad)
		if a.cfg.Proxies[0].Port != 1080 {
			t.Errorf("port %q taken: %d", bad, a.cfg.Proxies[0].Port)
		}
	}
	a.setProxyPort(0, "2080")
	if a.cfg.Proxies[0].Port != 2080 || a.settings.editingPort != 0 {
		t.Errorf("port 2080: %+v, editing %d", a.cfg.Proxies[0], a.settings.editingPort)
	}
}
