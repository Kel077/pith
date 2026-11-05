package ui

import (
	"os"
	"path/filepath"
	"testing"
)

func TestTrayIcon(t *testing.T) {
	for _, on := range []bool{true, false} {
		if b := TrayIcon(on); len(b) < 100 {
			t.Errorf("TrayIcon(%v): %d bytes", on, len(b))
		} else if *snapshots != "" {
			name := map[bool]string{true: "tray-on.png", false: "tray-off.png"}[on]
			os.WriteFile(filepath.Join(*snapshots, name), b, 0o644)
		}
	}
}
