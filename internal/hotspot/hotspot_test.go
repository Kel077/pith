package hotspot

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"reflect"
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
	if h.Locked || !reflect.DeepEqual(h.Cell, &want) {
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

// A hotspot slow to act on the switch still shows the connection it had
// once data is on again: Renew waits for a new one, which a test of the
// public addresses would otherwise find unchanged.
func TestRenewWaitsForANewConnection(t *testing.T) {
	f := hotspottest.New()
	old := f.Since
	f.Lag = 60
	d := device(t, f, hotspottest.Password)
	d.dropWait = 20 * time.Millisecond
	if err := d.Renew(context.Background(), nil); err != nil {
		t.Fatal(err)
	}
	f.Do(func(f *hotspottest.MiFi) {
		if f.Lag != 0 || !f.Since.After(old) {
			t.Fatalf("returned on the old connection, %d readings of it left", f.Lag)
		}
	})
}

func TestRenewCachesNetworkTokenAndRefreshesExpiredSession(t *testing.T) {
	f := hotspottest.New()
	d := device(t, f, hotspottest.Password)
	var networkReads int
	base := http.DefaultTransport
	d.HTTP.Transport = roundTripFunc(func(req *http.Request) (*http.Response, error) {
		if req.URL.Path == "/networks/" {
			networkReads++
		}
		return base.RoundTrip(req)
	})
	if err := d.Renew(context.Background(), nil); err != nil {
		t.Fatal(err)
	}
	if err := d.Renew(context.Background(), nil); err != nil {
		t.Fatal(err)
	}
	if networkReads != 1 || f.Logins != 1 {
		t.Fatalf("networkReads = %d, logins = %d; want 1, 1", networkReads, f.Logins)
	}
	// Expire the web session via diagnostics, then verify Renew re-acquires a token cleanly.
	f.Do(func(f *hotspottest.MiFi) { f.Expire = true })
	if _, err := d.Status(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := d.Renew(context.Background(), nil); err != nil {
		t.Fatal(err)
	}
	if networkReads != 2 || f.Logins != 2 {
		t.Fatalf("after expiry: networkReads = %d, logins = %d; want 2, 2", networkReads, f.Logins)
	}
}

func TestRenewRecoversFrom406StaleTokenAnd403SubmitLogin(t *testing.T) {
	f := hotspottest.New()
	d := device(t, f, hotspottest.Password)
	var rejectDisable406, rejectLogin403 bool
	base := http.DefaultTransport
	d.HTTP.Transport = roundTripFunc(func(req *http.Request) (*http.Response, error) {
		if rejectDisable406 && req.URL.Path == "/wwan/disablecellulardata/" {
			rejectDisable406 = false
			return &http.Response{
				StatusCode: http.StatusNotAcceptable,
				Status:     "406 Not Acceptable",
				Body:       io.NopCloser(strings.NewReader("")),
				Request:    req,
			}, nil
		}
		if rejectLogin403 && req.URL.Path == "/submitLogin/" {
			rejectLogin403 = false
			return &http.Response{
				StatusCode: http.StatusForbidden,
				Status:     "403 Forbidden",
				Body:       io.NopCloser(strings.NewReader("")),
				Request:    req,
			}, nil
		}
		return base.RoundTrip(req)
	})
	// Prime the cached gSecureToken.
	if err := d.Renew(context.Background(), nil); err != nil {
		t.Fatal(err)
	}
	// Simulate the real M2000 expiring its session idle: POST /wwan/disablecellulardata/
	// answers 406 Not Acceptable, and the first re-login answers 403 Forbidden.
	rejectDisable406 = true
	rejectLogin403 = true
	if err := d.Renew(context.Background(), nil); err != nil {
		t.Fatalf("Renew failed to recover from 406/403: %v", err)
	}
	f.Do(func(f *hotspottest.MiFi) {
		if !f.DataOn || strings.Join(f.Calls, ",") != "off,on,off,on" {
			t.Fatalf("unexpected data switches: %v (DataOn=%v)", f.Calls, f.DataOn)
		}
	})

	// Also verify an aged token (> tokenMaxAge) proactively refreshes /networks/.
	d.tokenAt = time.Now().Add(-2 * tokenMaxAge)
	if err := d.Renew(context.Background(), nil); err != nil {
		t.Fatal(err)
	}
}

func TestRenewReadyFinishesEarlyOnlyAfterADrop(t *testing.T) {
	f := hotspottest.New()
	d := device(t, f, hotspottest.Password)
	d.Poll = 50 * time.Millisecond
	d.HTTP.Transport = roundTripFunc(func(req *http.Request) (*http.Response, error) {
		res, err := http.DefaultTransport.RoundTrip(req)
		if err == nil && req.URL.Path == "/wwan/enablecellulardata/" {
			f.Do(func(f *hotspottest.MiFi) { f.Reconnect = 40 })
		}
		return res, err
	})
	start := time.Now()
	if err := d.RenewReady(context.Background(), nil, func(context.Context) bool {
		time.Sleep(10 * time.Millisecond)
		return true
	}); err != nil {
		t.Fatal(err)
	}
	if took := time.Since(start); took > 500*time.Millisecond {
		t.Fatalf("RenewReady waited %v for lagging /srv/status", took)
	}

	// Without an observed drop, ready must not cut past the old connection.
	f2 := hotspottest.New()
	f2.Lag = 10
	d2 := device(t, f2, hotspottest.Password)
	d2.dropWait = 5 * time.Millisecond
	readyCalled := false
	if err := d2.RenewReady(context.Background(), nil, func(context.Context) bool {
		readyCalled = true
		return true
	}); err != nil {
		t.Fatal(err)
	}
	if readyCalled {
		t.Fatal("ready ran even though the old connection had not dropped")
	}
}

// How long the connection has lasted: a string on the M2000, and unknown
// when the hotspot doesn't tell.
func TestReadConnectionDuration(t *testing.T) {
	for body, want := range map[string]time.Duration{
		`{"statusData":{"statusBarConnectionState":"Connected","statusBarConnectionDuration":"438"}}`: 438 * time.Second,
		`{"statusData":{"statusBarConnectionState":"Connected","statusBarConnectionDuration":5}}`:     5 * time.Second,
		`{"statusData":{"statusBarConnectionState":"Connected"}}`:                                     -1,
		`{"statusData":{"statusBarConnectionState":"Connected","statusBarConnectionDuration":""}}`:    -1,
	} {
		d := &Inseego{Base: "http://192.0.2.1", Model: "M2000", HTTP: &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
			return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader(body)), Request: req}, nil
		})}}
		h, lasted, err := d.read(context.Background())
		if err != nil || !h.Connected || lasted != want {
			t.Errorf("%s: %+v, %v, %v; want %v", body, h, lasted, err, want)
		}
	}
}

