package main

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/netip"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"

	"github.com/Kel077/pith/internal/api"
	"howett.net/plist"
)

type webkitPlan struct {
	App              api.AppRef `json:"app"`
	Executable       string     `json:"executable"`
	ExecutableSHA256 string     `json:"executableSHA256"`
	PlistSHA256      string     `json:"plistSHA256"`
	Nonce            string     `json:"navigationNonce"`
}

func inspectWebKitApp(path string) (*webkitPlan, error) {
	if !filepath.IsAbs(path) {
		return nil, errors.New("webkit-app must be an absolute application bundle path")
	}
	app, err := filepath.EvalSymlinks(path)
	if err != nil {
		return nil, err
	}
	if filepath.Ext(app) != ".app" {
		return nil, errors.New("webkit-app must name an application bundle")
	}
	f, err := os.OpenFile(filepath.Join(app, "Contents", "Info.plist"), os.O_RDONLY|syscall.O_NONBLOCK, 0)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil || !info.Mode().IsRegular() || info.Size() > 1<<20 {
		return nil, errors.New("browser metadata must be a regular file of at most 1 MiB")
	}
	data, err := io.ReadAll(io.LimitReader(f, 1<<20+1))
	if err != nil {
		return nil, err
	}
	if len(data) > 1<<20 {
		return nil, errors.New("browser metadata exceeds 1 MiB")
	}
	var metadata struct {
		Identifier string `plist:"CFBundleIdentifier"`
		Executable string `plist:"CFBundleExecutable"`
	}
	if _, err := plist.Unmarshal(data, &metadata); err != nil {
		return nil, err
	}
	if metadata.Identifier != "com.apple.Safari" || metadata.Executable == "" || metadata.Executable == "." || filepath.Base(metadata.Executable) != metadata.Executable {
		return nil, errors.New("this reviewed observation supports only Safari with a plain executable name")
	}
	executable, err := filepath.EvalSymlinks(filepath.Join(app, "Contents", "MacOS", metadata.Executable))
	if err != nil {
		return nil, err
	}
	relative, err := filepath.Rel(app, executable)
	if err != nil || relative == ".." || strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
		return nil, errors.New("browser executable must remain inside the reviewed app")
	}
	digest, err := browserExecutableHash(executable)
	if err != nil {
		return nil, err
	}
	return &webkitPlan{App: api.AppRef{Path: app, BundleID: metadata.Identifier, Name: "Safari"}, Executable: executable, ExecutableSHA256: digest, PlistSHA256: hashBytes(data)}, nil
}

func browserExecutableHash(path string) (string, error) {
	f, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NONBLOCK, 0)
	if err != nil {
		return "", err
	}
	defer f.Close()
	before, err := f.Stat()
	if err != nil || !before.Mode().IsRegular() || before.Mode().Perm()&0111 == 0 || before.Size() > 256<<20 {
		return "", errors.New("browser executable must be a regular executable of at most 256 MiB")
	}
	h := sha256.New()
	n, err := io.Copy(h, io.LimitReader(f, 256<<20+1))
	if err != nil {
		return "", err
	}
	after, err := f.Stat()
	if err != nil || n != before.Size() || after.Size() != before.Size() || !after.ModTime().Equal(before.ModTime()) {
		return "", errors.New("browser executable changed while hashing")
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

func (s *session) webkitURL() string {
	endpoint, _ := netip.ParseAddrPort(s.Target)
	host := s.ServerName
	if endpoint.Port() != 443 {
		host = net.JoinHostPort(host, strconv.Itoa(int(endpoint.Port())))
	}
	return (&url.URL{Scheme: "https", Host: host, Path: "/", RawQuery: "pith-rootcheck=" + s.WebKit.Nonce}).String()
}

func (s *session) webkitPlanBytes() ([]byte, error) {
	if s.WebKit == nil || s.TestProfile != nil || s.WebKit.App.BundleID != "com.apple.Safari" || !filepath.IsAbs(s.WebKit.App.Path) || !filepath.IsAbs(s.WebKit.Executable) || len(s.WebKit.ExecutableSHA256) != 64 || len(s.WebKit.PlistSHA256) != 64 || len(s.WebKit.Nonce) != 32 {
		return nil, errors.New("prepare a new host-ipv4 session with --webkit-app")
	}
	if _, err := hex.DecodeString(s.WebKit.Nonce); err != nil {
		return nil, errors.New("invalid browser navigation nonce")
	}
	if _, err := testAddress(s.Target); err != nil {
		return nil, err
	}
	plan := struct {
		Browser    *webkitPlan `json:"browser"`
		Target     string      `json:"capturedTarget"`
		URL        string      `json:"manualURL"`
		MaxSeconds int         `json:"maximumObservationSeconds"`
		Notes      []string    `json:"notes"`
	}{s.WebKit, s.Target, s.webkitURL(), 60, []string{
		"After READY, the coordinator navigates once manually, or explicit --webkit-open invokes /usr/bin/open -a with only the frozen app and URL once. No retries, AppleScript, window/tab control or browser termination. The coordinator closes only the test tab afterward.",
		"No DNS override or new capture scope. If the browser resolves another address, reuses a native pooled socket, or its short-lived PCB cannot be observed, the case is inconclusive; do not widen capture or keep retrying.",
		"Observe a fresh exact TCP/UDP tuple, then independently read its owner/effective PID and executable through the existing unprivileged PCB/process resolver. A process-name match alone is never delegation proof.",
		"Only socket delegation and routing are checked automatically. The HTTP status/body/navigation nonce inside TLS require separate manual page evidence; background URLSession is skipped.",
		"One navigation can open multiple sockets or trigger background traffic. The harness bounds capture time, not the browser's total network bytes; use the reviewed small plain-text endpoint only.",
	}}
	data, err := json.MarshalIndent(plan, "", "  ")
	return append(data, '\n'), err
}

func (s *session) writeWebKitPlan(path string) error {
	var err error
	s.WebKit, err = inspectWebKitApp(path)
	if err != nil {
		return err
	}
	var nonce [16]byte
	if _, err := rand.Read(nonce[:]); err != nil {
		return err
	}
	s.WebKit.Nonce = hex.EncodeToString(nonce[:])
	data, err := s.webkitPlanBytes()
	if err != nil {
		return err
	}
	if err := writePrivate(filepath.Join(s.Dir, "webkit-plan.json"), data); err != nil {
		return err
	}
	s.WebKitPlanSHA256 = hashBytes(data)
	return s.save()
}

func (s *session) reviewedWebKitPlan() error {
	data, err := s.webkitPlanBytes()
	if err != nil {
		return err
	}
	if hashBytes(data) != s.WebKitPlanSHA256 {
		return errors.New("browser observation plan changed after preparation")
	}
	path := filepath.Join(s.Dir, "webkit-plan.json")
	info, err := os.Lstat(path)
	if err != nil || !info.Mode().IsRegular() || info.Mode().Perm() != 0600 {
		return errors.New("browser review plan is not a private regular file")
	}
	actual, err := os.ReadFile(path)
	if err != nil || hashBytes(actual) != s.WebKitPlanSHA256 {
		return errors.New("browser review file changed after preparation")
	}
	current, err := inspectWebKitApp(s.WebKit.App.Path)
	if err != nil {
		return err
	}
	current.Nonce = s.WebKit.Nonce
	if *current != *s.WebKit {
		return errors.New("reviewed browser app or executable changed")
	}
	return nil
}
