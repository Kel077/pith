// Package hotspot controls cellular connections through the M2000 web admin
// and the Quectel RM520N-GL's local RMNet command-line tool.
package hotspot

import (
	"context"
	"crypto/sha1"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"html"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/Kel077/pith/internal/api"
)

// Model identifies an Inseego web admin by its interface label.
func Model(label string) string {
	if strings.Contains(strings.ToLower(label), "m2000") {
		return "M2000"
	}
	return ""
}

// Inseego is the web admin of an Inseego MiFi hotspot.
type Inseego struct {
	Base  string       // its address, from the interface: "http://192.0.2.1"
	Model string       // M2000
	HTTP  *http.Client // through the hotspot's interface, keeping cookies
	// Password returns the admin password Pith keeps, or "" when it has none.
	Password func() (string, error)
	// Poll is how long to wait between readings while renewing: 250 ms
	// when zero.
	Poll time.Duration

	dropWait       time.Duration // for its connection to drop once data is off: 10 s when zero
	restoreTimeout time.Duration // independent budget for ensureDataEnabled: 60 s when zero
	restoreBackoff time.Duration // initial retry delay in ensureDataEnabled: 1 s when zero
	diagCooldown   time.Duration // pause before retrying /diagnostics/ after slot overflow: 30 s when zero

	mu               sync.Mutex
	loggedIn         bool
	token            string    // gSecureToken of the current web session
	tokenAt          time.Time // when token was last read or accepted
	refused          string    // a hash of the password the hotspot refused: never tried again
	diagBackoffUntil time.Time // skip /diagnostics/ polling until this time after slot-full/session errors
}

// pageLimit bounds what Pith reads of a page of the web admin.
const (
	pageLimit             = 1 << 20
	tokenMaxAge           = 90 * time.Second
	defaultRestoreTimeout = 60 * time.Second
	defaultRestoreBackoff = time.Second
	maxRestoreBackoff     = 8 * time.Second
	defaultDiagCooldown   = 30 * time.Second
	statusCheckTimeout    = 5 * time.Second
)

type mobileDataState int8

const (
	mobileDataUnknown mobileDataState = iota
	mobileDataDisabled
	mobileDataEnabled
)

func (s mobileDataState) String() string {
	switch s {
	case mobileDataDisabled:
		return "disabled"
	case mobileDataEnabled:
		return "enabled"
	default:
		return "unknown"
	}
}

// RenewEvent describes one stage of a hotspot renewal for diagnostic logging.
// It never contains passwords, session tokens or cookies.
type RenewEvent struct {
	Stage      string
	At         time.Time
	Duration   time.Duration
	MobileData string
	Connected  *bool
	Attempt    int
	Skipped    bool
	Detail     string
	Error      string
}

type statusReading struct {
	hotspot    api.Hotspot
	lasted     time.Duration
	login      int // 0, 1, or -1 when omitted/unparsed
	mobileData mobileDataState
	identified bool
}

var errStatusFormat = errors.New("invalid status JSON")

// Identify checks the public Inseego status protocol without logging in.
// Other gateways can also serve /srv/status, so a successful HTTP request
// alone does not identify an M2000-compatible admin.
func (d *Inseego) Identify(ctx context.Context) error {
	r, err := d.readStatus(ctx)
	var status httpStatusError
	switch {
	case err == nil && r.identified:
		return nil
	case err == nil, errors.Is(err, errStatusFormat), errors.Is(err, syscall.ECONNREFUSED), errors.As(err, &status) && status.code < 500:
		return &api.Error{Code: api.ErrUnsupported, Detail: "the gateway does not speak the Inseego M2000 status protocol"}
	default:
		return err
	}
}

// Status reads the hotspot: its connection, and its cell when Pith has its
// admin password.
func (d *Inseego) Status(ctx context.Context) (api.Hotspot, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	r, err := d.readStatus(ctx)
	if err != nil {
		return r.hotspot, err
	}
	if r.login == 0 && d.loggedIn {
		d.clearSession()
	}
	// After a session-slot overflow or transient session failure, keep serving
	// /srv/status without repeatedly creating unauthenticated sessions every poll.
	if !d.loggedIn && !d.diagBackoffUntil.IsZero() && time.Now().Before(d.diagBackoffUntil) {
		return r.hotspot, nil
	}
	page, err := d.adminPage(ctx, "/diagnostics/", func(p string) bool { return strings.Contains(p, `id="band"`) })
	switch {
	case api.Code(err) == api.ErrWrongPassword:
		r.hotspot.Locked = true
		return r.hotspot, nil
	case err != nil:
		if errors.Is(err, errSessionOverflow) || errors.Is(err, errSession) {
			d.setDiagBackoff()
		}
		return r.hotspot, err
	}
	d.diagBackoffUntil = time.Time{}
	r.hotspot.Cell = parseCell(page)
	return r.hotspot, nil
}

