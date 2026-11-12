// Package router talks to the routers behind interfaces, over their web
// admin: Verizon's, such as the CHR30A.
package router

import (
	"bytes"
	"context"
	"crypto/sha1"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/netip"
	"strings"
	"sync"
	"time"

	"github.com/Kel077/pith/internal/api"
)

// Model is the model of a router Pith can talk to, by the name its web admin
// gives, or "" for any other. Only Verizon's CHR30A-R has been tested; others
// may differ, so Pith leaves them alone.
func Model(name string) string {
	if strings.EqualFold(strings.TrimSpace(name), "CHR30A-R") {
		return "CHR30A-R"
	}
	return ""
}

// pageLimit bounds what Pith reads of a reply of the web admin.
const pageLimit = 1 << 20

// Identify asks the web admin at base, without logging in, which router it
// is: the model of one Pith can talk to, else ErrUnsupported.
func Identify(ctx context.Context, client *http.Client, base string) (string, error) {
	status, body, _, err := send(ctx, client, base, "/CRSP/open/getModelName", nil, nil)
	unsupported := &api.Error{Code: api.ErrUnsupported, Detail: "the router is not one Pith knows"}
	if err != nil {
		return "", err
	}
	var r struct {
		ModelName string `json:"modelName"`
	}
	if status != http.StatusOK || json.Unmarshal(body, &r) != nil {
		return "", unsupported
	}
	model := Model(r.ModelName)
	if model == "" {
		return "", unsupported
	}
	return model, nil
}

// Verizon is the web admin of a Verizon router.
type Verizon struct {
	Base  string       // its address, from the interface: "https://192.0.2.1"
	Model string       // CHR30A-R
	HTTP  *http.Client // through the router's interface, keeping cookies
	// Password returns the admin password Pith keeps, or "" when it has none.
	Password func() (string, error)
	// Poll is how long to wait between readings while renewing: a second
	// when zero.
	Poll time.Duration

	mu       sync.Mutex
	loggedIn bool
	token    string // the session's XSRF token
	refused  string // a hash of the password the router refused: never tried again
}

// WAN is the router's connection to the internet.
type WAN struct {
	Connected bool
	IPv4      netip.Addr    // its public address; invalid when it has none
	Lease     time.Duration // until its DHCP lease ends
}

// The TR-181 parameters its own page sets for the buttons of the broadband
// connection's DHCP lease.
const (
	releaseParameter = "Device.DHCPv4.Client.[Alias == 'wan'].X_PRPL-COM_Release"
	renewParameter   = "Device.DHCPv4.Client.[Alias == 'wan'].Renew"
)

// Status reads the router: its connection to the internet when Pith has its
// admin password, else that it is locked.
func (d *Verizon) Status(ctx context.Context) (api.Router, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	r := api.Router{Model: d.Model, At: time.Now()}
	w, err := d.wan(ctx)
	switch {
	case api.Code(err) == api.ErrWrongPassword:
		r.Locked = true
		return r, nil
	case err != nil:
		return r, err
	}
	r.Connected = w.Connected
	if w.IPv4.IsValid() {
		r.PublicIPv4 = w.IPv4.String()
	}
	return r, nil
}

// TryPassword logs in with password: ErrWrongPassword when the router
// refuses it.
func (d *Verizon) TryPassword(ctx context.Context, password string) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.loggedIn, d.token = false, ""
	if password == "" || digest(password) == d.refused {
		return &api.Error{Code: api.ErrWrongPassword, Detail: "the router refused this admin password"}
	}
	return d.loginWith(ctx, password)
}

// WAN reads the router's connection to the internet.
func (d *Verizon) WAN(ctx context.Context) (WAN, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.wan(ctx)
}

