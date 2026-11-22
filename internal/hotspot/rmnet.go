package hotspot

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"os/exec"
	"strings"
	"time"

	"github.com/Kel077/pith/internal/api"
	"howett.net/plist"
)

// RMNet opens its one AT port for each CLI invocation. Share this controller
// across the client so a status read cannot take the port during a radio cycle.
type RMNet struct {
	gate chan struct{}
	run  func(context.Context, ...string) ([]byte, error)
	usb  func(context.Context) (usbModem, error)
	pace time.Duration
}

func NewRMNet() *RMNet {
	return &RMNet{gate: make(chan struct{}, 1), run: runRMNet, usb: attachedRM520N, pace: 500 * time.Millisecond}
}

func runRMNet(ctx context.Context, args ...string) ([]byte, error) {
	const path = "/Applications/RMNet.app/Contents/MacOS/RMNet"
	// radio on can spend 20 seconds enabling RF, then 30 waiting for registration.
	limit := 15 * time.Second
	if args[0] == "radio" {
		limit = time.Minute
	}
	ctx, cancel := context.WithTimeout(ctx, limit)
	defer cancel()
	b, err := exec.CommandContext(ctx, path, args...).Output()
	if ctx.Err() != nil {
		return nil, ctx.Err()
	}
	if err != nil {
		// The CLI prints a short failure message on stdout. JSON diagnostics
		// may include the subscriber's network and addresses.
		line := strings.TrimSpace(string(b))
		if len(line) <= 300 && !strings.ContainsAny(line, "\r\n") && strings.HasPrefix(line, args[0]+": ") {
			return nil, errors.New("RMNet " + line)
		}
		return b, fmt.Errorf("RMNet %s: %w", args[0], err)
	}
	return b, nil
}

type usbModem struct {
	Vendor  uint16 `plist:"idVendor"`
	Product uint16 `plist:"idProduct"`
	Serial  string `plist:"USB Serial Number"`
	Entry   uint64 `plist:"IORegistryEntryID"`
}

var errModemAbsent = errors.New("RMNet: the RM520N-GL is not attached")

// RMNet opens the first 2c7c:0801 USB device and has no device selector.
// Inspect the same USB class, including modems without a network interface.
func attachedRM520N(ctx context.Context) (usbModem, error) {
	ctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	b, err := exec.CommandContext(ctx, "/usr/sbin/ioreg", "-a", "-r", "-c", "IOUSBHostDevice", "-d", "1").Output()
	if err != nil {
		return usbModem{}, fmt.Errorf("RMNet: read USB devices: %w", err)
	}
	var devices []usbModem
	if _, err := plist.Unmarshal(b, &devices); err != nil {
		return usbModem{}, err
	}
	return singleRM520N(devices)
}

func singleRM520N(devices []usbModem) (usbModem, error) {
	var found *usbModem
	for i := range devices {
		d := &devices[i]
		if d.Vendor != 0x2c7c || d.Product != 0x0801 {
			continue
		}
		if found != nil {
			return usbModem{}, errors.New("RMNet cannot select between two attached RM520N-GL modems")
		}
		found = d
	}
	if found == nil {
		return usbModem{}, errModemAbsent
	}
	if found.Serial == "" && found.Entry == 0 {
		return usbModem{}, errors.New("RMNet: missing USB identity")
	}
	return *found, nil
}

func (m *RMNet) checkUSB(ctx context.Context, want usbModem) error {
	d, err := m.usb(ctx)
	if err != nil {
		return err
	}
	if d.Serial != want.Serial || (want.Serial == "" && d.Entry != want.Entry) {
		return errors.New("RMNet: the USB modem was replaced")
	}
	return nil
}