func (d *Inseego) setDiagBackoff() {
	cd := d.diagCooldown
	if cd == 0 {
		cd = defaultDiagCooldown
	}
	d.diagBackoffUntil = time.Now().Add(cd)
}

// TryPassword logs in with password: ErrWrongPassword when the hotspot
// refuses it.
func (d *Inseego) TryPassword(ctx context.Context, password string) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.clearSession()
	if password == "" || digest(password) == d.refused {
		return &api.Error{Code: api.ErrWrongPassword, Detail: "the hotspot refused this admin password"}
	}
	return d.loginWith(ctx, password)
}

func (d *Inseego) clearSession() {
	d.loggedIn, d.token, d.tokenAt = false, "", time.Time{}
}

func (d *Inseego) clearCookies() {
	if d.HTTP == nil || d.HTTP.Jar == nil {
		return
	}
	u, err := url.Parse(d.Base + "/")
	if err != nil {
		return
	}
	for _, c := range d.HTTP.Jar.Cookies(u) {
		expired := *c
		expired.Value = ""
		expired.MaxAge = -1
		expired.Expires = time.Unix(1, 0)
		d.HTTP.Jar.SetCookies(u, []*http.Cookie{&expired})
	}
}

// Renew has the hotspot reconnect to its carrier: it turns its cellular
// data off and on, and waits until it is connected again, by a connection
// made since data went off when the hotspot tells. before, if not
// nil, runs while Renew prepares the session: Renew turns data off once
// before has returned, and not at all if it failed.
func (d *Inseego) Renew(ctx context.Context, before func(context.Context) error) error {
	return d.RenewReady(ctx, before, nil)
}

// RenewReady is Renew with an optional readiness check that runs alongside
// status polling once the previous connection has been seen to drop.
func (d *Inseego) RenewReady(ctx context.Context, before func(context.Context) error, ready func(context.Context) bool) error {
	return d.RenewReadyObserved(ctx, before, ready, nil)
}

