package ui

import (
	"strings"
	"testing"
	"unicode/utf8"
)

// The menu bar's rate keeps one length and one shape, so the item never
// resizes as it changes: only digits and figure spaces vary, and the unit
// stays put below a gigabyte a second.
func TestTrayRateKeepsItsWidth(t *testing.T) {
	want := utf8.RuneCountInString(trayRate(0))
	shape := func(s string) string {
		return strings.Map(func(r rune) rune {
			if r == ' ' || r >= '0' && r <= '9' {
				return '#'
			}
			return r
		}, s)
	}
	for _, n := range []uint64{0, 1, 999, 1_000, 49_999, 50_000, 999_999, 1_000_000, 9_950_000, 12_345_678, 99_949_999, 99_950_000, 123_456_789, 999_949_999} {
		got := trayRate(n)
		if utf8.RuneCountInString(got) != want || shape(got) != shape(trayRate(0)) {
			t.Errorf("trayRate(%d) = %q, not the shape of %q", n, got, trayRate(0))
		}
		if strings.Contains(got, " ") && strings.Index(got, " ") != strings.LastIndex(got, " ") {
			t.Errorf("trayRate(%d) = %q pads with ordinary spaces", n, got)
		}
	}
	if got := trayRate(1_500_000_000); !strings.HasSuffix(got, " GB/s") || utf8.RuneCountInString(got) != want {
		t.Errorf("trayRate(1.5 GB/s) = %q", got)
	}
	if got := trayRate(12_345_678); got != "\u200712.3 MB/s" {
		t.Errorf("trayRate(12.3 MB/s) = %q", got)
	}
}