func TestReadMobileDataState(t *testing.T) {
	cases := map[string]mobileDataState{
		`{"statusData":{"statusBarConnectionState":"Connected","statusBarMobileDataEnabled":1}}`:         mobileDataEnabled,
		`{"statusData":{"statusBarConnectionState":"Disconnected","statusBarMobileDataEnabled":0}}`:      mobileDataDisabled,
		`{"statusData":{"statusBarConnectionState":"Connected","statusBarMobileDataEnabled":"1"}}`:       mobileDataEnabled,
		`{"statusData":{"statusBarConnectionState":"Disconnected","statusBarMobileDataEnabled":"0"}}`:    mobileDataDisabled,
		`{"statusData":{"statusBarConnectionState":"Connected"}}`:                                        mobileDataUnknown,
		`{"statusData":{"statusBarConnectionState":"Connected","statusBarMobileDataEnabled":null}}`:      mobileDataUnknown,
		`{"statusData":{"statusBarConnectionState":"Connected","statusBarMobileDataEnabled":"invalid"}}`: mobileDataUnknown,
	}
	for body, want := range cases {
		d := &Inseego{Base: "http://192.0.2.1", Model: "M2000", HTTP: &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
			return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader(body)), Request: req}, nil
		})}}
		r, err := d.readStatus(context.Background())
		if err != nil || r.mobileData != want {
			t.Errorf("%s: mobileData=%v err=%v, want %v", body, r.mobileData, err, want)
		}
	}
}