// RenewReadyObserved is RenewReady with an optional callback that receives
// sanitized stage events for diagnostic logging.
func (d *Inseego) RenewReadyObserved(ctx context.Context, before func(context.Context) error, ready func(context.Context) bool, onEvent func(RenewEvent)) (resultErr error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	emit := func(ev RenewEvent) {
		if onEvent == nil {
			return
		}
		if ev.At.IsZero() {
			ev.At = time.Now()
		}
		onEvent(ev)
	}

	type beforeResult struct {
		dur time.Duration
		err error
	}
	beforeCtx, stop := context.WithCancel(ctx)
	defer stop()
	beforeStart := time.Now()
	beforeDone := make(chan beforeResult, 1)
	if before == nil {
		beforeDone <- beforeResult{}
	} else {
		go func() {
			s := time.Now()
			err := before(beforeCtx)
			beforeDone <- beforeResult{dur: time.Since(s), err: err}
		}()
	}

	// Read /srv/status without logging in while before runs: it tells whether
	// cellular data is already disabled and whether the cached session ended.
	statusStart := time.Now()
	initialStatus, statusErr := d.readStatus(ctx)
	if statusErr == nil {
		if initialStatus.login == 0 && d.loggedIn {
			d.clearSession()
		}
		conn := initialStatus.hotspot.Connected
		emit(RenewEvent{
			Stage:      "status",
			At:         statusStart,
			Duration:   time.Since(statusStart),
			MobileData: initialStatus.mobileData.String(),
			Connected:  &conn,
		})
	} else {
		emit(RenewEvent{
			Stage:      "status",
			At:         statusStart,
			Duration:   time.Since(statusStart),
			MobileData: mobileDataUnknown.String(),
			Error:      SanitizeError(statusErr),
		})
	}

	tokStart := time.Now()
	tok, err := d.sessionToken(ctx)
	emit(RenewEvent{
		Stage:    "token",
		At:       tokStart,
		Duration: time.Since(tokStart),
		Error:    SanitizeError(err),
	})
	if err != nil {
		stop()
	}
	beforeRes := <-beforeDone
	if before != nil {
		emit(RenewEvent{
			Stage:    "before",
			At:       beforeStart,
			Duration: beforeRes.dur,
			Error:    SanitizeError(beforeRes.err),
		})
	}
	if err == nil {
		err = beforeRes.err
	}
	if err != nil {
		return err
	}

	form := url.Values{"gSecureToken": {tok}}
	// A canceled or failed disable request may still have reached the device
	// (which persists CellularDataEnabled=0 to flash). Always try to restore
	// data with an independent bounded context unless /srv/status explicitly
	// confirms cellular data is still enabled.
	needsEnable := true
	defer func() {
		if !needsEnable {
			return
		}
		restoreCtx, cancel := context.WithTimeout(context.Background(), d.restoreBudget())
		defer cancel()
		if restoreErr := d.ensureDataEnabled(restoreCtx, nil, form, "restore", true, emit); restoreErr != nil {
			resultErr = errors.Join(resultErr, restoreErr)
		}
	}()

	off := time.Now()
	var sawDrop bool
	if statusErr == nil && initialStatus.mobileData == mobileDataDisabled {
		// Cellular data is already disabled on the device: skip disable and
		// dropWait, and bring cellular data straight back up.
		emit(RenewEvent{Stage: "disable", MobileData: mobileDataDisabled.String(), Skipped: true, Detail: "already-disabled"})
		emit(RenewEvent{Stage: "dropWait", MobileData: mobileDataDisabled.String(), Skipped: true, Detail: "already-disabled"})
		sawDrop = !initialStatus.hotspot.Connected
	} else {
		disableStart := time.Now()
		off = disableStart
		err = d.post(ctx, "/wwan/disablecellulardata/", "/networks/", form)
		if errors.Is(err, errSession) {
			check, checkErr := d.readStatusBounded(statusCheckTimeout)
			if checkErr == nil && check.mobileData == mobileDataDisabled {
				// Disable already switched cellular data off despite the session
				// error reply; proceed to dropWait/enable with a fresh token.
				emit(RenewEvent{
					Stage:      "disable",
					At:         disableStart,
					Duration:   time.Since(disableStart),
					MobileData: check.mobileData.String(),
					Detail:     "confirmed-disabled-after-session-error",
					Error:      SanitizeError(err),
				})
				err = nil
				form = nil
			} else if ctx.Err() == nil {
				// The cached session or gSecureToken expired while idle (401/403/406
				// or gIsLoggedIn=0). Refresh and retry disable once.
				retryTokStart := time.Now()
				fresh, tokErr := d.sessionToken(ctx)
				emit(RenewEvent{
					Stage:    "token",
					At:       retryTokStart,
					Duration: time.Since(retryTokStart),
					Attempt:  2,
					Error:    SanitizeError(tokErr),
				})
				if tokErr != nil {
					if d.confirmMobileDataEnabled() {
						needsEnable = false
					}
					emit(RenewEvent{
						Stage:      "disable",
						At:         disableStart,
						Duration:   time.Since(disableStart),
						MobileData: check.mobileData.String(),
						Error:      SanitizeError(tokErr),
					})
					return tokErr
				}
				form = url.Values{"gSecureToken": {fresh}}
				off = time.Now()
				err = d.post(ctx, "/wwan/disablecellulardata/", "/networks/", form)
			}
		}
		if err != nil {
			var obs mobileDataState
			if errors.Is(err, errSession) {
				// Only skip recovery when /srv/status explicitly confirms cellular
				// data is still enabled. Disabled, unknown or failed status reads
				// must keep needsEnable = true.
				if check, checkErr := d.readStatusBounded(statusCheckTimeout); checkErr == nil {
					obs = check.mobileData
					if check.mobileData == mobileDataEnabled {
						needsEnable = false
					}
				}
			}
			emit(RenewEvent{
				Stage:      "disable",
				At:         disableStart,
				Duration:   time.Since(disableStart),
				MobileData: obs.String(),
				Error:      SanitizeError(err),
			})
			return fmt.Errorf("hotspot: turn cellular data off: %w", err)
		}
		if form != nil {
			emit(RenewEvent{Stage: "disable", At: disableStart, Duration: time.Since(disableStart)})
		}

		// Let it drop its connection: some take a moment, and turning data on
		// before that would leave the old connection in place.
		dropStart := time.Now()
		dropWait := d.dropWait
		if dropWait == 0 {
			dropWait = 10 * time.Second
		}
		sawDrop = d.await(ctx, dropWait, func(h api.Hotspot, _ time.Duration) bool { return !h.Connected })
		dropDetail := "timeout"
		if sawDrop {
			dropDetail = "dropped"
		}
		emit(RenewEvent{Stage: "dropWait", At: dropStart, Duration: time.Since(dropStart), Detail: dropDetail})
	}

	enableCtx, cancelEnable := context.WithTimeout(ctx, d.restoreBudget())
	err = d.ensureDataEnabled(enableCtx, ctx, form, "enable", false, emit)
	cancelEnable()
	if err != nil {
		// If the caller's context is still active, ensureDataEnabled already
		// spent the full bounded retry budget and verified /srv/status; avoid
		// waiting a second full budget in the defer. If ctx was canceled or
		// expired, keep needsEnable = true so the defer retries under an
		// independent context.
		if ctx.Err() == nil {
			needsEnable = false
		}
		return fmt.Errorf("hotspot: turn cellular data on: %w", err)
	}
	needsEnable = false

	// Connected, by a connection made since data went off. One slow to act
	// on the switch may still show the connection it had, through which a
	// test would find the addresses from before, as if they hadn't changed.
	// Its status tells in whole seconds how long its connection has lasted;
	// one that doesn't is taken at its word. Once a drop has been observed,
	// ready can finish early when /srv/status lags behind actual traffic.
	reconnectStart := time.Now()
	waitCtx, stopWait := context.WithCancel(ctx)
	var readyHit atomic.Bool
	var wg sync.WaitGroup
	if sawDrop && ready != nil {
		wg.Go(func() {
			if ready(waitCtx) {
				readyHit.Store(true)
				stopWait()
			}
		})
	}
	stale := false
	connected := d.await(waitCtx, 90*time.Second, func(h api.Hotspot, lasted time.Duration) bool {
		stale = h.Connected && lasted > time.Since(off)+time.Second
		return h.Connected && !stale
	})
	stopWait()
	wg.Wait()
	if !connected && !readyHit.Load() {
		var recErr error
		switch {
		case ctx.Err() != nil:
			recErr = ctx.Err()
		case stale:
			recErr = errors.New("hotspot: still on the connection it had before, after 90 s")
		default:
			recErr = errors.New("hotspot: not connected again after 90 s")
		}
		emit(RenewEvent{Stage: "reconnect", At: reconnectStart, Duration: time.Since(reconnectStart), Error: SanitizeError(recErr)})
		return recErr
	}
	detail := "status-connected"
	if readyHit.Load() {
		detail = "ready-probe"
	}
	connTrue := true
	emit(RenewEvent{Stage: "reconnect", At: reconnectStart, Duration: time.Since(reconnectStart), Connected: &connTrue, Detail: detail})
	return nil
}

