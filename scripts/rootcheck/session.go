package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"net/netip"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/Kel077/pith/internal/api"
	"github.com/Kel077/pith/internal/engine/capture"
	"github.com/Kel077/pith/internal/helper"
	"github.com/Kel077/pith/internal/netif"
	"github.com/Kel077/pith/internal/testprofile"
	"golang.org/x/sys/unix"
	"howett.net/plist"
)

type session struct {
	Dir              string                 `json:"directory"`
	UID              int                    `json:"uid"`
	Helper           string                 `json:"helper"`
	HelperSHA256     string                 `json:"helperSHA256"`
	Version          string                 `json:"version"`
	Target           string                 `json:"target"`
	ServerName       string                 `json:"serverName"`
	DNSTarget        string                 `json:"dnsTarget,omitempty"`
	ExtraDNSTargets  []string               `json:"extraDNSTargets,omitempty"`
	DNSName          string                 `json:"dnsName,omitempty"`
	DNSPlanSHA256    string                 `json:"dnsPlanSHA256,omitempty"`
	DNSPassed        bool                   `json:"dnsPassed,omitempty"`
	QUICTarget       string                 `json:"quicTarget,omitempty"`
	QUICServerName   string                 `json:"quicServerName,omitempty"`
	QUICPlanSHA256   string                 `json:"quicPlanSHA256,omitempty"`
	QUICPassed       bool                   `json:"quicPassed,omitempty"`
	Bundle           *bundlePlan            `json:"bundle,omitempty"`
	BundlePlanSHA256 string                 `json:"bundlePlanSHA256,omitempty"`
	BundlePassed     bool                   `json:"bundlePassed,omitempty"`
	WebKit           *webkitPlan            `json:"webKit,omitempty"`
	WebKitPlanSHA256 string                 `json:"webKitPlanSHA256,omitempty"`
	Interfaces       [2]api.InterfaceRef    `json:"interfaces"`
	Preflight        []preflight            `json:"preflight"`
	TestProfile      *testprofile.Profile   `json:"testProfile,omitempty"`
	SkippedDNS       []capture.DNSExclusion `json:"skippedDNS,omitempty"`
	// Original is the active rollback baseline; original.json is never replaced.
	Original               snapshot        `json:"original"`
	BaselineLabel          string          `json:"baselineLabel"`
	BaselineFile           string          `json:"baselineFile"`
	Installation           *installationID `json:"installation,omitempty"`
	Cleanup                *cleanupProof   `json:"cleanup,omitempty"`
	InstallScriptSHA256    string          `json:"installScriptSHA256"`
	UninstallScriptSHA256  string          `json:"uninstallScriptSHA256,omitempty"`
	CrashScriptSHA256      string          `json:"crashScriptSHA256,omitempty"`
	AuthorizationAttempted bool            `json:"authorizationAttempted"`
	CancelPassed           bool            `json:"cancelPassed"`
	CancelSkipped          bool            `json:"cancelSkipped,omitempty"`
	Installed              bool            `json:"installed"`
	SmokePassed            bool            `json:"smokePassed"`
	Ownership              captureHistory  `json:"captureOwnership"`
}

type pathState struct {
	Path    string      `json:"path"`
	Present bool        `json:"present"`
	Mode    os.FileMode `json:"mode,omitempty"`
	Error   string      `json:"error,omitempty"`
}

type snapshot struct {
	At               time.Time       `json:"at"`
	Interfaces       []api.Interface `json:"interfaces"`
	Routes           []capture.Route `json:"routes"`
	Paths            []pathState     `json:"paths"`
	Launchd          string          `json:"launchdOutput"`
	LaunchExit       int             `json:"launchdExit"`
	BootSession      string          `json:"bootSession,omitempty"`
	InterfaceIndices map[string]int  `json:"interfaceIndices,omitempty"`
	// Complete assigned prefixes, including loopback and link-local addresses.
	InterfaceAddresses map[string][]netip.Prefix `json:"interfaceAddresses,omitempty"`
}