func TestRenewHealsInitiallyDisabledData(t *testing.T) {
	for _, asString := range []bool{false, true} {
		t.Run(map[bool]string{false: "int", true: "string"}[asString], func(t *testing.T) {
			f := hotspottest.New()
			f.Do(func(f *hotspottest.MiFi) {
				f.DataOn = false
				f.MobileDataAsString = asString
			})
			d := device(t, f, hotspottest.Password)
			var events []RenewEvent
			err := d.RenewReadyObserved(context.Background(), nil, nil, func(ev RenewEvent) {
				events = append(events, ev)
			})
			if err != nil {
				t.Fatalf("RenewReadyObserved failed for initially disabled device: %v", err)
			}
			f.Do(func(f *hotspottest.MiFi) {
				if !f.DataOn || strings.Join(f.Calls, ",") != "on" {
					t.Fatalf("calls = %v, DataOn = %v; want [on], true", f.Calls, f.DataOn)
				}
			})
			var skippedDisable, skippedDrop bool
			for _, ev := range events {
				if ev.Stage == "disable" && ev.Skipped {
					skippedDisable = true
				}
				if ev.Stage == "dropWait" && ev.Skipped {
					skippedDrop = true
				}
			}
			if !skippedDisable || !skippedDrop {
				t.Fatalf("expected disable and dropWait skipped events, got %+v", events)
			}
		})
	}
}

func TestRenewDisableEffectiveDespiteSessionErrorRestoresData(t *testing.T) {
	for _, tc := range []struct {
		name        string
		reply406    bool
		cancelAfter bool
	}{
		{name: "406-continues-to-enable", reply406: true, cancelAfter: false},
		{name: "gIsLoggedIn0-continues-to-enable", reply406: false, cancelAfter: false},
		{name: "406-canceled-defer-restores", reply406: true, cancelAfter: true},
		{name: "gIsLoggedIn0-canceled-defer-restores", reply406: false, cancelAfter: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := hotspottest.New()
			d := device(t, f, hotspottest.Password)
			d.restoreTimeout = 2 * time.Second
			d.restoreBackoff = 5 * time.Millisecond
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()

			spoofOnce := true
			base := http.DefaultTransport
			d.HTTP.Transport = roundTripFunc(func(req *http.Request) (*http.Response, error) {
				res, err := base.RoundTrip(req)
				if err == nil && spoofOnce && req.URL.Path == "/wwan/disablecellulardata/" {
					spoofOnce = false
					res.Body.Close()
					if tc.cancelAfter {
						cancel()
					}
					if tc.reply406 {
						return &http.Response{
							StatusCode: http.StatusNotAcceptable,
							Status:     "406 Not Acceptable",
							Body:       io.NopCloser(strings.NewReader("")),
							Request:    req,
						}, nil
					}
					return &http.Response{
						StatusCode: http.StatusOK,
						Status:     "200 OK",
						Body:       io.NopCloser(strings.NewReader(`{"gIsLoggedIn":0}`)),
						Request:    req,
					}, nil
				}
				return res, err
			})

			err := d.Renew(ctx, nil)
			if tc.cancelAfter && err == nil {
				t.Fatal("expected cancellation error")
			}
			if !tc.cancelAfter && err != nil {
				t.Fatalf("Renew failed: %v", err)
			}
			f.Do(func(f *hotspottest.MiFi) {
				if !f.DataOn || strings.Join(f.Calls, ",") != "off,on" {
					t.Fatalf("calls = %v, DataOn = %v; want [off on], true", f.Calls, f.DataOn)
				}
			})
		})
	}
}