func (d *Inseego) restoreBudget() time.Duration {
	if d.restoreTimeout > 0 {
		return d.restoreTimeout
	}
	return defaultRestoreTimeout
}

func (d *Inseego) initialRestoreBackoff() time.Duration {
	if d.restoreBackoff > 0 {
		return d.restoreBackoff
	}
	return defaultRestoreBackoff
}

func (d *Inseego) readStatusBounded(limit time.Duration) (statusReading, error) {
	ctx, cancel := context.WithTimeout(context.Background(), limit)
	defer cancel()
	return d.readStatus(ctx)
}

func (d *Inseego) confirmMobileDataEnabled() bool {
	r, err := d.readStatusBounded(statusCheckTimeout)
	return err == nil && r.mobileData == mobileDataEnabled
}

// ensureDataEnabled turns cellular data back on with bounded backoff retries,
// refreshing session credentials cleanly on failure and confirming via
// /srv/status so a lost reply after an effective switch still succeeds.
func (d *Inseego) ensureDataEnabled(ctx, callerCtx context.Context, form url.Values, stage string, checkFirst bool, emit func(RenewEvent)) error {
	backoff := d.initialRestoreBackoff()
	maxBackoff := max(maxRestoreBackoff, backoff)
	if d.restoreBackoff > 0 {
		maxBackoff = min(maxRestoreBackoff, 8*backoff)
	}
	needRefresh := form == nil || form.Get("gSecureToken") == ""
	var lastErr error
	var lastMobileData mobileDataState

	for attempt := 1; ; attempt++ {
		attemptStart := time.Now()
		if ctx.Err() != nil {
			if lastErr == nil {
				lastErr = ctx.Err()
			}
			break
		}
		if checkFirst || attempt > 1 {
			r, err := d.readStatus(ctx)
			if err == nil {
				lastMobileData = r.mobileData
				if r.mobileData == mobileDataEnabled {
					emit(RenewEvent{
						Stage:      stage,
						At:         attemptStart,
						Duration:   time.Since(attemptStart),
						MobileData: r.mobileData.String(),
						Attempt:    attempt,
						Detail:     "confirmed-via-status",
					})
					return nil
				}
				if r.login == 0 && d.loggedIn {
					d.clearSession()
					needRefresh = true
				}
			}
		}

		if needRefresh || form == nil || form.Get("gSecureToken") == "" {
			if attempt > 1 || errors.Is(lastErr, errSession) {
				d.clearSession()
				d.clearCookies()
			}
			tokStart := time.Now()
			fresh, tokErr := d.sessionToken(ctx)
			emit(RenewEvent{
				Stage:    "token",
				At:       tokStart,
				Duration: time.Since(tokStart),
				Attempt:  attempt,
				Detail:   stage,
				Error:    SanitizeError(tokErr),
			})
			if tokErr != nil {
				lastErr = tokErr
				emit(RenewEvent{
					Stage:      stage,
					At:         attemptStart,
					Duration:   time.Since(attemptStart),
					MobileData: lastMobileData.String(),
					Attempt:    attempt,
					Detail:     "refresh-token-failed",
					Error:      SanitizeError(tokErr),
				})
				if api.Code(tokErr) == api.ErrWrongPassword {
					break
				}
				if !sleepContext(ctx, backoff) {
					break
				}
				backoff = min(backoff*2, maxBackoff)
				continue
			}
			form = url.Values{"gSecureToken": {fresh}}
			needRefresh = false
		}

		postErr := d.post(ctx, "/wwan/enablecellulardata/", "/networks/", form)
		if postErr == nil {
			emit(RenewEvent{
				Stage:      stage,
				At:         attemptStart,
				Duration:   time.Since(attemptStart),
				MobileData: mobileDataEnabled.String(),
				Attempt:    attempt,
			})
			return nil
		}
		lastErr = postErr
		// A network error or dropped reply may still have turned cellular data on.
		if r, statusErr := d.readStatus(ctx); statusErr == nil {
			lastMobileData = r.mobileData
			if r.mobileData == mobileDataEnabled {
				emit(RenewEvent{
					Stage:      stage,
					At:         attemptStart,
					Duration:   time.Since(attemptStart),
					MobileData: r.mobileData.String(),
					Attempt:    attempt,
					Detail:     "confirmed-via-status",
					Error:      SanitizeError(postErr),
				})
				return nil
			}
		}
		emit(RenewEvent{
			Stage:      stage,
			At:         attemptStart,
			Duration:   time.Since(attemptStart),
			MobileData: lastMobileData.String(),
			Attempt:    attempt,
			Error:      SanitizeError(postErr),
		})
		// Never resend a stale token after a failed enable attempt; force a clean
		// session refresh on the next round.
		needRefresh = true
		form = nil
		if !sleepContext(ctx, backoff) {
			break
		}
		backoff = min(backoff*2, maxBackoff)
	}

	// One final /srv/status check before reporting that cellular data may be off.
	if finalStatus, err := d.readStatusBounded(min(statusCheckTimeout, 3*time.Second)); err == nil && finalStatus.mobileData == mobileDataEnabled {
		emit(RenewEvent{
			Stage:      stage,
			MobileData: finalStatus.mobileData.String(),
			Detail:     "confirmed-via-status-final",
		})
		return nil
	}
	if lastErr == nil {
		lastErr = ctx.Err()
	}
	if lastErr == nil {
		lastErr = errors.New("cellular data could not be re-enabled")
	}
	// If the caller canceled during the primary enable stage, return the raw
	// error so the defer restore runs with its own independent budget before
	// reporting that cellular data may remain disabled.
	if callerCtx != nil && callerCtx.Err() != nil {
		return lastErr
	}
	return fmt.Errorf("hotspot: restore cellular data: M2000 cellular data switch may still be disabled (a reboot will not re-enable it; turn cellular data back on): %w", lastErr)
}

