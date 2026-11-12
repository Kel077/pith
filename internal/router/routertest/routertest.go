// Package routertest fakes the web admin of a Verizon router for tests, with
// made-up values.
package routertest

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/netip"
	"sync"
)

const (
	// Password is the admin password the router takes.
	Password = "example-password"
	token    = "3333333333333333333333333333333333333333"
)

// Router answers as a Verizon router's web admin does, with made-up values.
type Router struct {
	mu       sync.Mutex
	sessions map[string]bool
	Model    string     // as it names itself
	Logins   int        // attempts to log in
	Calls    []string   // the lease's buttons, in order: "release", "renew"
	Address  netip.Addr // its public address; invalid while it has none
	// Next are the addresses renewals give it, in turn; once there are none,
	// a renewal gives it the address it had before.
	Next      []netip.Addr
	Reconnect int  // readings of its connections until connected after a renewal
	Stuck     bool // never connected again
	Banned    int  // seconds it says it refuses logins for
	Full      bool // refuses logins: as many sessions as it allows
	Expire    bool // the session ends after the next request
	Requests  []string

	last    netip.Addr // the address it gave up
	waiting int
}

// New is a router connected with address.
func New(address string) *Router {
	return &Router{sessions: map[string]bool{}, Model: "CHR30A-R", Address: netip.MustParseAddr(address)}
}

// Do runs fn with the router to itself, to read or change its fields.
func (f *Router) Do(fn func(f *Router)) {
	f.mu.Lock()
	defer f.mu.Unlock()
	fn(f)
}

// Release gives the address up, as its page's Release button does.
func (f *Router) Release() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.release()
}

func (f *Router) release() {
	f.Calls = append(f.Calls, "release")
	if f.Address.IsValid() {
		f.last, f.Address = f.Address, netip.Addr{}
	}
}

func (f *Router) session(r *http.Request) bool {
	c, err := r.Cookie("session")
	return err == nil && f.sessions[c.Value]
}

func reply(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}

func (f *Router) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.Requests = append(f.Requests, r.URL.Path)
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	switch r.URL.Path {
	case "/CRSP/open/getModelName":
		reply(w, http.StatusOK, map[string]any{"modelName": f.Model, "role": 1})
		return
	case "/CRSP/open/getLoginMetadata":
		reply(w, http.StatusOK, map[string]any{"adminPwdChanged": false, "passwordRequired": true, "timeout": f.Banned})
		return
	case "/CRSP/open/getLoginToken":
		reply(w, http.StatusOK, map[string]int{"failedCount": 0})
		return
	case "/CRSP/open/login":
		f.Logins++
		want := "Basic " + base64.StdEncoding.EncodeToString([]byte("admin:"+Password))
		switch {
		case f.Full:
			reply(w, http.StatusTooManyRequests, map[string]int{"maxConcurrentSessions": 1})
		case f.Banned > 0 || r.Header.Get("Authorization") != want:
			reply(w, http.StatusUnauthorized, map[string]int{"timeout": 0})
		default:
			id := fmt.Sprint("s", f.Logins)
			f.sessions[id] = true
			http.SetCookie(w, &http.Cookie{Name: "session", Value: id, Path: "/", Secure: true, HttpOnly: true})
			reply(w, http.StatusOK, map[string]int{"QSWPerformed": 1})
		}
		return
	}
	if !f.session(r) {
		reply(w, http.StatusUnauthorized, map[string]string{})
		return
	}
	if r.URL.Path == "/CreateXSRFToken" {
		w.Header().Set("X-Xsrf-Token", token)
		reply(w, http.StatusOK, map[string]string{})
		return
	}
	if r.Header.Get("X-Xsrf-Token") != token {
		reply(w, http.StatusForbidden, map[string]string{})
		return
	}
	if f.Expire {
		f.Expire = false
		clear(f.sessions)
	}
	switch r.URL.Path {
	case "/CRSP/getNetworkConnections":
		if f.waiting > 0 {
			f.waiting--
		} else if !f.Address.IsValid() && f.last.IsValid() && !f.Stuck && f.Calls[len(f.Calls)-1] == "renew" {
			f.Address, f.last = f.last, netip.Addr{}
			if len(f.Next) > 0 {
				f.Address, f.Next = f.Next[0], f.Next[1:]
			}
		}
		address, status, expires := "0.0.0.0", false, 0
		if f.Address.IsValid() {
			address, status, expires = f.Address.String(), true, 114
		}
		reply(w, http.StatusOK, map[string]any{"networkConnectionsList": []map[string]any{
			{"alias": "lan", "status": true, "name": "Network (Home/Office)"},
			{"alias": "broadband", "status": status, "enable": true, "ipv4Address": address, "ipv4Protocol": "DHCP", "wanExpiresIN": expires},
		}, "timestamp": 1})
	case "/CRSP/tr181/updateParameterValues":
		var in struct {
			Pairs map[string]int `json:"nameValuePairs"`
		}
		if json.NewDecoder(r.Body).Decode(&in) != nil || len(in.Pairs) != 1 {
			reply(w, http.StatusBadRequest, map[string]string{})
			return
		}
		switch {
		case in.Pairs["Device.DHCPv4.Client.[Alias == 'wan'].X_PRPL-COM_Release"] == 1:
			f.release()
		case in.Pairs["Device.DHCPv4.Client.[Alias == 'wan'].Renew"] == 1:
			f.Calls = append(f.Calls, "renew")
			f.waiting = f.Reconnect
		default:
			reply(w, http.StatusBadRequest, map[string]string{})
			return
		}
		reply(w, http.StatusOK, map[string]string{})
	case "/CRSP/logout":
		c, _ := r.Cookie("session")
		delete(f.sessions, c.Value)
		reply(w, http.StatusOK, map[string]string{})
	default:
		http.NotFound(w, r)
	}
}
