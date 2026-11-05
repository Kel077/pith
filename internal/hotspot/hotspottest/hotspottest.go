// Package hotspottest fakes the web admin of an Inseego MiFi hotspot for
// tests, with made-up values.
package hotspottest

import (
	"crypto/sha1"
	"encoding/hex"
	"fmt"
	"net/http"
	"strings"
	"sync"
)

const (
	// Password is the admin password the MiFi takes.
	Password     = "example-password"
	loginToken   = "1111111111111111111111111111111111111111"
	sessionToken = "2222222222222222222222222222222222222222"
)

// MiFi answers as an Inseego MiFi's web admin does, with made-up values.
type MiFi struct {
	mu        sync.Mutex
	sessions  map[string]bool
	Logins    int      // attempts to log in
	Calls     []string // the cellular data switches, in order
	DataOn    bool
	Reconnect int  // readings of the status until connected again
	Stuck     bool // never connected again
	Expire    bool // the session ends after the next diagnostics page
}

// New is a MiFi that is connected.
func New() *MiFi { return &MiFi{sessions: map[string]bool{}, DataOn: true} }

// Do runs fn with the MiFi to itself, to read or change its fields.
func (f *MiFi) Do(fn func(f *MiFi)) {
	f.mu.Lock()
	defer f.mu.Unlock()
	fn(f)
}

func (f *MiFi) loggedIn(r *http.Request) bool {
	c, err := r.Cookie("session")
	return err == nil && f.sessions[c.Value]
}

func (f *MiFi) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	switch r.URL.Path {
	case "/srv/status":
		state := "Disconnected"
		if f.DataOn && f.Reconnect == 0 {
			state = "Connected"
		} else if f.DataOn {
			f.Reconnect--
		}
		login := 0
		if f.loggedIn(r) {
			login = 1
		}
		fmt.Fprintf(w, `{"loginStatus":%d,"statusData":{"statusBarConnectionState":%q,"statusBarTechnology":"5G","statusBarSignalBars":4}}`, login, state)
	case "/login/":
		fmt.Fprintf(w, `<form><input type="hidden" name="gSecureToken" id="gSecureToken" value="%s" /></form>`, loginToken)
	case "/submitLogin/":
		f.Logins++
		sum := sha1.Sum([]byte(Password + loginToken))
		if r.FormValue("gSecureToken") != loginToken || r.FormValue("shaPassword") != hex.EncodeToString(sum[:]) || r.Header.Get("X-Requested-With") == "" {
			fmt.Fprint(w, `{"gIsLoggedIn":0,"gPageId":"login"}`)
			return
		}
		id := fmt.Sprint("s", f.Logins)
		f.sessions[id] = true
		http.SetCookie(w, &http.Cookie{Name: "session", Value: id, Path: "/"})
		// As the real one: its reply is the login page's state, logged out.
		fmt.Fprint(w, `{"gIsAdminSettingsEnabled":1,"gIsLoggedIn":0,"gPageId":"login"}`)
	case "/diagnostics/":
		if !f.loggedIn(r) {
			fmt.Fprintf(w, `<script>var data = { gSecureToken : "%s" };</script><p>Log in</p>`, loginToken)
			return
		}
		if f.Expire { // the session ends after this page
			f.sessions = map[string]bool{}
			f.Expire = false
		}
		fmt.Fprint(w, `<div class="form-group"><div class="label"> Status: </div><div class="input" id="internetStatus4G"> Connected </div></div>
<div class="form-group"><div class="label"> Network Operator: </div><div class="input" id="internetStatusNetworkOperator4G"> Example Mobile </div></div>
<div class="form-group"><div class="label"> Network Operator: </div><div class="input" id="internetStatusNetworkOperator5G"> Other </div></div>
<div class="form-group"><div class="label"> Signal Strength: </div><div class="input" id="internetStatus4gRSSI"> -101 dBm </div></div>
<div class="form-group"><div class="label"> SNR: </div><div class="input" id="internetStatusSNR"> 9 dB </div></div>
<div class="form-group"><div class="label"> Band: </div><div class="input" id="band"> n71 </div></div>
<div class="form-group"><div class="label"> Roaming: </div><div class="input" id="internetStatus4gRoaming"> No </div></div>`)
	case "/networks/":
		if !f.loggedIn(r) {
			fmt.Fprintf(w, `<script>var data = { gSecureToken : "%s" };</script>`, loginToken)
			return
		}
		fmt.Fprintf(w, `<script>$.ajax({ url : url, type : "POST", data : { gSecureToken : "%s" } }); var on = "/wwan/enablecellulardata/";</script>`, sessionToken)
	case "/wwan/disablecellulardata/", "/wwan/enablecellulardata/":
		if r.Method != http.MethodPost || !f.loggedIn(r) || r.FormValue("gSecureToken") != sessionToken {
			fmt.Fprint(w, `{"gIsLoggedIn":0}`)
			return
		}
		on := strings.Contains(r.URL.Path, "enable")
		f.Calls = append(f.Calls, map[bool]string{true: "on", false: "off"}[on])
		f.DataOn = on
		if on {
			f.Reconnect = 2
			if f.Stuck {
				f.Reconnect = 1 << 30
			}
		}
		fmt.Fprint(w, `{"gIsLoggedIn":1,"gPageId":"wwan_settings"}`)
	default:
		http.NotFound(w, r)
	}
}
