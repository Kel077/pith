package router

import (
	"context"
	"errors"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/netip"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/Kel077/pith/internal/api"
	"github.com/Kel077/pith/internal/router/routertest"
)

func newRouter(t *testing.T, f *routertest.Router, password string) *Verizon {
	t.Helper()
	srv := httptest.NewTLSServer(f)
	t.Cleanup(srv.Close)
	client := srv.Client()
	jar, err := cookiejar.New(nil)
	if err != nil {
		t.Fatal(err)
	}
	client.Jar = jar
	return &Verizon{Base: srv.URL, Model: "CHR30A-R", HTTP: client, Poll: time.Millisecond,
		Password: func() (string, error) { return password, nil }}
}

func TestModel(t *testing.T) {
	for name, want := range map[string]string{"CHR30A-R": "CHR30A-R", " chr30a-r ": "CHR30A-R", "CR1000A": "", "": ""} {
		if got := Model(name); got != want {
			t.Errorf("Model(%q) = %q, want %q", name, got, want)
		}
	}
}

func TestIdentify(t *testing.T) {
	f := routertest.New("203.0.113.10")
	d := newRouter(t, f, "")
	if model, err := Identify(context.Background(), d.HTTP, d.Base); err != nil || model != "CHR30A-R" {
		t.Fatalf("Identify = %q, %v", model, err)
	}
	f.Do(func(f *routertest.Router) { f.Model = "CR1000A" })
	if _, err := Identify(context.Background(), d.HTTP, d.Base); api.Code(err) != api.ErrUnsupported {
		t.Fatalf("an untested model: %v, want unsupported", err)
	}
	other := httptest.NewTLSServer(http.NotFoundHandler())
	defer other.Close()
	if _, err := Identify(context.Background(), other.Client(), other.URL); api.Code(err) != api.ErrUnsupported {
		t.Fatalf("another web server: %v, want unsupported", err)
	}
}