func TestRenewDisableRejectedWithDataStillOnDoesNotLoopOrMisbehave(t *testing.T) {
	t.Run("recovers-on-retry", func(t *testing.T) {
		f := hotspottest.New()
		d := device(t, f, hotspottest.Password)
		rejectOnce := true
		base := http.DefaultTransport
		d.HTTP.Transport = roundTripFunc(func(req *http.Request) (*http.Response, error) {
			if rejectOnce && req.URL.Path == "/wwan/disablecellulardata/" {
				rejectOnce = false
				return &http.Response{
					StatusCode: http.StatusNotAcceptable,
					Status:     "406 Not Acceptable",
					Body:       io.NopCloser(strings.NewReader("")),
					Request:    req,
				}, nil
			}
			return base.RoundTrip(req)
		})
		if err := d.Renew(context.Background(), nil); err != nil {
			t.Fatalf("Renew failed: %v", err)
		}
		f.Do(func(f *hotspottest.MiFi) {
			if !f.DataOn || strings.Join(f.Calls, ",") != "off,on" {
				t.Fatalf("calls = %v, DataOn = %v; want [off on], true", f.Calls, f.DataOn)
			}
		})
	})

	t.Run("always-rejected-skips-unnecessary-enable-when-status-confirms-on", func(t *testing.T) {
		f := hotspottest.New()
		d := device(t, f, hotspottest.Password)
		disableAttempts := 0
		base := http.DefaultTransport
		d.HTTP.Transport = roundTripFunc(func(req *http.Request) (*http.Response, error) {
			if req.URL.Path == "/wwan/disablecellulardata/" {
				disableAttempts++
				return &http.Response{
					StatusCode: http.StatusNotAcceptable,
					Status:     "406 Not Acceptable",
					Body:       io.NopCloser(strings.NewReader("")),
					Request:    req,
				}, nil
			}
			return base.RoundTrip(req)
		})
		if err := d.Renew(context.Background(), nil); err == nil {
			t.Fatal("expected error when disable is always rejected")
		}
		if disableAttempts != 2 {
			t.Fatalf("disableAttempts = %d, want 2", disableAttempts)
		}
		f.Do(func(f *hotspottest.MiFi) {
			if !f.DataOn || len(f.Calls) != 0 {
				t.Fatalf("calls = %v, DataOn = %v; want [], true", f.Calls, f.DataOn)
			}
		})
	})

	t.Run("unknown-mobile-data-forces-restore", func(t *testing.T) {
		f := hotspottest.New()
		f.Do(func(f *hotspottest.MiFi) { f.OmitMobileData = true })
		d := device(t, f, hotspottest.Password)
		d.restoreTimeout = 2 * time.Second
		d.restoreBackoff = 5 * time.Millisecond
		base := http.DefaultTransport
		d.HTTP.Transport = roundTripFunc(func(req *http.Request) (*http.Response, error) {
			if req.URL.Path == "/wwan/disablecellulardata/" {
				return &http.Response{
					StatusCode: http.StatusNotAcceptable,
					Status:     "406 Not Acceptable",
					Body:       io.NopCloser(strings.NewReader("")),
					Request:    req,
				}, nil
			}
			return base.RoundTrip(req)
		})
		if err := d.Renew(context.Background(), nil); err == nil {
			t.Fatal("expected error when disable fails")
		}
		f.Do(func(f *hotspottest.MiFi) {
			if !f.DataOn || strings.Join(f.Calls, ",") != "on" {
				t.Fatalf("calls = %v, DataOn = %v; want [on], true", f.Calls, f.DataOn)
			}
		})
	})
}

func TestRenewEnableFailureAndInitialTokenRefreshFailureDoesNotSendStaleToken(t *testing.T) {
	f := hotspottest.New()
	d := device(t, f, hotspottest.Password)
	d.restoreTimeout = 2 * time.Second
	d.restoreBackoff = 5 * time.Millisecond

	var (
		failEnableOnce      = true
		failNextNetworkRead = false
		enablePosts         int
		networkReads        int
	)
	base := http.DefaultTransport
	d.HTTP.Transport = roundTripFunc(func(req *http.Request) (*http.Response, error) {
		switch req.URL.Path {
		case "/wwan/enablecellulardata/":
			enablePosts++
			if failEnableOnce {
				failEnableOnce = false
				failNextNetworkRead = true
				return &http.Response{
					StatusCode: http.StatusNotAcceptable,
					Status:     "406 Not Acceptable",
					Body:       io.NopCloser(strings.NewReader("")),
					Request:    req,
				}, nil
			}
		case "/networks/":
			networkReads++
			if failNextNetworkRead {
				failNextNetworkRead = false
				return nil, errors.New("synthetic transient /networks/ failure")
			}
		}
		return base.RoundTrip(req)
	})

	if err := d.Renew(context.Background(), nil); err != nil {
		t.Fatalf("Renew did not recover after enable + token refresh failure: %v", err)
	}
	// Only two enable POSTs: the initial rejected one and the one after the
	// second token refresh succeeded. The failed token refresh round must not
	// send a stale token POST.
	if enablePosts != 2 {
		t.Fatalf("enablePosts = %d, want 2 (stale token must not be resent)", enablePosts)
	}
	if networkReads != 3 {
		t.Fatalf("networkReads = %d, want 3 (initial + 1 failed refresh + 1 succeeded refresh)", networkReads)
	}
	f.Do(func(f *hotspottest.MiFi) {
		if !f.DataOn || strings.Join(f.Calls, ",") != "off,on" {
			t.Fatalf("calls = %v, DataOn = %v; want [off on], true", f.Calls, f.DataOn)
		}
	})
}