// Renew has the router give its address up, waits pause, and has it ask its
// provider for an address again: it returns the connection it has once it is
// connected again. Once it was asked to give its address up, it is asked for
// one again whatever happens, within an independent bound, so that it doesn't
// stay offline.
func (d *Verizon) Renew(ctx context.Context, pause time.Duration) (w WAN, resultErr error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	// Make sure of a session first: once the address is given up, logging in
	// again is one more thing that can fail.
	if _, err := d.wan(ctx); err != nil {
		return w, err
	}
	// A canceled or failed request may still have reached the router.
	needsRenew := true
	defer func() {
		if !needsRenew {
			return
		}
		restoreCtx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cancel()
		var restoreErr error
		for range 2 {
			if restoreErr = d.set(restoreCtx, renewParameter); restoreErr == nil || restoreCtx.Err() != nil {
				break
			}
		}
		if restoreErr != nil {
			resultErr = errors.Join(resultErr, fmt.Errorf("router: ask for an address again: %w", restoreErr))
		}
	}()
	if err := d.set(ctx, releaseParameter); err != nil {
		return w, fmt.Errorf("router: give the address up: %w", err)
	}
	// It says it is disconnected within a second or two.
	d.await(ctx, 10*time.Second, func(w WAN) bool { return !w.Connected })
	select {
	case <-ctx.Done():
		return w, ctx.Err()
	case <-time.After(pause):
	}
	err := d.set(ctx, renewParameter)
	if err != nil && ctx.Err() == nil {
		err = d.set(ctx, renewParameter)
	}
	if err != nil {
		return w, fmt.Errorf("router: ask for an address again: %w", err)
	}
	needsRenew = false
	w, ok := d.await(ctx, 90*time.Second, func(w WAN) bool { return w.Connected })
	if !ok {
		if err := ctx.Err(); err != nil {
			return w, err
		}
		return w, errors.New("router: not connected again after 90 s")
	}
	return w, nil
}

// Reconnected waits until the router is connected, for limit at most, and
// returns its connection then; false when it isn't.
func (d *Verizon) Reconnected(ctx context.Context, limit time.Duration) (WAN, bool) {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.await(ctx, limit, func(w WAN) bool { return w.Connected })
}

// Close ends Pith's session, so that it doesn't count against the sessions
// the router allows.
func (d *Verizon) Close(ctx context.Context) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.loggedIn {
		_, _, _, _ = send(ctx, d.HTTP, d.Base, "/CRSP/logout", nil, d.headers())
	}
	d.loggedIn, d.token = false, ""
}

// await reads the connection until ok, or until limit has passed, and returns
// the last one it read.
func (d *Verizon) await(ctx context.Context, limit time.Duration, ok func(WAN) bool) (WAN, bool) {
	poll := d.Poll
	if poll == 0 {
		poll = time.Second
	}
	deadline := time.Now().Add(limit)
	var last WAN
	for {
		if w, err := d.wan(ctx); err == nil {
			last = w
			if ok(w) {
				return w, true
			}
		}
		if time.Now().Add(poll).After(deadline) {
			return last, false
		}
		select {
		case <-ctx.Done():
			return last, false
		case <-time.After(poll):
		}
	}
}

// wan reads the broadband connection from the list its network page shows.
func (d *Verizon) wan(ctx context.Context) (WAN, error) {
	body, err := d.call(ctx, "/CRSP/getNetworkConnections", nil)
	if err != nil {
		return WAN{}, err
	}
	var r struct {
		List []struct {
			Alias   string  `json:"alias"`
			Status  bool    `json:"status"`
			Address string  `json:"ipv4Address"`
			Expires float64 `json:"wanExpiresIN"` // minutes
		} `json:"networkConnectionsList"`
	}
	if err := json.Unmarshal(body, &r); err != nil {
		return WAN{}, fmt.Errorf("router: read its connections: %w", err)
	}
	for _, c := range r.List {
		if c.Alias != "broadband" {
			continue
		}
		w := WAN{Lease: time.Duration(max(c.Expires, 0) * float64(time.Minute))}
		if a, err := netip.ParseAddr(c.Address); err == nil && a.Is4() && a.IsGlobalUnicast() {
			w.IPv4 = a
		}
		w.Connected = c.Status && w.IPv4.IsValid()
		return w, nil
	}
	return WAN{}, errors.New("router: no broadband connection in its list")
}

// set sets a TR-181 parameter to 1, as the buttons of its page do.
func (d *Verizon) set(ctx context.Context, parameter string) error {
	_, err := d.call(ctx, "/CRSP/tr181/updateParameterValues", map[string]any{"nameValuePairs": map[string]int{parameter: 1}})
	return err
}