func sleepContext(ctx context.Context, d time.Duration) bool {
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-timer.C:
		return true
	}
}

func (d *Inseego) sessionToken(ctx context.Context) (string, error) {
	if d.loggedIn && d.token != "" && !d.tokenAt.IsZero() && time.Since(d.tokenAt) <= tokenMaxAge {
		return d.token, nil
	}
	d.token, d.tokenAt = "", time.Time{}
	page, err := d.adminPage(ctx, "/networks/", func(p string) bool { return strings.Contains(p, "/wwan/enablecellulardata/") })
	if err != nil {
		return "", err
	}
	m := tokenScript.FindStringSubmatch(page)
	if m == nil {
		return "", errors.New("hotspot: no session token on its network settings")
	}
	d.token, d.tokenAt = m[1], time.Now()
	return d.token, nil
}

// await reads the status until ok, or until limit has passed. ok is given
// how long the connection has lasted, or -1 when the hotspot doesn't say.
func (d *Inseego) await(ctx context.Context, limit time.Duration, ok func(h api.Hotspot, lasted time.Duration) bool) bool {
	poll := d.Poll
	if poll == 0 {
		poll = 250 * time.Millisecond
	}
	deadline := time.Now().Add(limit)
	for {
		if h, lasted, err := d.read(ctx); err == nil && ok(h, lasted) {
			return true
		}
		if time.Now().Add(poll).After(deadline) {
			return false
		}
		select {
		case <-ctx.Done():
			return false
		case <-time.After(poll):
		}
	}
}

// read reads the status, and how long the connection has lasted: -1 when
// the hotspot doesn't say.
func (d *Inseego) read(ctx context.Context) (api.Hotspot, time.Duration, error) {
	r, err := d.readStatus(ctx)
	return r.hotspot, r.lasted, err
}