func prepare(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("prepare", flag.ContinueOnError)
	s := &session{UID: os.Getuid(), BaselineLabel: "initial", BaselineFile: "original.json", Ownership: captureHistory{Complete: true}}
	var names [2]string
	fs.StringVar(&s.Helper, "helper", "", "absolute path to the reviewed helper executable")
	fs.StringVar(&s.Version, "version", "dev", "expected helper build version")
	fs.StringVar(&names[0], "interface-a", "", "first interface BSD name, chosen by the user")
	fs.StringVar(&names[1], "interface-b", "", "second interface BSD name, chosen by the user")
	fs.StringVar(&s.Target, "target", "", "public literal TLS IP:port in the selected capture mode")
	fs.StringVar(&s.ServerName, "server-name", "", "DNS name on the test endpoint's TLS certificate")
	fs.Func("dns-target", "optional public DNS test IPv4:53; repeat for at most two reviewed resolvers", func(value string) error {
		if s.DNSTarget == "" {
			s.DNSTarget = value
		} else {
			s.ExtraDNSTargets = append(s.ExtraDNSTargets, value)
		}
		return nil
	})
	fs.StringVar(&s.DNSName, "dns-name", "", "reviewed lowercase DNS question name; enables the explicit UDP/DNS stage")
	fs.StringVar(&s.QUICTarget, "quic-target", "", "optional reviewed public HTTP/3 IPv4:443")
	fs.StringVar(&s.QUICServerName, "quic-server-name", "", "reviewed HTTP/3 TLS certificate name")
	bundle := fs.Bool("app-bundle", false, "freeze a disposable pure-Go app and helper fixture using this rootcheck executable")
	webkit := fs.String("webkit-app", "", "freeze an absolute Safari .app path for explicit bounded delegation observation")
	mode := fs.String("capture-mode", "host-ipv4", "reviewed installation mode: host-ipv4, full-ipv4 or host-ipv6")
	lease := fs.Duration("test-lease", 0, "profile lease: default/max 120s for full-ipv4, 60s for host-ipv6; minimum 30s")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() != 0 || !filepath.IsAbs(s.Helper) || names[0] == "" || names[0] == names[1] || names[1] == "" || s.Version == "" {
		return errors.New("supply an absolute helper path, two distinct interface names, a target, a TLS name, and the helper version")
	}
	// Connection logs use netip's canonical address and port spelling. Freeze
	// that spelling so equivalent IPv6 inputs cannot fail exact flow matching.
	if target, err := netip.ParseAddrPort(s.Target); err == nil {
		s.Target = target.String()
	}
	leaseSpecified := false
	fs.Visit(func(f *flag.Flag) {
		if f.Name == "test-lease" {
			leaseSpecified = true
		}
	})
	if *mode == testprofile.FullIPv4 || *mode == testprofile.HostIPv6 {
		if !leaseSpecified {
			*lease = 120 * time.Second
			if *mode == testprofile.HostIPv6 {
				*lease = 60 * time.Second
			}
		}
		s.TestProfile = &testprofile.Profile{Mode: *mode, Lease: *lease, Target: s.Target, ServerName: s.ServerName}
	} else if *mode != "host-ipv4" {
		return errors.New("capture-mode must be host-ipv4, full-ipv4 or host-ipv6")
	} else if leaseSpecified {
		return errors.New("test-lease requires an explicit full-ipv4 or host-ipv6 installation")
	}
	if *bundle && s.TestProfile != nil {
		return errors.New("app-bundle acceptance currently requires host-ipv4")
	}
	if *webkit != "" && s.TestProfile != nil {
		return errors.New("webkit acceptance currently requires host-ipv4")
	}
	if *webkit != "" {
		if err := s.freezeWebKit(ctx, *webkit, net.DefaultResolver.LookupNetIP); err != nil {
			return err
		}
	}
	if _, err := s.scopes(); err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(ctx, 40*time.Second)
	defer cancel()
	before, err := takeSnapshot(ctx)
	if err != nil {
		return err
	}
	if err := before.requireAbsent(); err != nil {
		return err
	}
	for i, name := range names {
		iface := (api.InterfaceRef{Name: name}).Resolve(before.Interfaces)
		if !s.usableInterface(iface) {
			return fmt.Errorf("interface %q is absent, down, or has no usable address for %s", name, s.captureMode())
		}
		s.Interfaces[i] = api.InterfaceRef{Name: iface.Name, HardwareAddr: iface.HardwareAddr, Label: iface.Label}
	}
	if (api.InterfaceRef{Name: "pith-missing"}).Resolve(before.Interfaces) != nil {
		return errors.New("the synthetic unavailable interface already exists; cannot test deterministic fallback")
	}
	s.HelperSHA256, err = fileHash(s.Helper)
	if err != nil {
		return err
	}
	// Ordinary, interface-bound sockets are a preflight only. The check command
	// uses unbound app sockets, which must actually pass through the new utun.
	for _, iface := range []string{"", names[0], names[1]} {
		probeCtx, probeCancel := context.WithTimeout(ctx, 8*time.Second)
		evidence, err := exchangeTLS(probeCtx, s, iface, nil)
		probeCancel()
		if err != nil {
			return fmt.Errorf("unprivileged TLS preflight through %q: %w", iface, err)
		}
		s.Preflight = append(s.Preflight, preflight{Interface: iface, TLS: evidence})
	}
	s.Original, err = takeSnapshot(ctx)
	if err != nil {
		return err
	}
	if err := s.Original.requireAbsent(); err != nil {
		return err
	}
	if !stableSnapshots(before, s.Original) {
		return errors.New("routes or interfaces changed during preparation preflight; review the change before preparing again")
	}
	if s.TestProfile != nil && s.TestProfile.Mode == testprofile.FullIPv4 {
		if err := s.freezeFullScope(s.Original); err != nil {
			return err
		}
	}
	if err := s.validateSnapshotScope(s.Original); err != nil {
		return fmt.Errorf("test capture conflicts with an existing route: %w", err)
	}
	target, _ := netip.ParseAddrPort(s.Target)
	if capture.SystemInterface(s.Original.Routes, target.Addr()) == "" {
		return errors.New("the test destination has no effective system route")
	}
	plan, err := s.plan()
	if err != nil {
		return err
	}
	s.InstallScriptSHA256 = hashBytes([]byte(plan.Script))
	s.UninstallScriptSHA256 = hashBytes([]byte(helper.PlanUninstall().Script))
	s.CrashScriptSHA256 = hashBytes([]byte(crashPlan().Script))
	s.Dir, err = os.MkdirTemp(os.TempDir(), "pith-rootcheck-")
	if err != nil {
		return err
	}
	// Resolve the standard macOS /var -> /private/var alias once. Every later
	// write requires this private directory to remain under the current TMPDIR.
	s.Dir, err = filepath.EvalSymlinks(s.Dir)
	if err != nil {
		return err
	}
	if err := s.save(); err != nil {
		return err
	}
	if *bundle {
		if err := s.writeBundlePlan(); err != nil {
			return err
		}
	}
	if *webkit != "" {
		if err := s.writeWebKitPlan(); err != nil {
			return err
		}
	}
	if err := s.writeDNSPlan(); err != nil {
		return err
	}
	if err := s.writeQUICPlan(); err != nil {
		return err
	}
	if err := writeJSON(filepath.Join(s.Dir, "install-plan.json"), plan); err != nil {
		return err
	}
	if err := writeJSON(filepath.Join(s.Dir, "original.json"), s.Original); err != nil {
		return err
	}
	if err := writeJSON(filepath.Join(s.Dir, "preflight.json"), s.Preflight); err != nil {
		return err
	}
	if s.TestProfile != nil && s.TestProfile.Mode == testprofile.FullIPv4 {
		if err := writeJSON(filepath.Join(s.Dir, "dns-skips.json"), s.SkippedDNS); err != nil {
			return err
		}
	}
	if err := writePrivate(filepath.Join(s.Dir, "install.sh"), []byte(plan.Script)); err != nil {
		return err
	}
	undo := helper.PlanUninstall()
	if err := writePrivate(filepath.Join(s.Dir, "uninstall.sh"), []byte(undo.Script)); err != nil {
		return err
	}
	if err := writePrivate(filepath.Join(s.Dir, "crash.sh"), []byte(crashPlan().Script)); err != nil {
		return err
	}
	if err := writePrivate(filepath.Join(s.Dir, "emergency-command.txt"), []byte(emergencyCommand(undo)+"\n")); err != nil {
		return err
	}
	fmt.Println("Prepared private review material:", s.Dir)
	fmt.Println("Helper SHA-256:", s.HelperSHA256)
	fmt.Println("Capture mode:", s.captureMode())
	if s.WebKit != nil {
		fmt.Printf("Browser review: %d frozen A records; Private Relay status unknown. Establish direct Safari connectivity before running; active Private Relay does not meet this origin-capture prerequisite.\n", len(s.WebKit.Addresses))
		if s.WebKit.DNSReview != nil {
			for _, warning := range s.WebKit.DNSReview.Warnings {
				fmt.Println("WARNING:", warning)
			}
		}
	}
	for _, skipped := range s.SkippedDNS {
		fmt.Printf("DNS capture skipped for %s: %s\n", skipped.Address, skipped.Reason)
	}
	fmt.Println("Review install-plan.json, install.sh, uninstall.sh, original.json, and emergency-command.txt before authorization.")
	fmt.Println("No helper was installed and no routing was changed.")
	fmt.Println("Next command: rootcheck cancel --session " + shellQuote(s.Dir))
	return nil
}

