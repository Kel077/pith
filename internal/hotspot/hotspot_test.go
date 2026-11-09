package hotspot

import (
	"context"
	"errors"
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

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestRenewRestoresDataAfterCancellationOrLostDisableReply(t *testing.T) {
	for _, lost := range []bool{false, true} {
		t.Run(map[bool]string{false: "cancel", true: "lost-reply"}[lost], func(t *testing.T) {
			f := hotspottest.New()
			d := device(t, f, hotspottest.Password)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			d.HTTP.Transport = roundTripFunc(func(req *http.Request) (*http.Response, error) {
				res, err := http.DefaultTransport.RoundTrip(req)
				if err == nil && req.URL.Path == "/wwan/disablecellulardata/" {
					if lost {
						res.Body.Close()
						return nil, errors.New("synthetic lost disable reply")
					}
					cancel()
				}
				return res, err
			})
			if err := d.Renew(ctx, nil); err == nil {
				t.Fatal("failed operation counted as success")
			}
			f.Do(func(f *hotspottest.MiFi) {
				if !f.DataOn || strings.Join(f.Calls, ",") != "off,on" {
					t.Fatal("data left disabled", f.Calls)
				}
			})
		})
	}
}

func TestModel(t *testing.T) {
	for label, want := range map[string]string{"M2000": "M2000", "Inseego MiFi M2000": "M2000", "Inseego MiFi M2100": "", "M3000": "", "MiFi 8800L": "", "iPhone USB": "", "Ethernet": ""} {
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
	calls := -1
	err := d.Renew(context.Background(), func(context.Context) error {
		time.Sleep(50 * time.Millisecond) // long after the network settings are read
		f.Do(func(f *hotspottest.MiFi) { calls = len(f.Calls) })
		return nil
	})
	if err != nil || calls != 0 {
		t.Fatalf("%v, %d calls before before returned", err, calls)
	}
	if strings.Join(f.Calls, ",") != "off,on" || !f.DataOn {
		t.Fatalf("calls %v, data on %v", f.Calls, f.DataOn)
	}
	if h, err := d.Status(context.Background()); err != nil || !h.Connected {
		t.Fatalf("%+v %v", h, err)
	}
}

func TestRenewStopsWhenBeforeFails(t *testing.T) {
	f := hotspottest.New()
	failed := errors.New("example failure")
	err := device(t, f, hotspottest.Password).Renew(context.Background(), func(context.Context) error { return failed })
	f.Do(func(f *hotspottest.MiFi) {
		if !errors.Is(err, failed) || len(f.Calls) != 0 {
			t.Fatalf("%v, calls %v", err, f.Calls)
		}
	})
}

func TestRenewNeedsThePassword(t *testing.T) {
	f := hotspottest.New()
	// Without a session, it doesn't wait for before, which it cancels.
	before := func(ctx context.Context) error { <-ctx.Done(); return ctx.Err() }
	if err := device(t, f, "").Renew(context.Background(), before); api.Code(err) != api.ErrWrongPassword {
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
	if err := d.Renew(ctx, nil); err == nil {
		t.Fatal("renewed a hotspot that never connected again")
	}
}
