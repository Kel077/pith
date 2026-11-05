package main

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/netip"
	"net/url"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/Kel077/pith/internal/api"
	"howett.net/plist"
)

type webkitPlan struct {
	App              api.AppRef       `json:"app"`
	Executable       string           `json:"executable"`
	ExecutableSHA256 string           `json:"executableSHA256"`
	PlistSHA256      string           `json:"plistSHA256"`
	Nonce            string           `json:"navigationNonce"`
	Addresses        []string         `json:"frozenARecords"`
	ResolvedAt       time.Time        `json:"resolvedAt"`
	DNSReview        *webkitDNSReview `json:"dnsReview,omitempty"`
}

type webkitDNSReview struct {
	AQueries  int      `json:"aQueries"`
	StableA   bool     `json:"stableA"`
	AAAAKnown bool     `json:"aaaaKnown"`
	AAAACount int      `json:"aaaaCount"`
	Warnings  []string `json:"warnings,omitempty"`
}

type webkitResolver func(context.Context, string, string) ([]netip.Addr, error)

func (s *session) freezeWebKit(ctx context.Context, path string, resolve webkitResolver) error {
	if s.TestProfile != nil {
		return errors.New("webkit acceptance requires host-ipv4")
	}
	name, err := api.ParseTarget(s.ServerName)
	if err != nil || !name.IsDomain() || name.Subdomains || name.String() != s.ServerName {
		return errors.New("webkit target requires a plain lowercase DNS name")
	}
	endpoint, err := testAddress(s.Target)
	if err != nil {
		return err
	}
	plan, err := inspectWebKitApp(path)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	addresses, err := resolve(ctx, "ip4", s.ServerName)
	if err != nil {
		return fmt.Errorf("freeze browser A records: %w", err)
	}
	unique := make(map[netip.Addr]bool)
	for _, addr := range addresses {
		addr = addr.Unmap()
		if _, err := testAddress(netip.AddrPortFrom(addr, endpoint.Port()).String()); err != nil {
			return errors.New("browser A records contain a non-public IPv4 address")
		}
		unique[addr] = true
	}
	if len(unique) == 0 || len(unique) > 16 || !unique[endpoint.Addr()] {
		return errors.New("browser needs 1 to 16 distinct A records including the frozen TLS target")
	}
	ordered := make([]netip.Addr, 0, len(unique))
	for addr := range unique {
		ordered = append(ordered, addr)
	}
	slices.SortFunc(ordered, netip.Addr.Compare)
	for _, addr := range ordered {
		plan.Addresses = append(plan.Addresses, addr.String())
	}
	plan.DNSReview = reviewWebKitDNS(ctx, s.ServerName, ordered, resolve)
	var nonce [16]byte
	if _, err := rand.Read(nonce[:]); err != nil {
		return err
	}
	plan.Nonce, plan.ResolvedAt = hex.EncodeToString(nonce[:]), time.Now()
	s.WebKit = plan
	return nil
}

// Three IPv4 samples including the frozen answer, plus one IPv6 query, share the
// preparation lookup deadline. Cached answers and a short sample cannot prove
// future stability. A different answer is a warning, never scope expansion.
func reviewWebKitDNS(ctx context.Context, name string, frozen []netip.Addr, resolve webkitResolver) *webkitDNSReview {
	r := &webkitDNSReview{AQueries: 1, StableA: true}
	for range 2 {
		r.AQueries++
		addresses, err := resolve(ctx, "ip4", name)
		if err != nil {
			r.StableA = false
			continue
		}
		addresses = slices.Clone(addresses)
		for i := range addresses {
			addresses[i] = addresses[i].Unmap()
		}
		slices.SortFunc(addresses, netip.Addr.Compare)
		if !slices.Equal(slices.Compact(addresses), frozen) {
			r.StableA = false
		}
	}
	if !r.StableA {
		r.Warnings = append(r.Warnings, "Browser A records changed or could not be compared across three bounded queries; choose a small stable endpoint, not a rotating CDN subset.")
	}
	addresses, err := resolve(ctx, "ip6", name)
	var dnsError *net.DNSError
	if err == nil {
		r.AAAAKnown = true
		unique := map[netip.Addr]bool{}
		for _, addr := range addresses {
			if !addr.Is6() || addr.Is4In6() {
				r.AAAAKnown = false
				continue
			}
			unique[addr] = true
		}
		r.AAAACount = len(unique)
	} else if errors.As(err, &dnsError) && dnsError.IsNotFound {
		r.AAAAKnown = true // No IPv6 records, as distinct from a resolver timeout.
	}
	if !r.AAAAKnown {
		r.Warnings = append(r.Warnings, "Browser AAAA availability is unknown; verify that the endpoint has no AAAA records before this IPv4-only observation.")
	}
	if r.AAAACount > 0 {
		r.Warnings = append(r.Warnings, "Browser endpoint has AAAA records; Safari may select IPv6 outside this IPv4-only capture. Choose an endpoint without AAAA records.")
	}
	return r
}