// call sends a request of the logged-in session: it logs in first, again when
// the router says the session ended, and fetches another XSRF token when the
// router refuses the one it has, as its page does.
func (d *Verizon) call(ctx context.Context, path string, in any) ([]byte, error) {
	refreshed := false
	for range 3 {
		if err := d.login(ctx); err != nil {
			return nil, err
		}
		status, body, _, err := send(ctx, d.HTTP, d.Base, path, in, d.headers())
		if err != nil {
			return nil, err
		}
		switch {
		case status == http.StatusOK:
			return body, nil
		case (status == http.StatusForbidden || status == 419) && !refreshed:
			refreshed = true
			if err := d.refreshToken(ctx); err != nil {
				d.loggedIn = false
			}
		case status == http.StatusUnauthorized || status == http.StatusForbidden || status == 419:
			d.loggedIn, d.token = false, ""
		default:
			return nil, fmt.Errorf("router: %s: HTTP %d", path, status)
		}
	}
	return nil, fmt.Errorf("router: %s needs a session it doesn't keep", path)
}

// login makes sure of a session with the password Pith keeps. Without one,
// or with one the router refused before, it is ErrWrongPassword at once:
// trying a refused password again would have the router refuse logins for a
// while.
func (d *Verizon) login(ctx context.Context) error {
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
		return &api.Error{Code: api.ErrWrongPassword, Detail: "the router refused the admin password"}
	}
	return d.loginWith(ctx, password)
}

func (d *Verizon) loginWith(ctx context.Context, password string) error {
	status, body, _, err := send(ctx, d.HTTP, d.Base, "/CRSP/open/getLoginMetadata", nil, nil)
	if err != nil {
		return err
	}
	var meta struct {
		Timeout int `json:"timeout"` // seconds it refuses logins for, after failed ones
	}
	if status != http.StatusOK || json.Unmarshal(body, &meta) != nil {
		return fmt.Errorf("router: read its login page: HTTP %d", status)
	}
	if meta.Timeout > 0 {
		return fmt.Errorf("router: refuses logins for %d s more", meta.Timeout)
	}
	// As its page does before logging in.
	if status, _, _, err = send(ctx, d.HTTP, d.Base, "/CRSP/open/getLoginToken", nil, nil); err != nil {
		return err
	} else if status != http.StatusOK {
		return fmt.Errorf("router: start a login: HTTP %d", status)
	}
	credentials := "Basic " + base64.StdEncoding.EncodeToString([]byte("admin:"+password))
	status, _, _, err = send(ctx, d.HTTP, d.Base, "/CRSP/open/login", map[string]bool{"keepMeSignIn": false}, map[string]string{"Authorization": credentials})
	if err != nil {
		return err
	}
	switch status {
	case http.StatusOK:
	case http.StatusUnauthorized:
		d.refused = digest(password)
		return &api.Error{Code: api.ErrWrongPassword, Detail: "the router refused the admin password"}
	case http.StatusTooManyRequests:
		return errors.New("router: it has as many admin sessions as it allows")
	default:
		return fmt.Errorf("router: log in: HTTP %d", status)
	}
	d.loggedIn = true
	if err := d.refreshToken(ctx); err != nil {
		d.loggedIn = false
		return err
	}
	return nil
}

// refreshToken fetches the session's XSRF token, which its page sends with
// every request.
func (d *Verizon) refreshToken(ctx context.Context) error {
	status, _, header, err := send(ctx, d.HTTP, d.Base, "/CreateXSRFToken", nil, nil)
	if err != nil {
		return err
	}
	token := header.Get("X-Xsrf-Token")
	if status != http.StatusOK || token == "" {
		return fmt.Errorf("router: no XSRF token: HTTP %d", status)
	}
	d.token = token
	return nil
}

func (d *Verizon) headers() map[string]string {
	if d.token == "" {
		return nil
	}
	return map[string]string{"X-Xsrf-Token": d.token}
}

// send posts to the web admin as its page does: every request is a POST, with
// a JSON body or none.
func send(ctx context.Context, client *http.Client, base, path string, in any, headers map[string]string) (int, []byte, http.Header, error) {
	var body io.Reader = http.NoBody
	if in != nil {
		data, err := json.Marshal(in)
		if err != nil {
			return 0, nil, nil, err
		}
		body = bytes.NewReader(data)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, base+path, body)
	if err != nil {
		return 0, nil, nil, err
	}
	if in != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Origin", base)
	req.Header.Set("Referer", base+"/")
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	res, err := client.Do(req)
	if err != nil {
		return 0, nil, nil, err
	}
	defer res.Body.Close()
	data, err := io.ReadAll(io.LimitReader(res.Body, pageLimit))
	if err != nil {
		return 0, nil, nil, err
	}
	return res.StatusCode, data, res.Header, nil
}

func digest(password string) string {
	sum := sha1.Sum([]byte("pith:" + password))
	return hex.EncodeToString(sum[:])
}