// readStatus reads /srv/status without logging in, including the connection
// state, connection duration, loginStatus and statusBarMobileDataEnabled.
func (d *Inseego) readStatus(ctx context.Context) (statusReading, error) {
	r := statusReading{
		hotspot: api.Hotspot{Model: d.Model, At: time.Now()},
		lasted:  -1,
		login:   -1,
	}
	body, err := d.get(ctx, "/srv/status")
	if err != nil {
		return r, err
	}
	var s struct {
		LoginStatus json.RawMessage `json:"loginStatus"`
		StatusData  struct {
			Connection string          `json:"statusBarConnectionState"`
			Technology *string         `json:"statusBarTechnology"`
			Bars       json.RawMessage `json:"statusBarSignalBars"`
			Lasted     json.RawMessage `json:"statusBarConnectionDuration"` // seconds, as a string on the M2000
			MobileData json.RawMessage `json:"statusBarMobileDataEnabled"`  // 0/1 or "0"/"1" on the M2000
		} `json:"statusData"`
	}
	if err := json.Unmarshal(body, &s); err != nil {
		return r, fmt.Errorf("hotspot: %w: %w", errStatusFormat, err)
	}
	r.hotspot.Connected = s.StatusData.Connection == "Connected"
	if s.StatusData.Technology != nil {
		r.hotspot.Technology = strings.TrimSpace(*s.StatusData.Technology)
	}
	bars, barsErr := strconv.Atoi(strings.Trim(string(s.StatusData.Bars), `" `))
	r.hotspot.Bars = min(max(bars, 0), 5)
	if seconds, err := strconv.Atoi(strings.Trim(string(s.StatusData.Lasted), `" `)); err == nil && seconds >= 0 {
		r.lasted = time.Duration(seconds) * time.Second
	}
	if ls, err := strconv.Atoi(strings.Trim(string(s.LoginStatus), `" `)); err == nil {
		r.login = ls
	}
	r.mobileData = parseMobileDataState(s.StatusData.MobileData)
	r.identified = (r.login == 0 || r.login == 1) && strings.TrimSpace(s.StatusData.Connection) != "" &&
		barsErr == nil && bars >= 0 && bars <= 5 && (s.StatusData.Technology != nil || r.lasted >= 0 || r.mobileData != mobileDataUnknown)
	return r, nil
}

func parseMobileDataState(raw json.RawMessage) mobileDataState {
	if len(raw) == 0 {
		return mobileDataUnknown
	}
	v := strings.TrimSpace(string(raw))
	if v == "" || v == "null" {
		return mobileDataUnknown
	}
	if len(v) >= 2 && v[0] == '"' && v[len(v)-1] == '"' {
		var s string
		if err := json.Unmarshal(raw, &s); err != nil {
			return mobileDataUnknown
		}
		v = strings.TrimSpace(s)
	}
	switch strings.ToLower(v) {
	case "1", "true":
		return mobileDataEnabled
	case "0", "false":
		return mobileDataDisabled
	default:
		return mobileDataUnknown
	}
}

// adminPage reads a page behind the admin's login, logging in first, and
// once more if the page shows the session has ended.
func (d *Inseego) adminPage(ctx context.Context, path string, ok func(string) bool) (string, error) {
	for range 2 {
		if err := d.login(ctx); err != nil {
			return "", err
		}
		body, err := d.get(ctx, path)
		if err != nil {
			return "", err
		}
		if page := string(body); ok(page) {
			return page, nil
		}
		d.clearSession()
	}
	return "", fmt.Errorf("hotspot: %s needs a session it doesn't keep", path)
}

// login makes sure of a session with the password Pith keeps. Without one,
// or with one the hotspot refused before, it is ErrWrongPassword at once:
// trying a refused password again and again would lock the admin.
func (d *Inseego) login(ctx context.Context) error {
	if d.loggedIn {
		return nil
	}
	password, err := d.Password()
	if err != nil {
		return err
	}
	switch {
	case password == "":
		return &api.Error{Code: api.ErrWrongPassword, Detail: "no admin password"}
	case digest(password) == d.refused:
		return &api.Error{Code: api.ErrWrongPassword, Detail: "the hotspot refused the admin password"}
	}
	return d.loginWith(ctx, password)
}

func (d *Inseego) loginWith(ctx context.Context, password string) error {
	for attempt := range 2 {
		if attempt > 0 {
			d.clearCookies()
		}
		err := d.loginOnce(ctx, password)
		if err == nil {
			return nil
		}
		// Only retry once with fresh cookies when the reply indicates a stale
		// session or token mismatch. Never retry immediately on slot overflow,
		// wrong password, server 5xx or I/O errors so we do not allocate extra
		// unlogged-out session slots on the M2000.
		if attempt == 0 && ctx.Err() == nil && !errors.Is(err, errSessionOverflow) && shouldRetryLoginWithFreshCookies(err) {
			continue
		}
		return err
	}
	return nil
}

func shouldRetryLoginWithFreshCookies(err error) bool {
	var st httpStatusError
	if errors.As(err, &st) && isAuthStatus(st.code) {
		return true
	}
	return errors.Is(err, errSession)
}

type loginAuthKind uint8

const (
	loginAuthNone loginAuthKind = iota
	loginAuthWrongPassword
	loginAuthOverflow
	loginAuthStaleSession
	loginAuthTransient
)