func (s *session) scopes() ([]netip.Prefix, error) {
	if err := s.validateWebKit(); err != nil {
		return nil, err
	}
	if err := s.validateDNS(); err != nil {
		return nil, err
	}
	if err := s.validateQUIC(); err != nil {
		return nil, err
	}
	parsed, err := api.ParseTarget(s.ServerName)
	if err != nil || !parsed.IsDomain() || parsed.Subdomains || parsed.String() != s.ServerName {
		return nil, errors.New("server-name must be a plain, lowercase ASCII DNS name")
	}
	if s.TestProfile != nil {
		if s.DNSTarget != "" || s.TestProfile.Target != s.Target || s.TestProfile.ServerName != s.ServerName || s.TestProfile.Lease < 30*time.Second {
			return nil, errors.New("profile session needs the same frozen TLS endpoint, no dns-target, and a lease of at least 30s within its mode's maximum")
		}
		if err := s.TestProfile.Validate(); err != nil {
			return nil, err
		}
		return s.TestProfile.Prefixes(), nil
	}
	target, err := testAddress(s.Target)
	if err != nil {
		return nil, err
	}
	prefixes := []netip.Prefix{netip.PrefixFrom(target.Addr(), 32)}
	for _, raw := range s.dnsTargets() {
		dns, err := testAddress(raw)
		if err != nil || dns.Port() != 53 {
			return nil, errors.New("dns-target must be a public IPv4:53 endpoint")
		}
		p := netip.PrefixFrom(dns.Addr(), 32)
		if !slices.Contains(prefixes, p) {
			prefixes = append(prefixes, p)
		}
	}
	if s.QUICTarget != "" {
		endpoint, _ := netip.ParseAddrPort(s.QUICTarget)
		prefix := netip.PrefixFrom(endpoint.Addr(), 32)
		if !slices.Contains(prefixes, prefix) {
			prefixes = append(prefixes, prefix)
		}
	}
	if s.WebKit != nil {
		for _, text := range s.WebKit.Addresses {
			prefix := netip.PrefixFrom(netip.MustParseAddr(text), 32)
			if !slices.Contains(prefixes, prefix) {
				prefixes = append(prefixes, prefix)
			}
		}
	}
	if len(prefixes) > 16 {
		return nil, errors.New("all scoped stages together must fit within sixteen reviewed hosts")
	}
	return prefixes, nil
}

