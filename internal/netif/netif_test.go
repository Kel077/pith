package netif

import (
	"testing"

	"github.com/Kel077/pith/internal/api"
)

func TestStoreInterfaces(t *testing.T) {
	store, err := parseStore(`<dictionary> {
  Setup:/Network/Service/A : <dictionary> {
    UserDefinedName : Example: USB
  }
  Setup:/Network/Service/A/Interface : <dictionary> {
    DeviceName : en2
    Type : Ethernet
    Hardware : Ethernet
  }
  State:/Network/Service/A/IPv4 : <dictionary> {
    Router : 192.0.2.1
  }
  State:/Network/Service/A/IPv6 : <dictionary> {
    Router : 2001:db8::1
  }
  State:/Network/Service/A/DNS : <dictionary> {
    ServerAddresses : <array> {
      0 : 192.0.2.53
      1 : 2001:db8::53
    }
  }
}`)
	if err != nil {
		t.Fatal(err)
	}
	iface := storeInterfaces(store)["en2"]
	if iface.Label != "Example: USB" || iface.Kind != api.KindEthernet || iface.Router != "192.0.2.1" || iface.Router6 != "2001:db8::1" || len(iface.DNS) != 2 || iface.DNS[1] != "2001:db8::53" {
		t.Fatal(iface)
	}
	for _, data := range []string{"", "No such key", "<dictionary> {", "}", "<dictionary> {\ninvalid\n}"} {
		if _, err := parseStore(data); err == nil {
			t.Errorf("accepted malformed store %q", data)
		}
	}
}

func TestStoreEmptyValues(t *testing.T) {
	for _, empty := range []string{"0 : ", "0 :"} {
		store, err := parseStore("<dictionary> {\n  State:/Network/Service/A/DNS : <dictionary> {\n    SupplementalMatchDomains : <array> {\n      " + empty + "\n      1 : example.com\n    }\n  }\n}")
		if err != nil {
			t.Fatalf("%q: %v", empty, err)
		}
		domains := store.dict("State:/Network/Service/A/DNS").array("SupplementalMatchDomains")
		if len(domains) != 1 || domains[0] != "example.com" {
			t.Fatalf("%q: %v", empty, domains)
		}
	}
}

func TestKinds(t *testing.T) {
	for _, tt := range []struct {
		name, label, typ string
		want             api.InterfaceKind
	}{
		{"utun3", "Example VPN", "", api.KindVPN},
		{"en2", "Wireless", "IEEE80211", api.KindWiFi},
		{"en3", "iPhone USB", "Ethernet", api.KindCellular},
		{"bridge0", "Thunderbolt Bridge", "Ethernet", api.KindThunderbolt},
		{"en4", "USB", "Ethernet", api.KindEthernet},
	} {
		if got := kind(tt.name, tt.label, tt.typ, ""); got != tt.want {
			t.Errorf("%+v: %s", tt, got)
		}
	}
}