// classifyLoginAuthError maps the M2000 /submitLogin/ authError key (from
// Login::ValidateAuthentication in the stock web UI) into wrong-password vs
// retryable session/slot errors.
func classifyLoginAuthError(authErr string) (loginAuthKind, string) {
	s := strings.TrimSpace(authErr)
	if s == "" {
		return loginAuthNone, ""
	}
	key := s
	if i := strings.IndexAny(key, " \t\r\n"); i > 0 {
		key = key[:i]
	}
	lower := strings.ToLower(s)
	switch {
	case strings.Contains(lower, "login_invalid_password_status"),
		strings.Contains(lower, "login_invalid_login_status"):
		return loginAuthWrongPassword, key
	case strings.Contains(lower, "login_db_overflow_status"),
		strings.Contains(lower, "no open slots"):
		return loginAuthOverflow, key
	case strings.Contains(lower, "login_unknown_session_status"),
		strings.Contains(lower, "login_invalid_token_status"),
		strings.Contains(lower, "login_session_expired_status"):
		return loginAuthStaleSession, key
	default:
		return loginAuthTransient, key
	}
}

func (d *Inseego) classifyLoginReply(password string, body []byte) error {
	var reply struct {
		AuthError string `json:"authError"`
	}
	if json.Unmarshal(body, &reply) != nil {
		return nil
	}
	kind, key := classifyLoginAuthError(reply.AuthError)
	switch kind {
	case loginAuthWrongPassword:
		d.refused = digest(password)
		return &api.Error{Code: api.ErrWrongPassword, Detail: "the hotspot refused the admin password"}
	case loginAuthOverflow:
		return fmt.Errorf("%w (%s)", errSessionOverflow, key)
	case loginAuthStaleSession:
		return fmt.Errorf("%w: %s", errSession, key)
	case loginAuthTransient:
		return fmt.Errorf("hotspot: login failed (%s)", key)
	default:
		return nil
	}
}

func (d *Inseego) loginOnce(ctx context.Context, password string) error {
	body, err := d.get(ctx, "/login/")
	if err != nil {
		return err
	}
	m := tokenField.FindStringSubmatch(string(body))
	if m == nil {
		return errors.New("hotspot: no token on its login page")
	}
	// As its page does: SHA-1 of the password and the token, never the password.
	sum := sha1.Sum([]byte(password + m[1]))
	replyBody, err := d.submit(ctx, "/submitLogin/", "/login/", url.Values{"shaPassword": {hex.EncodeToString(sum[:])}, "gSecureToken": {m[1]}})
	if err != nil {
		var st httpStatusError
		if errors.As(err, &st) && len(st.body) > 0 {
			if authErr := d.classifyLoginReply(password, st.body); authErr != nil {
				return authErr
			}
		}
		return err
	}
	if authErr := d.classifyLoginReply(password, replyBody); authErr != nil {
		return authErr
	}
	var reply map[string]json.RawMessage
	if err := json.Unmarshal(replyBody, &reply); err != nil {
		return fmt.Errorf("hotspot: read login reply: %w", err)
	}
	// Its reply tells the state of the login page, logged out, whatever the
	// result: /srv/status tells whether the session is logged in now.
	statusBody, err := d.get(ctx, "/srv/status")
	if err != nil {
		return err
	}
	var st struct {
		LoginStatus int `json:"loginStatus"`
	}
	if err := json.Unmarshal(statusBody, &st); err != nil {
		return fmt.Errorf("hotspot: read login status: %w", err)
	}
	if st.LoginStatus != 1 {
		// /submitLogin/ did not report an invalid password, yet /srv/status says
		// the session is not logged in (for example, the session slot was evicted
		// or a transient login failure occurred). Never blacklist the password.
		d.clearSession()
		return fmt.Errorf("%w: login did not establish a session", errSession)
	}
	d.loggedIn = true
	d.diagBackoffUntil = time.Time{}
	return nil
}

func (d *Inseego) get(ctx context.Context, path string) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, d.Base+path, nil)
	if err != nil {
		return nil, err
	}
	return d.do(req)
}

var (
	// errSession: the reply of the web admin says its session is not logged in.
	errSession = errors.New("hotspot: not logged in")
	// errSessionOverflow: the M2000 session table had no open slot.
	errSessionOverflow = errors.New("hotspot: web admin session slots full")
)

// submit sends a form as the web admin's own scripts do.
func (d *Inseego) submit(ctx context.Context, path, from string, form url.Values) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, d.Base+path, strings.NewReader(form.Encode()))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded; charset=UTF-8")
	req.Header.Set("X-Requested-With", "XMLHttpRequest")
	req.Header.Set("Referer", d.Base+from)
	return d.do(req)
}

type httpStatusError struct {
	method string
	path   string
	status string
	code   int
	body   []byte // bounded prefix for internal classification only; never printed in Error()
}

func (e httpStatusError) Error() string {
	return fmt.Sprintf("hotspot: %s %s: %s", e.method, e.path, e.status)
}