func TestStatus(t *testing.T) {
	f := routertest.New("203.0.113.10")
	r, err := newRouter(t, f, routertest.Password).Status(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if r.Locked || !r.Connected || r.PublicIPv4 != "203.0.113.10" || r.Model != "CHR30A-R" {
		t.Fatalf("status = %+v", r)
	}
}

func TestLockedWithoutPassword(t *testing.T) {
	f := routertest.New("203.0.113.10")
	r, err := newRouter(t, f, "").Status(context.Background())
	if err != nil || !r.Locked || r.PublicIPv4 != "" {
		t.Fatalf("status = %+v, %v; want locked", r, err)
	}
	f.Do(func(f *routertest.Router) {
		if f.Logins != 0 {
			t.Errorf("logged in %d times without a password", f.Logins)
		}
	})
}

// A refused password is never sent again: the router would refuse logins
// for a while.
func TestRefusedPasswordOnce(t *testing.T) {
	f := routertest.New("203.0.113.10")
	d := newRouter(t, f, "wrong")
	for range 3 {
		if r, err := d.Status(context.Background()); err != nil || !r.Locked {
			t.Fatalf("status = %+v, %v; want locked", r, err)
		}
	}
	if err := d.TryPassword(context.Background(), "wrong"); api.Code(err) != api.ErrWrongPassword {
		t.Fatalf("TryPassword = %v", err)
	}
	f.Do(func(f *routertest.Router) {
		if f.Logins != 1 {
			t.Errorf("logged in %d times with a refused password, want 1", f.Logins)
		}
	})
	if err := d.TryPassword(context.Background(), routertest.Password); err != nil {
		t.Fatalf("TryPassword with the right password: %v", err)
	}
}

func TestBannedSendsNoPassword(t *testing.T) {
	f := routertest.New("203.0.113.10")
	f.Do(func(f *routertest.Router) { f.Banned = 60 })
	_, err := newRouter(t, f, routertest.Password).WAN(context.Background())
	if err == nil || !strings.Contains(err.Error(), "60 s") {
		t.Fatalf("WAN = %v; want logins refused for 60 s", err)
	}
	f.Do(func(f *routertest.Router) {
		if f.Logins != 0 {
			t.Errorf("logged in %d times while refused", f.Logins)
		}
	})
}

func TestFullSessionsIsNotAWrongPassword(t *testing.T) {
	f := routertest.New("203.0.113.10")
	f.Do(func(f *routertest.Router) { f.Full = true })
	d := newRouter(t, f, routertest.Password)
	if _, err := d.WAN(context.Background()); err == nil || api.Code(err) == api.ErrWrongPassword {
		t.Fatalf("WAN = %v; want an error other than a wrong password", err)
	}
	f.Do(func(f *routertest.Router) { f.Full = false })
	if _, err := d.WAN(context.Background()); err != nil {
		t.Fatalf("the password was taken for a wrong one: %v", err)
	}
}

func TestSessionEnds(t *testing.T) {
	f := routertest.New("203.0.113.10")
	d := newRouter(t, f, routertest.Password)
	if _, err := d.WAN(context.Background()); err != nil {
		t.Fatal(err)
	}
	f.Do(func(f *routertest.Router) { f.Expire = true })
	if _, err := d.WAN(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, err := d.WAN(context.Background()); err != nil {
		t.Fatalf("after the session ended: %v", err)
	}
	f.Do(func(f *routertest.Router) {
		if f.Logins != 2 {
			t.Errorf("logged in %d times, want 2", f.Logins)
		}
	})
}

func TestRenew(t *testing.T) {
	f := routertest.New("203.0.113.10")
	f.Do(func(f *routertest.Router) {
		f.Next = []netip.Addr{netip.MustParseAddr("203.0.113.20")}
		f.Reconnect = 3
	})
	d := newRouter(t, f, routertest.Password)
	w, err := d.Renew(context.Background(), time.Millisecond)
	if err != nil {
		t.Fatal(err)
	}
	if !w.Connected || w.IPv4 != netip.MustParseAddr("203.0.113.20") {
		t.Fatalf("renewed = %+v", w)
	}
	f.Do(func(f *routertest.Router) {
		if !slices.Equal(f.Calls, []string{"release", "renew"}) {
			t.Errorf("calls = %v", f.Calls)
		}
	})
}

func TestRenewSameAddress(t *testing.T) {
	f := routertest.New("203.0.113.10")
	w, err := newRouter(t, f, routertest.Password).Renew(context.Background(), time.Millisecond)
	if err != nil || w.IPv4 != netip.MustParseAddr("203.0.113.10") {
		t.Fatalf("renewed = %+v, %v", w, err)
	}
}

// Once it gave its address up, the router is asked for one again even when
// the renewal is canceled.
func TestRenewCanceledStillAsksAgain(t *testing.T) {
	f := routertest.New("203.0.113.10")
	d := newRouter(t, f, routertest.Password)
	ctx, cancel := context.WithCancel(context.Background())
	d.Poll = 5 * time.Millisecond
	go func() {
		for {
			done := false
			f.Do(func(f *routertest.Router) { done = len(f.Calls) > 0 })
			if done {
				cancel()
				return
			}
			time.Sleep(time.Millisecond)
		}
	}()
	_, err := d.Renew(ctx, time.Hour)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("Renew = %v, want canceled", err)
	}
	f.Do(func(f *routertest.Router) {
		if !slices.Equal(f.Calls, []string{"release", "renew"}) {
			t.Errorf("calls = %v, want release then renew", f.Calls)
		}
	})
}

func TestRenewStuck(t *testing.T) {
	f := routertest.New("203.0.113.10")
	f.Do(func(f *routertest.Router) { f.Stuck = true })
	d := newRouter(t, f, routertest.Password)
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	if _, err := d.Renew(ctx, time.Millisecond); err == nil {
		t.Fatal("a router that never connects again renewed")
	}
}

func TestClose(t *testing.T) {
	f := routertest.New("203.0.113.10")
	d := newRouter(t, f, routertest.Password)
	if _, err := d.WAN(context.Background()); err != nil {
		t.Fatal(err)
	}
	d.Close(context.Background())
	f.Do(func(f *routertest.Router) {
		if f.Requests[len(f.Requests)-1] != "/CRSP/logout" {
			t.Errorf("last request %s, want a logout", f.Requests[len(f.Requests)-1])
		}
	})
}
