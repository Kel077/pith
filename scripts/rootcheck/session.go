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
	"howett.net/plist"
)

type session struct {
	Dir          string              `json:"directory"`
	UID          int                 `json:"uid"`
	Helper       string              `json:"helper"`
	HelperSHA256 string              `json:"helperSHA256"`
	Version      string              `json:"version"`
	Target       string              `json:"target"`
	ServerName   string              `json:"serverName"`
	DNSTarget    string              `json:"dnsTarget,omitempty"`
	Interfaces   [2]api.InterfaceRef `json:"interfaces"`
	Preflight    []preflight         `json:"preflight"`
	// Original is the active rollback baseline; original.json is never replaced.
	Original               snapshot        `json:"original"`
	BaselineLabel          string          `json:"baselineLabel"`
	BaselineFile           string          `json:"baselineFile"`
	Installation           *installationID `json:"installation,omitempty"`
	Cleanup                *cleanupProof   `json:"cleanup,omitempty"`
	InstallScriptSHA256    string          `json:"installScriptSHA256"`
	AuthorizationAttempted bool            `json:"authorizationAttempted"`
	CancelPassed           bool            `json:"cancelPassed"`
	Installed              bool            `json:"installed"`
	SmokePassed            bool            `json:"smokePassed"`
}

type pathState struct {
	Path    string      `json:"path"`
	Present bool        `json:"present"`
	Mode    os.FileMode `json:"mode,omitempty"`
	Error   string      `json:"error,omitempty"`
}

type snapshot struct {
	At         time.Time       `json:"at"`
	Interfaces []api.Interface `json:"interfaces"`
	Routes     []capture.Route `json:"routes"`
	Paths      []pathState     `json:"paths"`
	Launchd    string          `json:"launchdOutput"`
	LaunchExit int             `json:"launchdExit"`
}

func prepare(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("prepare", flag.ContinueOnError)
	s := &session{UID: os.Getuid(), BaselineLabel: "initial", BaselineFile: "original.json"}
	var names [2]string
	fs.StringVar(&s.Helper, "helper", "", "absolute path to the reviewed helper executable")
	fs.StringVar(&s.Version, "version", "dev", "expected helper build version")
	fs.StringVar(&names[0], "interface-a", "", "first interface BSD name, chosen by the user")
	fs.StringVar(&names[1], "interface-b", "", "second interface BSD name, chosen by the user")
	fs.StringVar(&s.Target, "target", "", "public TLS test IPv4:port, chosen by the user")
	fs.StringVar(&s.ServerName, "server-name", "", "DNS name on the test endpoint's TLS certificate")
	fs.StringVar(&s.DNSTarget, "dns-target", "", "optional public DNS test IPv4:53, for manual checks")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() != 0 || !filepath.IsAbs(s.Helper) || names[0] == "" || names[0] == names[1] || names[1] == "" || s.Version == "" {
		return errors.New("supply an absolute helper path, two distinct interface names, a target, a TLS name, and the helper version")
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
		if iface == nil || !iface.Up || len(iface.IPv4) == 0 {
			return fmt.Errorf("interface %q is absent, down, or has no IPv4 address", name)
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
	plan, err := s.plan()
	if err != nil {
		return err
	}
	s.InstallScriptSHA256 = hashBytes([]byte(plan.Script))
	// Ordinary, interface-bound sockets are a preflight only. The check command
	// uses unbound app sockets, which must actually pass through the new utun.
	for _, iface := range []string{"", names[0], names[1]} {
		evidence, err := exchangeTLS(ctx, s, iface, nil)
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
	scopes, _ := s.scopes()
	if _, err := capture.Build(s.Original.Routes, nil, false, scopes); err != nil {
		return fmt.Errorf("test capture conflicts with an existing route: %w", err)
	}
	if capture.SystemInterface(s.Original.Routes, scopes[0].Addr()) == "" {
		return errors.New("the test destination has no effective system route")
	}
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
	if err := writeJSON(filepath.Join(s.Dir, "install-plan.json"), plan); err != nil {
		return err
	}
	if err := writeJSON(filepath.Join(s.Dir, "original.json"), s.Original); err != nil {
		return err
	}
	if err := writeJSON(filepath.Join(s.Dir, "preflight.json"), s.Preflight); err != nil {
		return err
	}
	if err := writePrivate(filepath.Join(s.Dir, "install.sh"), []byte(plan.Script)); err != nil {
		return err
	}
	undo := helper.PlanUninstall()
	if err := writePrivate(filepath.Join(s.Dir, "uninstall.sh"), []byte(undo.Script)); err != nil {
		return err
	}
	if err := writePrivate(filepath.Join(s.Dir, "emergency-command.txt"), []byte(emergencyCommand(undo)+"\n")); err != nil {
		return err
	}
	fmt.Println("Prepared private review material:", s.Dir)
	fmt.Println("Helper SHA-256:", s.HelperSHA256)
	fmt.Println("Review install-plan.json, install.sh, uninstall.sh, original.json, and emergency-command.txt before authorization.")
	fmt.Println("No helper was installed and no routing was changed.")
	fmt.Println("Next command: rootcheck cancel --session " + shellQuote(s.Dir))
	return nil
}

func (s *session) scopes() ([]netip.Prefix, error) {
	target, err := testAddress(s.Target)
	if err != nil {
		return nil, err
	}
	parsed, err := api.ParseTarget(s.ServerName)
	if err != nil || !parsed.IsDomain() || parsed.Subdomains || parsed.String() != s.ServerName {
		return nil, errors.New("server-name must be a plain, lowercase ASCII DNS name")
	}
	prefixes := []netip.Prefix{netip.PrefixFrom(target.Addr(), 32)}
	if s.DNSTarget != "" {
		dns, err := testAddress(s.DNSTarget)
		if err != nil || dns.Port() != 53 {
			return nil, errors.New("dns-target must be a public IPv4:53 endpoint")
		}
		p := netip.PrefixFrom(dns.Addr(), 32)
		if p != prefixes[0] {
			prefixes = append(prefixes, p)
		}
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
	for _, p := range scopes {
		want = append(want, "--capture", p.String())
	}
	if launch.Label != helper.Label || !slices.Equal(launch.ProgramArguments, want) {
		return errors.New("installed launchd arguments do not restrict capture to this session's reviewed hosts")
	}
	return nil
}

func takeSnapshot(ctx context.Context) (snapshot, error) {
	s := snapshot{At: time.Now()}
	var err error
	s.Interfaces, err = netif.List(ctx)
	if err != nil {
		return s, err
	}
	s.Routes, err = capture.Snapshot()
	if err != nil {
		return s, err
	}
	for _, path := range []string{helper.BinaryPath, helper.PlistPath, helper.DataDir, filepath.Join(helper.DataDir, "capture.json"), helper.SocketPath} {
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

func (s snapshot) requireAbsent() error {
	for _, path := range s.Paths {
		if path.Present || path.Error != "" {
			return fmt.Errorf("managed path already exists or cannot be inspected: %s; this harness never replaces an existing installation", path.Path)
		}
	}
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
	if !sameRoutes(s.Original.Routes, after.Routes) || !sameTunnels(s.Original.Interfaces, after.Interfaces) {
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
