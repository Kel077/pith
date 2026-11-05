// Package hotspot talks to mobile hotspots through the interfaces they
// serve, over their web admin: Inseego MiFi devices, such as the M2000.
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
	"time"

	"github.com/Kel077/pith/internal/api"
)

// Model is the model of the hotspot behind an interface, by the interface's
// label, or "" when Pith can't talk to it.
func Model(label string) string {
	for _, m := range []string{"M2000", "M2100", "M3000", "MiFi"} {
		if strings.Contains(strings.ToLower(label), strings.ToLower(m)) {
			return m
		}
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
	// Poll is how long to wait between readings while renewing: a second
	// when zero.
	Poll time.Duration

	mu       sync.Mutex
	loggedIn bool
	refused  string // a hash of the password the hotspot refused: never tried again
}

// pageLimit bounds what Pith reads of a page of the web admin.
const pageLimit = 1 << 20

// Status reads the hotspot: its connection, and its cell when Pith has its
// admin password.
func (d *Inseego) Status(ctx context.Context) (api.Hotspot, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	h, err := d.status(ctx)
	if err != nil {
		return h, err
	}
	page, err := d.adminPage(ctx, "/diagnostics/", func(p string) bool { return strings.Contains(p, `id="band"`) })
	switch {
	case api.Code(err) == api.ErrWrongPassword:
		h.Locked = true
		return h, nil
	case err != nil:
		return h, err
	}
	h.Cell = parseCell(page)
	return h, nil
}

// TryPassword logs in with password: ErrWrongPassword when the hotspot
// refuses it.
func (d *Inseego) TryPassword(ctx context.Context, password string) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.loggedIn = false
	if password == "" || digest(password) == d.refused {
		return &api.Error{Code: api.ErrWrongPassword, Detail: "the hotspot refused this admin password"}
	}
	return d.loginWith(ctx, password)
}

// Renew has the hotspot reconnect to its carrier: it turns its cellular
// data off and on, and waits until it is connected again.
func (d *Inseego) Renew(ctx context.Context) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	page, err := d.adminPage(ctx, "/networks/", func(p string) bool { return strings.Contains(p, "/wwan/enablecellulardata/") })
	if err != nil {
		return err
	}
	m := tokenScript.FindStringSubmatch(page)
	if m == nil {
		return errors.New("hotspot: no session token on its network settings")
	}
	form := url.Values{"gSecureToken": {m[1]}}
	if err := d.post(ctx, "/wwan/disablecellulardata/", "/networks/", form); err != nil {
		return fmt.Errorf("hotspot: turn cellular data off: %w", err)
	}
	// Let it drop its connection: some take a moment, and turning data on
	// before that would leave the old connection in place.
	d.await(ctx, 10*time.Second, func(h api.Hotspot) bool { return !h.Connected })
	err = d.post(ctx, "/wwan/enablecellulardata/", "/networks/", form)
	if err != nil && ctx.Err() == nil {
		err = d.post(ctx, "/wwan/enablecellulardata/", "/networks/", form)
	}
	if err != nil {
		return fmt.Errorf("hotspot: turn cellular data on: %w", err)
	}
	if !d.await(ctx, 90*time.Second, func(h api.Hotspot) bool { return h.Connected }) {
		if err := ctx.Err(); err != nil {
			return err
		}
		return errors.New("hotspot: not connected again after 90 s")
	}
	return nil
}

// await reads the status until ok, or until limit has passed.
func (d *Inseego) await(ctx context.Context, limit time.Duration, ok func(api.Hotspot) bool) bool {
	poll := d.Poll
	if poll == 0 {
		poll = time.Second
	}
	deadline := time.Now().Add(limit)
	for {
		if h, err := d.status(ctx); err == nil && ok(h) {
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

// status reads what the hotspot shows anyone: its connection and signal.
func (d *Inseego) status(ctx context.Context) (api.Hotspot, error) {
	h := api.Hotspot{Model: d.Model, At: time.Now()}
	body, err := d.get(ctx, "/srv/status")
	if err != nil {
		return h, err
	}
	var s struct {
		StatusData struct {
			Connection string          `json:"statusBarConnectionState"`
			Technology string          `json:"statusBarTechnology"`
			Bars       json.RawMessage `json:"statusBarSignalBars"`
		} `json:"statusData"`
	}
	if err := json.Unmarshal(body, &s); err != nil {
		return h, fmt.Errorf("hotspot: read its status: %w", err)
	}
	h.Connected = s.StatusData.Connection == "Connected"
	h.Technology = strings.TrimSpace(s.StatusData.Technology)
	bars, _ := strconv.Atoi(strings.Trim(string(s.StatusData.Bars), `" `))
	h.Bars = min(max(bars, 0), 5)
	return h, nil
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
		d.loggedIn = false
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
	if _, err := d.submit(ctx, "/submitLogin/", "/login/", url.Values{"shaPassword": {hex.EncodeToString(sum[:])}, "gSecureToken": {m[1]}}); err != nil {
		return err
	}
	// Its reply tells the state of the login page, logged out, whatever the
	// result: the status tells whether the session is logged in now.
	body, err = d.get(ctx, "/srv/status")
	if err != nil {
		return err
	}
	var st struct {
		LoginStatus int `json:"loginStatus"`
	}
	if json.Unmarshal(body, &st) != nil || st.LoginStatus != 1 {
		d.refused = digest(password)
		return &api.Error{Code: api.ErrWrongPassword, Detail: "the hotspot refused the admin password"}
	}
	d.loggedIn = true
	return nil
}

func (d *Inseego) get(ctx context.Context, path string) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, d.Base+path, nil)
	if err != nil {
		return nil, err
	}
	return d.do(req)
}

// errSession: the reply of the web admin says its session is not logged in.
var errSession = errors.New("hotspot: not logged in")

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

// post submits a form of a logged-in session: its reply must say so.
func (d *Inseego) post(ctx context.Context, path, from string, form url.Values) error {
	body, err := d.submit(ctx, path, from, form)
	if err != nil {
		return err
	}
	var r struct {
		LoggedIn int `json:"gIsLoggedIn"`
	}
	if json.Unmarshal(body, &r) != nil || r.LoggedIn != 1 {
		d.loggedIn = false
		return errSession
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
		return nil, fmt.Errorf("hotspot: %s %s: %s", req.Method, req.URL.Path, res.Status)
	}
	return body, nil
}

func digest(password string) string {
	sum := sha1.Sum([]byte("pith:" + password))
	return hex.EncodeToString(sum[:])
}

var (
	tokenField  = regexp.MustCompile(`id="gSecureToken"\s+value="([0-9a-f]{40})"`)
	tokenScript = regexp.MustCompile(`gSecureToken\s*:\s*"([0-9a-f]{40})"`)
	cellValue   = regexp.MustCompile(`(?s)<div class="input" id="([A-Za-z0-9]+)">(.*?)</div>`)
	markup      = regexp.MustCompile(`<[^>]*>`)
	leadingInt  = regexp.MustCompile(`-?\d+`)
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
