package client

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/Kel077/pith/internal/api"
	"github.com/Kel077/pith/internal/hotspot/hotspottest"
	"github.com/Kel077/pith/internal/keychain"
)

func TestHotspot(t *testing.T) {
	c := testClient(t)
	f := hotspottest.New()
	mifi := httptest.NewServer(f)
	defer mifi.Close()
	_, c.hotspotPort, _ = net.SplitHostPort(mifi.Listener.Addr().String())
	c.dialer = func(string, time.Duration, []string) (*net.Dialer, error) {
		return &net.Dialer{Timeout: time.Second}, nil
	}
	c.interfaces = func(context.Context) ([]api.Interface, error) {
		return []api.Interface{
			{Name: "en0", Label: "Ethernet", Kind: api.KindEthernet, Up: true, Router: "127.0.0.1"},
			{Name: "en9", Label: "M2000", Kind: api.KindCellular, HardwareAddr: "02:00:5e:10:00:09", Up: true, Router: "127.0.0.1"},
		}, nil
	}
	secrets := map[string]string{}
	c.secretGet = func(_ context.Context, service, account string) (string, error) {
		if s, ok := secrets[service+"/"+account]; ok {
			return s, nil
		}
		return "", keychain.ErrNotFound
	}
	c.secretSet = func(_ context.Context, service, account, secret string) error {
		secrets[service+"/"+account] = secret
		return nil
	}
	ipify := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { fmt.Fprint(w, "192.0.2.9\n") }))
	defer ipify.Close()
	c.probeURLs = [2]string{ipify.URL, ipify.URL}
	ctx := context.Background()

	if _, err := c.Hotspot(ctx, "en0"); api.Code(err) != api.ErrUnsupported {
		t.Fatalf("Ethernet: %v", err)
	}
	if _, err := c.RenewHotspot(ctx, "en0"); api.Code(err) != api.ErrUnsupported {
		t.Fatalf("renewed Ethernet: %v", err)
	}
	h, err := c.Hotspot(ctx, "en9")
	if err != nil || !h.Locked || h.Interface != "en9" || h.Bars != 4 || h.Cell != nil {
		t.Fatalf("without a password: %+v %v", h, err)
	}
	if err := c.SetHotspotPassword(ctx, "en9", "wrong"); api.Code(err) != api.ErrWrongPassword || len(secrets) != 0 {
		t.Fatalf("wrong password: %v, kept %v", err, secrets)
	}
	if err := c.SetHotspotPassword(ctx, "en9", hotspottest.Password); err != nil || secrets[hotspotKeychain+"/02:00:5e:10:00:09"] != hotspottest.Password {
		t.Fatalf("password: %v, kept %v", err, secrets)
	}
	h, err = c.Hotspot(ctx, "en9")
	if err != nil || h.Locked || h.Cell == nil || h.Cell.Band != "n71" || h.Cell.Operator != "Example Mobile" {
		t.Fatalf("with the password: %+v %v", h, err)
	}
	r, err := c.RenewHotspot(ctx, "en9")
	if err != nil || !r.Before.OK || r.After.PublicIPv4 != "192.0.2.9" || r.Took <= 0 {
		t.Fatalf("renewal: %+v %v", r, err)
	}
	f.Do(func(f *hotspottest.MiFi) {
		if strings.Join(f.Calls, ",") != "off,on" || !f.DataOn {
			t.Fatalf("cellular data: %v, on %v", f.Calls, f.DataOn)
		}
	})
}