func (m *RMNet) acquire(ctx context.Context) error {
	select {
	case m.gate <- struct{}{}:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

type rmnetState struct {
	Modem      string
	Attached   bool
	Properties struct {
		State string `json:"RMNetState"`
	}
}

func (s rmnetState) connected() bool {
	return s.Modem == "qmi" && s.Attached && (s.Properties.State == "up" || s.Properties.State == "refreshing")
}

func (m *RMNet) state(ctx context.Context) (s rmnetState, err error) {
	b, err := m.run(ctx, "status", "--json")
	// The extension listing may fail even though the CLI read the driver's
	// registry properties. Only those properties describe the data session.
	if decodeErr := json.Unmarshal(b, &s); decodeErr != nil || s.Modem == "" {
		if err == nil {
			err = errors.New("RMNet: invalid driver status")
		}
		return s, err
	}
	return s, nil
}

type rmnetLeg struct {
	RAT, Band  string
	RSRP, RSRQ *int
	SINR       *float64
}

type rmnetNetwork struct {
	Radio, Operator, NetworkName, Technology string
	Roaming                                  bool
	Cells                                    []rmnetLeg
	Carriers                                 []string
	CarrierComponents                        []api.HotspotCarrier
}

func (m *RMNet) network(ctx context.Context) (n rmnetNetwork, err error) {
	b, err := m.run(ctx, "network", "--json")
	if err == nil {
		err = json.Unmarshal(b, &n)
	}
	return n, err
}

func (n rmnetNetwork) hotspot(s rmnetState) api.Hotspot {
	h := api.Hotspot{Model: api.HotspotRM520N, Connected: s.connected() && n.Radio != "airplane" && n.Radio != "minimum", Technology: n.Technology, At: time.Now()}
	c := &api.HotspotCell{Operator: cmp.Or(n.Operator, n.NetworkName), Roaming: n.Roaming, Carriers: n.CarrierComponents}
	if len(c.Carriers) == 0 { // Older versions report only the bands.
		for _, band := range n.Carriers {
			c.Carriers = append(c.Carriers, api.HotspotCarrier{Band: band})
		}
	}
	if len(c.Carriers) != 0 {
		c.Band = c.Carriers[0].Band
	}
	if len(n.Cells) != 0 {
		leg := n.Cells[0]
		for _, other := range n.Cells {
			if other.RAT == "NR5G-SA" || other.RAT == "NR5G-NSA" && other.RSRP != nil {
				leg = other
				break
			}
		}
		c.Band = cmp.Or(c.Band, leg.Band)
		if leg.RAT != "WCDMA" { // WCDMA reports RSCP instead.
			c.RSRP, c.RSRQ = leg.RSRP, leg.RSRQ
		}
		c.SINR = leg.SINR
		if leg.RSRP != nil {
			c.Signal = *leg.RSRP
			h.Bars = min(5, max(0, (c.Signal+120)/10))
		}
		if c.SINR != nil {
			c.SNR = int(math.Round(*c.SINR))
		}
	}
	h.Cell = c
	return h
}

func (m *RMNet) Status(ctx context.Context) (api.Hotspot, error) {
	h := api.Hotspot{Model: api.HotspotRM520N}
	if err := m.acquire(ctx); err != nil {
		return h, err
	}
	defer func() { <-m.gate }()
	if _, err := m.usb(ctx); err != nil {
		return h, err
	}
	s, err := m.state(ctx)
	if err != nil {
		return h, err
	}
	if !s.Attached || s.Modem != "qmi" {
		return h, errors.New("RMNet: the QMI driver is not attached")
	}
	n, err := m.network(ctx)
	return n.hotspot(s), err
}

func (m *RMNet) Renew(ctx context.Context, before func(context.Context) error) (resultErr error) {
	if err := m.acquire(ctx); err != nil {
		return err
	}
	defer func() { <-m.gate }()
	id, err := m.usb(ctx)
	if err != nil {
		return err
	}
	s, err := m.state(ctx)
	if err != nil {
		return err
	}
	n, err := m.network(ctx)
	if err != nil {
		return err
	}
	if !s.connected() || n.Radio != "on" {
		return &api.Error{Code: api.ErrInterfaceUnavailable, Detail: "RMNet needs an active data connection to renew"}
	}
	if before != nil {
		if err := before(ctx); err != nil {
			return err
		}
	}
	if err := m.checkUSB(ctx, id); err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	// The modem can act even when the caller misses the reply. Restore with
	// a fresh context, also when the normal radio-on attempt was interrupted.
	restore := true
	defer func() {
		if restore {
			cleanup, cancel := context.WithTimeout(context.Background(), 22*time.Second)
			defer cancel()
			if err := m.radioOn(cleanup, id); err != nil {
				resultErr = errors.Join(resultErr, fmt.Errorf("RMNet: restore radio: %w", err))
			}
		}
	}()
	if _, err := m.run(ctx, "radio", "off"); err != nil {
		return err
	}
	// The successful off command acknowledges airplane mode. The driver can
	// lag the modem slightly while it processes the data-call disconnect.
	drop, cancel := context.WithTimeout(ctx, 3*time.Second)
	_ = m.waitData(drop, false)
	cancel()
	if err := m.radioOn(ctx, id); err != nil {
		return err
	}
	restore = false
	// Radio on waits for registration. Only the driver's WDS state says that
	// a data call is up; the client then tests public addresses on its port.
	reconnect, cancel := context.WithTimeout(ctx, 90*time.Second)
	defer cancel()
	if err := m.waitData(reconnect, true); err != nil {
		return fmt.Errorf("RMNet: wait for the data connection: %w", err)
	}
	return m.checkUSB(ctx, id)
}

func (m *RMNet) radioOn(ctx context.Context, id usbModem) error {
	// A re-enumerating device may have no BSD interface yet. Identify the
	// USB device directly before enabling it and refuse a replacement.
	wait, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	for {
		err := m.checkUSB(wait, id)
		if !errors.Is(err, errModemAbsent) {
			if err != nil {
				return err
			}
			_, err = m.run(ctx, "radio", "on")
			return err
		}
		select {
		case <-wait.Done():
			return wait.Err()
		case <-time.After(m.pace):
		}
	}
}

func (m *RMNet) waitData(ctx context.Context, up bool) error {
	for {
		if s, err := m.state(ctx); err == nil && s.connected() == up {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(m.pace):
		}
	}
}
