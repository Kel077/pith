package hotspot

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/Kel077/pith/internal/api"
)

const (
	driverUp   = `{"attached":true,"modem":"qmi","properties":{"RMNetState":"up"}}`
	driverDown = `{"attached":true,"modem":"qmi","properties":{"RMNetState":"backoff"}}`
	networkSA  = `{"radio":"on","operator":"Example Mobile","technology":"5G SA","state":"NOCONN","roaming":true,
		"cells":[{"rat":"NR5G-SA","band":"n78","rsrp":-87,"rsrq":-8,"sinr":12.5}],
		"carrierComponents":[{"role":"PCC","rat":"NR5G","band":"n78","bandwidthMHz":80},
			{"role":"SCC","rat":"NR5G","band":"n78","bandwidthMHz":20}]}`
)

func fakeRMNet(t *testing.T, reply func(context.Context, string) (string, error)) *RMNet {
	t.Helper()
	m := NewRMNet()
	m.pace = time.Millisecond
	m.usb = func(context.Context) (usbModem, error) { return usbModem{Serial: "example"}, nil }
	m.run = func(ctx context.Context, args ...string) ([]byte, error) {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		s, err := reply(ctx, strings.Join(args, " "))
		return []byte(s), err
	}
	return m
}

func TestRMNetStatus(t *testing.T) {
	networks := []string{networkSA,
		`{"radio":"on","technology":"5G NSA","networkName":"Example","carriers":["B3","n78"],"cells":[{"rat":"LTE","rsrp":-100},{"rat":"NR5G-NSA","rsrp":-91,"sinr":0}]}`,
		`{"radio":"on","technology":"LTE","cells":[{"rat":"LTE","band":"B3","rsrp":null,"sinr":null}]}`,
	}
	m := fakeRMNet(t, func(_ context.Context, cmd string) (string, error) {
		switch cmd {
		case "status --json":
			return driverUp, nil
		case "network --json":
			n := networks[0]
			networks = networks[1:]
			return n, nil
		}
		t.Fatalf("unexpected command %q", cmd)
		return "", nil
	})
	ctx := context.Background()
	h, err := m.Status(ctx)
	if err != nil || !h.Connected || h.Locked || h.Model != api.HotspotRM520N || h.Technology != "5G SA" || h.Bars == 0 {
		t.Fatalf("%+v, %v", h, err)
	}
	c := h.Cell
	if c == nil || c.Operator != "Example Mobile" || c.Band != "n78" || !c.Roaming || len(c.Carriers) != 2 ||
		c.Carriers[1] != (api.HotspotCarrier{Role: "SCC", RAT: "NR5G", Band: "n78", BandwidthMHz: 20}) ||
		c.RSRP == nil || *c.RSRP != -87 || c.RSRQ == nil || *c.RSRQ != -8 || c.SINR == nil || *c.SINR != 12.5 {
		t.Fatal(c)
	}
	h, err = m.Status(ctx)
	if err != nil || h.Cell.Operator != "Example" || h.Cell.Band != "B3" || h.Cell.Signal != -91 ||
		h.Cell.SINR == nil || *h.Cell.SINR != 0 || h.Cell.RSRQ != nil || len(h.Cell.Carriers) != 2 {
		t.Fatal(h.Cell, err)
	}
	h, err = m.Status(ctx)
	if err != nil || h.Cell.Band != "B3" || h.Cell.RSRP != nil || h.Cell.SINR != nil {
		t.Fatal(h.Cell, err)
	}
}

func TestRMNetRenewal(t *testing.T) {
	for _, tc := range []string{"reconnect", "before failed", "radio already off", "off reply lost", "cancel after off", "on reply lost", "USB briefly absent", "USB replaced"} {
		t.Run(tc, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			var calls []string
			dataUp := true
			onTries := 0
			m := fakeRMNet(t, func(cmdCtx context.Context, cmd string) (string, error) {
				switch cmd {
				case "status --json":
					if dataUp {
						return driverUp, nil
					}
					return driverDown, nil
				case "network --json":
					if tc == "radio already off" {
						return `{"radio":"airplane"}`, nil
					}
					return networkSA, nil
				case "radio off":
					calls = append(calls, "off")
					dataUp = false
					switch tc {
					case "off reply lost":
						return "", errors.New("lost reply")
					case "cancel after off":
						cancel()
					}
				case "radio on":
					if cmdCtx.Err() != nil {
						t.Fatal("restore used the canceled context")
					}
					calls = append(calls, "on")
					onTries++
					if tc == "on reply lost" && onTries == 1 {
						return "", errors.New("lost reply")
					}
					dataUp = true
				default:
					t.Fatalf("unexpected command %s", cmd)
				}
				return "", nil
			})
			missing := true
			m.usb = func(ctx context.Context) (usbModem, error) {
				if ctx.Err() != nil {
					return usbModem{}, ctx.Err()
				}
				if !dataUp {
					if tc == "USB replaced" {
						return usbModem{Serial: "other"}, nil
					}
					if tc == "USB briefly absent" && missing {
						missing = false
						return usbModem{}, errModemAbsent
					}
				}
				return usbModem{Serial: "example"}, nil
			}
			err := m.Renew(ctx, func(context.Context) error {
				calls = append(calls, "before")
				if tc == "before failed" {
					return errors.New("before failed")
				}
				return nil
			})
			want := []string{"before", "off", "on"}
			switch tc {
			case "radio already off":
				want = nil
			case "before failed":
				want = []string{"before"}
			case "USB replaced":
				want = []string{"before", "off"}
			case "on reply lost":
				want = append(want, "on")
			}
			ok := tc == "reconnect" || tc == "USB briefly absent"
			if !slices.Equal(calls, want) || (err == nil) != ok || (!dataUp && tc != "USB replaced") {
				t.Fatalf("commands %v, connected %t, error %v", calls, dataUp, err)
			}
		})
	}
}

func TestRMNetUSBSelection(t *testing.T) {
	target := usbModem{Vendor: 0x2c7c, Product: 0x0801, Entry: 7}
	if got, err := singleRM520N([]usbModem{{Vendor: 0x2c7c, Product: 1}, target}); err != nil || got != target {
		t.Fatal(got, err)
	}
	for _, devices := range [][]usbModem{nil, {target, target}, {{Vendor: 0x2c7c, Product: 0x0801}}} {
		if _, err := singleRM520N(devices); err == nil {
			t.Fatal("accepted absent, ambiguous or unidentified device")
		}
	}
}