func testAddress(raw string) (netip.AddrPort, error) {
	a, err := netip.ParseAddrPort(raw)
	if err != nil || !a.Addr().Is4() || !a.Addr().IsGlobalUnicast() || a.Addr().IsPrivate() || a.Port() == 0 || netip.MustParsePrefix("100.64.0.0/10").Contains(a.Addr()) {
		return netip.AddrPort{}, errors.New("this smoke harness accepts only a public, literal IPv4:port; no subnet or IPv6 capture")
	}
	return a, nil
}

func (s *session) plan() (helper.InstallPlan, error) {
	scopes, err := s.scopes()
	if err != nil {
		return helper.InstallPlan{}, err
	}
	if s.TestProfile != nil {
		return helper.PlanTestInstall(s.Helper, uint32(s.UID), *s.TestProfile)
	}
	return helper.PlanScopedInstall(s.Helper, uint32(s.UID), helper.ScopedOptions{Prefixes: scopes, ProbeAddress: s.Target, ProbeServerName: s.ServerName})
}

func (s *session) reviewedPlan(ctx context.Context) (helper.InstallPlan, error) {
	if err := ctx.Err(); err != nil {
		return helper.InstallPlan{}, err
	}
	checksum, err := fileHash(s.Helper)
	if err != nil || checksum != s.HelperSHA256 {
		return helper.InstallPlan{}, errors.New("helper changed after preparation; create and review a new session")
	}
	plan, err := s.plan()
	if err != nil {
		return helper.InstallPlan{}, err
	}
	if hashBytes([]byte(plan.Script)) != s.InstallScriptSHA256 {
		return helper.InstallPlan{}, errors.New("installation plan changed after preparation; create and review a new session")
	}
	return plan, nil
}

