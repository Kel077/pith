package captureprefix

import (
	"net/netip"
	"slices"
	"testing"
)

func TestFullCoverageExcludesOnlyZeroFirstOctets(t *testing.T) {
	want := []string{"1.0.0.0/8", "2.0.0.0/7", "4.0.0.0/6", "8.0.0.0/5", "16.0.0.0/4", "32.0.0.0/3", "64.0.0.0/2", "128.0.0.0/1", "100::/8", "200::/7", "400::/6", "800::/5", "1000::/4", "2000::/3", "4000::/2", "8000::/1"}
	for _, ipv6 := range []bool{false, true} {
		prefixes := Full(ipv6)
		n := 8
		if ipv6 {
			n = 16
		}
		var got []string
		for _, prefix := range prefixes {
			got = append(got, prefix.String())
		}
		if !slices.Equal(got, want[:n]) {
			t.Fatal("unexpected full-capture scope", got)
		}
		for first := range 256 {
			for _, last := range []byte{0, 255} {
				v6 := [16]byte{}
				for i := range v6 {
					v6[i] = last
				}
				v6[0] = byte(first)
				for _, address := range []netip.Addr{netip.AddrFrom4([4]byte{byte(first), last, last, last}), netip.AddrFrom16(v6)} {
					if Includes(address) != (first != 0) {
						t.Fatal("address admission disagrees with capture scope", address)
					}
					matches := 0
					for _, prefix := range prefixes {
						if prefix.Contains(address) {
							matches++
						}
					}
					expected := 0
					if first != 0 && (address.Is4() || ipv6) {
						expected = 1
					}
					if matches != expected {
						t.Fatalf("%s covered %d times, want %d", address, matches, expected)
					}
				}
			}
		}
	}
}
