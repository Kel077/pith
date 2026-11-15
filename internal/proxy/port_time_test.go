package proxy

import (
	"context"
	"encoding/json"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/Kel077/pith/internal/api"
)

// A port tells when its addresses were found in UTC, whatever zone the
// client keeps its times in: an answer doesn't tell this Mac's time zone.
func TestPortAnswersTellTimesInUTC(t *testing.T) {
	zone := time.FixedZone("example", -5*60*60)
	at := time.Date(2026, 1, 2, 3, 4, 5, 0, zone)
	m, f := newTestManager(t, Options{Dial: noDial(t),
		Info: func(context.Context, api.InterfaceRef, time.Duration) (api.ProxyInfo, error) {
			return api.ProxyInfo{Interface: "Example adapter", Online: true, PublicIPv4: "192.0.2.1", At: at, CanChangeIP: true}, nil
		},
		ChangeIP: func(context.Context, api.InterfaceRef) (api.ProxyIPChange, error) {
			return api.ProxyIPChange{BeforeIPv4: "192.0.2.1", PublicIPv4: "192.0.2.2", At: at, Seconds: 1}, nil
		}})
	p := testProxy(18080)
	p.ChangeIP = true
	applyTest(t, m, p)
	for _, request := range [][2]string{{"GET", "/"}, {"POST", "/change-ip"}} {
		c := portRequest(t, f, p.Port, request[0], request[1], "", "")
		res := httpAnswer(t, c)
		body, err := io.ReadAll(res.Body)
		res.Body.Close()
		if err != nil || res.StatusCode != 200 || !strings.Contains(string(body), `"at":"2026-01-02T08:04:05Z"`) {
			t.Fatalf("%s %s: %d %s (%v)", request[0], request[1], res.StatusCode, body, err)
		}
		var told struct {
			At time.Time `json:"at"`
		}
		if json.Unmarshal(body, &told) != nil || !told.At.Equal(at) {
			t.Fatalf("%s %s tells another time: %s", request[0], request[1], body)
		}
	}
	// What the address was changed to is told the same way to this Mac's user.
	change, err := m.ChangeIP(context.Background(), p.Port)
	if err != nil || change.At.Location() != time.UTC || !change.At.Equal(at) {
		t.Fatalf("ChangeIP: %+v, %v", change, err)
	}
}