func TestRenewEnableLostReplyConfirmedViaStatus(t *testing.T) {
	f := hotspottest.New()
	d := device(t, f, hotspottest.Password)
	d.restoreTimeout = 2 * time.Second
	d.restoreBackoff = 5 * time.Millisecond

	enablePosts := 0
	base := http.DefaultTransport
	d.HTTP.Transport = roundTripFunc(func(req *http.Request) (*http.Response, error) {
		res, err := base.RoundTrip(req)
		if err == nil && req.URL.Path == "/wwan/enablecellulardata/" {
			enablePosts++
			res.Body.Close()
			return nil, errors.New("synthetic EOF after enable took effect")
		}
		return res, err
	})

	if err := d.Renew(context.Background(), nil); err != nil {
		t.Fatalf("Renew should confirm enable via /srv/status: %v", err)
	}
	if enablePosts != 1 {
		t.Fatalf("enablePosts = %d, want 1", enablePosts)
	}
	f.Do(func(f *hotspottest.MiFi) {
		if !f.DataOn || strings.Join(f.Calls, ",") != "off,on" {
			t.Fatalf("calls = %v, DataOn = %v; want [off on], true", f.Calls, f.DataOn)
		}
	})
}

func TestLoginTransientOrSlotOverflowDoesNotBlacklistPassword(t *testing.T) {
	cases := []struct {
		name     string
		override func(req *http.Request, base http.RoundTripper) (*http.Response, error, bool)
	}{
		{
			name: "slot-overflow-500",
			override: func(req *http.Request, _ http.RoundTripper) (*http.Response, error, bool) {
				if req.URL.Path == "/submitLogin/" {
					return &http.Response{
						StatusCode: http.StatusInternalServerError,
						Status:     "500 Internal Server Error",
						Body:       io.NopCloser(strings.NewReader(`{"gIsLoggedIn":0,"gPageId":"login","authError":"login_db_overflow_status","noOfRemainingLoginAttempts":5}`)),
						Request:    req,
					}, nil, true
				}
				return nil, nil, false
			},
		},
		{
			name: "server-500-no-auth-error",
			override: func(req *http.Request, _ http.RoundTripper) (*http.Response, error, bool) {
				if req.URL.Path == "/submitLogin/" {
					return &http.Response{
						StatusCode: http.StatusInternalServerError,
						Status:     "500 Internal Server Error",
						Body:       io.NopCloser(strings.NewReader(`internal error`)),
						Request:    req,
					}, nil, true
				}
				return nil, nil, false
			},
		},
		{
			name: "200-but-session-not-established",
			override: func(req *http.Request, _ http.RoundTripper) (*http.Response, error, bool) {
				if req.URL.Path == "/submitLogin/" {
					return &http.Response{
						StatusCode: http.StatusOK,
						Status:     "200 OK",
						Body:       io.NopCloser(strings.NewReader(`{"gIsLoggedIn":0,"gPageId":"login"}`)),
						Request:    req,
					}, nil, true
				}
				return nil, nil, false
			},
		},
		{
			name: "corrupt-status-json-after-login",
			override: func(req *http.Request, base http.RoundTripper) (*http.Response, error, bool) {
				if req.URL.Path == "/srv/status" {
					res, err := base.RoundTrip(req)
					if err != nil {
						return nil, err, true
					}
					res.Body.Close()
					return &http.Response{
						StatusCode: http.StatusOK,
						Status:     "200 OK",
						Body:       io.NopCloser(strings.NewReader(`{truncated`)),
						Request:    req,
					}, nil, true
				}
				return nil, nil, false
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := hotspottest.New()
			d := device(t, f, hotspottest.Password)
			d.diagCooldown = 20 * time.Millisecond
			inject := true
			base := http.DefaultTransport
			d.HTTP.Transport = roundTripFunc(func(req *http.Request) (*http.Response, error) {
				if inject {
					if res, err, handled := tc.override(req, base); handled {
						return res, err
					}
				}
				return base.RoundTrip(req)
			})

			err := d.TryPassword(context.Background(), hotspottest.Password)
			if err == nil {
				t.Fatal("expected transient login failure")
			}
			if api.Code(err) == api.ErrWrongPassword {
				t.Fatalf("transient error misclassified as ErrWrongPassword: %v", err)
			}
			if d.refused != "" {
				t.Fatalf("d.refused polluted by transient failure: %q", d.refused)
			}
			// Once the transient condition clears, the same password logs in cleanly.
			inject = false
			if err := d.TryPassword(context.Background(), hotspottest.Password); err != nil {
				t.Fatalf("subsequent login with valid password failed: %v", err)
			}
		})
	}

	// Verify Status() backs off /diagnostics/ polling after slot overflow instead
	// of creating a fresh unauthenticated session on every poll.
	t.Run("status-backs-off-after-slot-overflow", func(t *testing.T) {
		f := hotspottest.New()
		d := device(t, f, hotspottest.Password)
		d.diagCooldown = 50 * time.Millisecond
		loginPosts := 0
		overflow := true
		base := http.DefaultTransport
		d.HTTP.Transport = roundTripFunc(func(req *http.Request) (*http.Response, error) {
			if overflow && req.URL.Path == "/submitLogin/" {
				loginPosts++
				return &http.Response{
					StatusCode: http.StatusInternalServerError,
					Status:     "500 Internal Server Error",
					Body:       io.NopCloser(strings.NewReader(`{"gIsLoggedIn":0,"authError":"login_db_overflow_status","noOfRemainingLoginAttempts":5}`)),
					Request:    req,
				}, nil
			}
			return base.RoundTrip(req)
		})
		if _, err := d.Status(context.Background()); err == nil || api.Code(err) == api.ErrWrongPassword {
			t.Fatalf("expected retryable overflow error, got %v", err)
		}
		if loginPosts != 1 {
			t.Fatalf("slot overflow retried login immediately: loginPosts = %d, want 1", loginPosts)
		}
		// Immediate follow-up poll during cooldown returns /srv/status without calling /submitLogin/.
		h, err := d.Status(context.Background())
		if err != nil || !h.Connected || h.Locked {
			t.Fatalf("cooldown Status() = %+v, %v; want connected, unlocked, nil", h, err)
		}
		if loginPosts != 1 {
			t.Fatalf("cooldown poll still hit /submitLogin/: loginPosts = %d, want 1", loginPosts)
		}
		// After cooldown and overflow clearing, Status() reads cell info again.
		overflow = false
		time.Sleep(60 * time.Millisecond)
		h, err = d.Status(context.Background())
		if err != nil || h.Cell == nil || h.Cell.Band != "n71" {
			t.Fatalf("post-cooldown Status() = %+v, %v", h, err)
		}
	})
}