func loadSession(dir string) (*session, error) {
	if err := privateTempDirectory(dir); err != nil {
		return nil, err
	}
	data, err := os.ReadFile(filepath.Join(dir, "session.json"))
	if err != nil {
		return nil, err
	}
	var s session
	if err := json.Unmarshal(data, &s); err != nil {
		return nil, err
	}
	if s.Dir != filepath.Clean(dir) || s.UID != os.Getuid() || s.Version == "" || !filepath.IsAbs(s.Helper) || len(s.HelperSHA256) != 64 || len(s.InstallScriptSHA256) != 64 {
		return nil, errors.New("session location, owner, executable, or checksums do not match")
	}
	if _, err := s.scopes(); err != nil {
		return nil, err
	}
	return &s, nil
}

func privateTempDirectory(dir string) error {
	if !filepath.IsAbs(dir) {
		return errors.New("session directory must be an absolute path below TMPDIR")
	}
	tmp, err := filepath.EvalSymlinks(os.TempDir())
	if err != nil {
		return err
	}
	actual, err := filepath.EvalSymlinks(dir)
	if err != nil {
		return err
	}
	rel, err := filepath.Rel(tmp, actual)
	if err != nil || rel == "." || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) || filepath.Clean(dir) != actual {
		return errors.New("session must be a real directory strictly below the current TMPDIR")
	}
	info, err := os.Lstat(dir)
	if err != nil {
		return err
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !info.IsDir() || info.Mode().Perm() != 0700 || !ok || stat.Uid != uint32(os.Getuid()) {
		return errors.New("session must be a private mode-0700 directory owned by the installing user")
	}
	return nil
}

func (s *session) save() error {
	if err := privateTempDirectory(s.Dir); err != nil {
		return err
	}
	return writeJSON(filepath.Join(s.Dir, "session.json"), s)
}

func (s *session) lock() (*os.File, error) {
	f, err := os.OpenFile(filepath.Join(s.Dir, ".lock"), os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return nil, err
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		f.Close()
		return nil, errors.New("another command is using this session; interrupt it normally or use the emergency command")
	}
	return f, nil
}

func (s *session) verifyInstalled() error {
	checksum, err := fileHash(helper.BinaryPath)
	if err != nil || checksum != s.HelperSHA256 {
		return errors.New("installed helper is not the reviewed executable")
	}
	return s.verifyInstalledPlist(helper.PlistPath)
}

func (s *session) verifyInstalledPlist(path string) error {
	data, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	var launch struct {
		Label            string
		ProgramArguments []string
	}
	if _, err := plist.Unmarshal(data, &launch); err != nil {
		return err
	}
	want := []string{helper.BinaryPath, "--owner-uid", strconv.Itoa(s.UID), "--safe-target", s.Target, "--safe-server-name", s.ServerName}
	scopes, err := s.scopes()
	if err != nil {
		return err
	}
	if s.TestProfile != nil {
		args, err := s.TestProfile.Arguments()
		if err != nil {
			return err
		}
		want = append([]string{helper.BinaryPath, "--owner-uid", strconv.Itoa(s.UID)}, args...)
	} else {
		for _, p := range scopes {
			want = append(want, "--capture", p.String())
		}
	}
	if launch.Label != helper.Label || !slices.Equal(launch.ProgramArguments, want) {
		return errors.New("installed launchd arguments do not match this session's reviewed capture mode and limits")
	}
	return nil
}

func takeSnapshot(ctx context.Context) (snapshot, error) {
	s := snapshot{At: time.Now()}
	var err error
	s.BootSession, err = unix.Sysctl("kern.bootsessionuuid")
	if err != nil {
		return s, err
	}
	s.Interfaces, err = netif.List(ctx)
	if err != nil {
		return s, err
	}
	ifaces, err := net.Interfaces()
	if err != nil {
		return s, err
	}
	s.InterfaceIndices = make(map[string]int, len(ifaces))
	s.InterfaceAddresses = make(map[string][]netip.Prefix, len(ifaces))
	for _, iface := range ifaces {
		s.InterfaceIndices[iface.Name] = iface.Index
		addresses, err := iface.Addrs()
		if err != nil {
			return s, err
		}
		s.InterfaceAddresses[iface.Name] = nil
		for _, address := range addresses {
			prefix, err := netip.ParsePrefix(address.String())
			if err != nil {
				return s, fmt.Errorf("read interface address: %w", err)
			}
			s.InterfaceAddresses[iface.Name] = append(s.InterfaceAddresses[iface.Name], prefix)
		}
	}
	s.Routes, err = capture.Snapshot()
	if err != nil {
		return s, err
	}
	for _, path := range managedPaths() {
		p := pathState{Path: path}
		info, err := os.Lstat(path)
		switch {
		case err == nil:
			p.Present, p.Mode = true, info.Mode()
		case errors.Is(err, os.ErrNotExist):
		default:
			p.Error = err.Error()
		}
		s.Paths = append(s.Paths, p)
	}
	commandCtx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	cmd := exec.CommandContext(commandCtx, "/bin/launchctl", "print", "system/"+helper.Label)
	cmd.Env = []string{"PATH=/usr/bin:/bin:/usr/sbin:/sbin", "LC_ALL=C"}
	output, err := cmd.CombinedOutput()
	s.Launchd = string(output)
	if commandCtx.Err() != nil {
		return s, commandCtx.Err()
	}
	if err != nil {
		var exit *exec.ExitError
		if !errors.As(err, &exit) {
			return s, err
		}
		s.LaunchExit = exit.ExitCode()
	}
	return s, nil
}