func (s *session) validateWebKit() error {
	if s.WebKit == nil {
		return nil
	}
	if s.TestProfile != nil || len(s.WebKit.Addresses) == 0 || len(s.WebKit.Addresses) > 16 || s.WebKit.ResolvedAt.IsZero() {
		return errors.New("prepare a new host-ipv4 browser session with frozen A records")
	}
	endpoint, err := testAddress(s.Target)
	if err != nil {
		return err
	}
	var previous netip.Addr
	found := false
	for _, text := range s.WebKit.Addresses {
		addr, err := netip.ParseAddr(text)
		if err != nil {
			return errors.New("invalid frozen browser A record")
		}
		if _, err := testAddress(netip.AddrPortFrom(addr, endpoint.Port()).String()); err != nil || addr.String() != text || previous.IsValid() && previous.Compare(addr) >= 0 {
			return errors.New("browser A records must be canonical, public, unique and sorted")
		}
		previous, found = addr, found || addr == endpoint.Addr()
	}
	if !found {
		return errors.New("browser A records omit the frozen TLS target")
	}
	return nil
}

func (s *session) webkitTargetIndex(remote string) int {
	endpoint, err := netip.ParseAddrPort(remote)
	target, _ := netip.ParseAddrPort(s.Target)
	if err != nil || endpoint.Port() != target.Port() || s.WebKit == nil {
		return 0
	}
	return slices.Index(s.WebKit.Addresses, endpoint.Addr().String()) + 1
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
	if err := s.validateWebKit(); err != nil {
		return nil, err
	}
	plan := struct {
		Browser    *webkitPlan `json:"browser"`
		Target     string      `json:"capturedTarget"`
		URL        string      `json:"manualURL"`
		MaxSeconds int         `json:"maximumObservationSeconds"`
		Notes      []string    `json:"notes"`
	}{s.WebKit, s.Target, s.webkitURL(), 60, []string{
		"Manual mode leaves browser cleanup to the coordinator. Explicit --webkit-open refuses an already running Safari, then invokes /usr/bin/open -a with only the frozen app and URL once after READY. No retries, AppleScript or window/tab control.",
		"Every automatic-open return attempts bounded cleanup: SIGTERM the new exact Safari identity, wait up to five seconds, then allow three seconds for its independently owned new WebKit services to exit. Only surviving services in Safari's new coalitions may receive SIGKILL, followed by a two-second wait. PID generation protects signal targeting; pre-existing, reused or unproven identities are never signaled.",
		"Process cleanup has an independent fifteen-second deadline. The private report records the prior browser state, identities, signals, exit results and uncertain survivors. Uninstall and audit-processes independently list all processes started during the recorded window and still present, without sending signals.",
		"Preparation freezes the first complete A-record answer as distinct /32 routes, with at most 16 total scoped hosts. Two further A queries and one AAAA query share the five-second lookup bound and supply warnings only; no later DNS override or scope expansion.",
		"Use a small stable A-record set and no AAAA records. A CDN that rotates subsets of a larger pool is unsuitable; short repeated/cacheable DNS samples cannot prove future stability. IPv6, new DNS answers, pooled sockets or short-lived PCBs can make observation incomplete.",
		"Private Relay status is unknown: no documented reliable per-navigation read-only detector is used. The coordinator must establish a direct-connection prerequisite before running; active relay traffic will not reach these origin /32 routes.",
		"Observe a fresh exact TCP/UDP tuple, then independently read its owner/effective PID and executable through the existing unprivileged PCB/process resolver. A process-name match alone is never delegation proof.",
		"Only socket delegation and routing are checked automatically. The HTTP status/body/navigation nonce inside TLS require separate manual page evidence; background URLSession is skipped.",
		"One navigation can open multiple sockets or trigger background traffic. The harness bounds capture time, not the browser's total network bytes; use the reviewed small plain-text endpoint only.",
	}}
	data, err := json.MarshalIndent(plan, "", "  ")
	return append(data, '\n'), err
}

func (s *session) writeWebKitPlan() error {
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
	current.Addresses, current.ResolvedAt = s.WebKit.Addresses, s.WebKit.ResolvedAt
	current.DNSReview = s.WebKit.DNSReview
	if !reflect.DeepEqual(current, s.WebKit) {
		return errors.New("reviewed browser app or executable changed")
	}
	return nil
}