func TestRenewRestoreTotalFailureReturnsClearWarning(t *testing.T) {
	f := hotspottest.New()
	d := device(t, f, hotspottest.Password)
	d.restoreTimeout = 80 * time.Millisecond
	d.restoreBackoff = 5 * time.Millisecond

	base := http.DefaultTransport
	d.HTTP.Transport = roundTripFunc(func(req *http.Request) (*http.Response, error) {
		if req.URL.Path == "/wwan/enablecellulardata/" {
			return &http.Response{
				StatusCode: http.StatusInternalServerError,
				Status:     "500 Internal Server Error",
				Body:       io.NopCloser(strings.NewReader(`{"error":"failed"}`)),
				Request:    req,
			}, nil
		}
		return base.RoundTrip(req)
	})

	err := d.Renew(context.Background(), nil)
	if err == nil {
		t.Fatal("expected error when cellular data cannot be re-enabled")
	}
	msg := err.Error()
	if !strings.Contains(msg, "cellular data switch may still be disabled") || !strings.Contains(msg, "reboot") {
		t.Fatalf("expected clear warning that cellular data may remain disabled across reboot, got: %v", err)
	}
	f.Do(func(f *hotspottest.MiFi) {
		if f.DataOn {
			t.Fatal("expected device DataOn to remain false in total failure fixture")
		}
	})
}

func TestSanitizeErrorRedactsSecrets(t *testing.T) {
	err := errors.New("POST /submitLogin/ failed: gSecureToken=0123456789abcdef0123456789abcdef01234567&shaPassword=deadbeef Cookie: mifisess=abcdef123456\npassword=secret-pass")
	got := SanitizeError(err)
	for _, leaked := range []string{"0123456789abcdef0123456789abcdef01234567", "deadbeef", "abcdef123456", "secret-pass", "\n"} {
		if strings.Contains(got, leaked) {
			t.Fatalf("SanitizeError leaked %q in %q", leaked, got)
		}
	}
}