func managedPaths() [5]string {
	return [5]string{helper.BinaryPath, helper.PlistPath, helper.DataDir, filepath.Join(helper.DataDir, "capture.json"), helper.SocketPath}
}

func (s snapshot) requireAbsent() error {
	expected := managedPaths()
	if len(s.Paths) != len(expected) {
		return errors.New("absence verification requires a complete snapshot of the five managed paths")
	}
	seen := make(map[string]bool, len(expected))
	for _, path := range s.Paths {
		if !slices.Contains(expected[:], path.Path) || seen[path.Path] {
			return errors.New("managed path snapshot contains an unexpected or duplicate path")
		}
		seen[path.Path] = true
		if path.Present || path.Error != "" {
			return fmt.Errorf("managed path already exists or cannot be inspected: %s; this harness never replaces an existing installation", path.Path)
		}
	}
	// launchctl output includes mutable diagnostic text. Only service absence
	// matters here; snapshot acquisition errors are returned by takeSnapshot.
	if s.LaunchExit == 0 {
		return errors.New("the managed launchd service already exists")
	}
	return nil
}

func (s *session) expectOriginal(ctx context.Context, phase string) error {
	return s.expectOriginalWithOps(ctx, phase, liveOps())
}

func (s *session) expectOriginalWithOps(ctx context.Context, phase string, ops rootcheckOps) error {
	after, err := ops.snapshot(ctx)
	if err != nil {
		return err
	}
	if _, err := ops.archive(s, phase, after); err != nil {
		return err
	}
	if err := after.requireAbsent(); err != nil {
		return err
	}
	if !sameCleanupBaseline(s.Original, after) {
		return errors.New("routes or tunnel inventory differ from the current baseline; inspect the private snapshots before continuing")
	}
	return nil
}

func sameRoutes(a, b []capture.Route) bool {
	keys := func(routes []capture.Route) []string {
		result := make([]string, 0, len(routes))
		for _, route := range routes {
			data, _ := json.Marshal(route)
			result = append(result, string(data))
		}
		slices.Sort(result)
		return result
	}
	return slices.Equal(keys(a), keys(b))
}

func sameTunnels(a, b []api.Interface) bool {
	names := func(ifaces []api.Interface) []string {
		var result []string
		for _, iface := range ifaces {
			if strings.HasPrefix(iface.Name, "utun") {
				result = append(result, iface.Name)
			}
		}
		slices.Sort(result)
		return result
	}
	return reflect.DeepEqual(names(a), names(b))
}

func writeJSON(path string, value any) error {
	data, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return err
	}
	return writePrivate(path, append(data, '\n'))
}

func writePrivate(path string, data []byte) error {
	f, err := os.CreateTemp(filepath.Dir(path), ".rootcheck-")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	if _, err := f.Write(data); err != nil {
		f.Close()
		return err
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	return os.Rename(f.Name(), path)
}

func fileHash(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return "", err
	}
	if !info.Mode().IsRegular() {
		return "", errors.New("expected a regular executable file")
	}
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

func hashBytes(data []byte) string {
	h := sha256.Sum256(data)
	return hex.EncodeToString(h[:])
}

func shellQuote(s string) string { return "'" + strings.ReplaceAll(s, "'", "'\"'\"'") + "'" }

func emergencyCommand(plan helper.InstallPlan) string {
	appleScript := "do shell script " + strconv.Quote(plan.Script) + " with administrator privileges with prompt " + strconv.Quote(plan.Prompt)
	return "/usr/bin/osascript -e " + shellQuote(appleScript)
}