// isAuthStatus reports HTTP statuses the M2000 web UI returns when a session
// or its gSecureToken is missing, expired or displaced (401, 403, 406).
func isAuthStatus(code int) bool {
	return code == http.StatusUnauthorized || code == http.StatusForbidden || code == http.StatusNotAcceptable
}

// post submits a form of a logged-in session: its reply must say so. On the
// M2000, an expired session or stale gSecureToken answers 401, 403 or 406.
func (d *Inseego) post(ctx context.Context, path, from string, form url.Values) error {
	body, err := d.submit(ctx, path, from, form)
	if err != nil {
		var st httpStatusError
		if errors.As(err, &st) && isAuthStatus(st.code) {
			d.clearSession()
			d.clearCookies()
			return fmt.Errorf("%w: %w", errSession, err)
		}
		return err
	}
	var r struct {
		LoggedIn int `json:"gIsLoggedIn"`
	}
	if json.Unmarshal(body, &r) != nil || r.LoggedIn != 1 {
		d.clearSession()
		return errSession
	}
	if d.token != "" {
		d.tokenAt = time.Now()
	}
	return nil
}

func (d *Inseego) do(req *http.Request) ([]byte, error) {
	res, err := d.HTTP.Do(req)
	if err != nil {
		return nil, err
	}
	defer res.Body.Close()
	body, err := io.ReadAll(io.LimitReader(res.Body, pageLimit))
	if err != nil {
		return nil, err
	}
	if res.StatusCode != http.StatusOK {
		snippet := body
		if len(snippet) > 4096 {
			snippet = snippet[:4096]
		}
		return nil, httpStatusError{method: req.Method, path: req.URL.Path, status: res.Status, code: res.StatusCode, body: snippet}
	}
	return body, nil
}

// SanitizeError returns a single-line, credential-free error string suitable
// for diagnostic logs. It strips passwords, gSecureToken values and cookies.
func SanitizeError(err error) string {
	if err == nil {
		return ""
	}
	s := strings.ReplaceAll(err.Error(), "\r", " ")
	s = strings.ReplaceAll(s, "\n", " ")
	s = secretKV.ReplaceAllString(s, `${1}=[redacted]`)
	s = secretHeader.ReplaceAllString(s, `${1}: [redacted]`)
	s = strings.TrimSpace(s)
	if len(s) > 512 {
		s = s[:512] + "..."
	}
	return s
}

func digest(password string) string {
	sum := sha1.Sum([]byte("pith:" + password))
	return hex.EncodeToString(sum[:])
}

var (
	tokenField   = regexp.MustCompile(`id="gSecureToken"\s+value="([0-9a-f]{40})"`)
	tokenScript  = regexp.MustCompile(`gSecureToken\s*:\s*"([0-9a-f]{40})"`)
	cellValue    = regexp.MustCompile(`(?s)<div class="input" id="([A-Za-z0-9]+)">(.*?)</div>`)
	markup       = regexp.MustCompile(`<[^>]*>`)
	leadingInt   = regexp.MustCompile(`-?\d+`)
	secretKV     = regexp.MustCompile(`(?i)\b(gSecureToken|shaPassword|password|mifisess|session)\s*[:=]\s*["']?[^\s&;,\"']+["']?`)
	secretHeader = regexp.MustCompile(`(?i)\b(set-cookie|cookie|authorization)\s*:\s*[^\r\n;]+`)
)

// parseCell reads the cell from the diagnostics page by the IDs of its
// values, which don't change with the admin's language.
func parseCell(page string) *api.HotspotCell {
	type field struct{ id, value string }
	var fields []field
	for _, m := range cellValue.FindAllStringSubmatch(page, -1) {
		fields = append(fields, field{m[1], strings.TrimSpace(html.UnescapeString(markup.ReplaceAllString(m[2], "")))})
	}
	// The first value whose ID matches, in the order of the page.
	find := func(match func(id string) bool) string {
		for _, f := range fields {
			if f.value != "" && match(f.id) {
				return f.value
			}
		}
		return ""
	}
	is := func(id string) func(string) bool { return func(s string) bool { return s == id } }
	between := func(prefix, suffix string) func(string) bool {
		return func(s string) bool { return strings.HasPrefix(s, prefix) && strings.HasSuffix(s, suffix) }
	}
	number := func(s string) int {
		n, _ := strconv.Atoi(leadingInt.FindString(s))
		return n
	}
	cell := &api.HotspotCell{
		Band:     find(is("band")),
		Operator: find(between("internetStatusNetworkOperator", "")),
		Signal:   number(find(between("internetStatus", "RSSI"))),
		SNR:      number(find(is("internetStatusSNR"))),
		Roaming:  strings.EqualFold(find(between("internetStatus", "Roaming")), "Yes"),
	}
	if cell.Band == "" && cell.Operator == "" && cell.Signal == 0 {
		return nil
	}
	return cell
}
