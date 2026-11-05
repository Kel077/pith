package hotspot

import (
	"context"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/Kel077/pith/internal/api"
	"github.com/Kel077/pith/internal/hotspot/hotspottest"
)

func device(t *testing.T, f *hotspottest.MiFi, password string) *Inseego {
	t.Helper()
	srv := httptest.NewServer(f)
	t.Cleanup(srv.Close)
	jar, _ := cookiejar.New(nil)
	return &Inseego{
		Base: srv.URL, Model: "M2000", Poll: time.Millisecond,
		HTTP:     &http.Client{Jar: jar, Timeout: 5 * time.Second},
		Password: func() (string, error) { return password, nil },
	}
}

func TestModel(t *testing.T) {
	for label, want := range map[string]string{"M2000": "M2000", "Inseego MiFi M2100": "M2100", "MiFi 8800L": "MiFi", "iPhone USB": "", "Ethernet": ""} {
		if got := Model(label); got != want {
			t.Errorf("Model(%q) = %q, want %q", label, got, want)
		}
	}
}

func TestStatusWithoutPassword(t *testing.T) {
	f := hotspottest.New()
	h, err := device(t, f, "").Status(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if !h.Connected || h.Technology != "5G" || h.Bars != 4 || !h.Locked || h.Cell != nil || h.Model != "M2000" {
		t.Fatalf("%+v", h)
	}
	if f.Logins != 0 {
		t.Fatalf("logged in %d times without a password", f.Logins)
	}
}

func TestStatusReadsTheCell(t *testing.T) {
	h, err := device(t, hotspottest.New(), hotspottest.Password).Status(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	want := api.HotspotCell{Operator: "Example Mobile", Band: "n71", Signal: -101, SNR: 9}
	if h.Locked || h.Cell == nil || *h.Cell != want {
		t.Fatalf("%+v %+v", h, h.Cell)
	}
}

func TestRefusedPasswordIsNotTriedAgain(t *testing.T) {
	f := hotspottest.New()
	d := device(t, f, "wrong")
	for range 3 {
		h, err := d.Status(context.Background())
		if err != nil || !h.Locked {
			t.Fatalf("%+v %v", h, err)
		}
	}
	if f.Logins != 1 {
		t.Fatalf("tried a refused password %d times", f.Logins)
	}
	if err := d.TryPassword(context.Background(), "wrong"); api.Code(err) != api.ErrWrongPassword || f.Logins != 1 {
		t.Fatalf("TryPassword: %v after %d logins", err, f.Logins)
	}
	if err := d.TryPassword(context.Background(), hotspottest.Password); err != nil {
		t.Fatal(err)
	}
}

func TestSessionThatEndsLogsInAgain(t *testing.T) {
	f := hotspottest.New()
	d := device(t, f, hotspottest.Password)
	if _, err := d.Status(context.Background()); err != nil {
		t.Fatal(err)
	}
	f.Do(func(f *hotspottest.MiFi) { f.Expire = true })
	if _, err := d.Status(context.Background()); err != nil {
		t.Fatal(err)
	}
	h, err := d.Status(context.Background())
	if err != nil || h.Cell == nil {
		t.Fatalf("%+v %v", h, err)
	}
	if f.Logins != 2 {
		t.Fatalf("logins = %d, want 2", f.Logins)
	}
}

func TestRenew(t *testing.T) {
	f := hotspottest.New()
	d := device(t, f, hotspottest.Password)
	if err := d.Renew(context.Background()); err != nil {
		t.Fatal(err)
	}
	if strings.Join(f.Calls, ",") != "off,on" || !f.DataOn {
		t.Fatalf("calls %v, data on %v", f.Calls, f.DataOn)
	}
	if h, err := d.Status(context.Background()); err != nil || !h.Connected {
		t.Fatalf("%+v %v", h, err)
	}
}

func TestRenewNeedsThePassword(t *testing.T) {
	f := hotspottest.New()
	if err := device(t, f, "").Renew(context.Background()); api.Code(err) != api.ErrWrongPassword {
		t.Fatalf("err = %v", err)
	}
	if len(f.Calls) != 0 {
		t.Fatalf("switched data without a session: %v", f.Calls)
	}
}

func TestRenewGivesUp(t *testing.T) {
	f := hotspottest.New()
	d := device(t, f, hotspottest.Password)
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	f.Do(func(f *hotspottest.MiFi) { f.Stuck = true })
	d.Poll = 5 * time.Millisecond
	if err := d.Renew(ctx); err == nil {
		t.Fatal("renewed a hotspot that never connected again")
	}
}
