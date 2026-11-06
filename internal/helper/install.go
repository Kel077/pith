package helper

import (
	"context"
	"crypto/sha256"
	"fmt"
	"io"
	"net/netip"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"

	"github.com/Kel077/pith/internal/api"
	"github.com/Kel077/pith/internal/helper/identity"
	"github.com/Kel077/pith/internal/testprofile"
	"howett.net/plist"
)

// InstallPlan is reviewable without privileges. Generating it only reads the
// supplied executable; Execute is the only operation that opens a password
// dialog or modifies the system. Update failures restore the previous files
// and the previous launchd registration.
type InstallPlan struct {
	Script           string
	Prompt           string
	Changes          []string
	CreatesIfMissing []string // Shared parent directories; existing metadata is never changed.
	Undo             string
}

const (
	InstallPrompt   = "Pith needs your password to install its routing helper."
	UpdatePrompt    = "Pith needs your password to update its routing helper."
	UninstallPrompt = "Pith needs your password to remove its routing helper."
)

func shellQuote(s string) string { return "'" + strings.ReplaceAll(s, "'", "'\"'\"'") + "'" }

func launchPlist(uid uint32, extraArgs ...string) ([]byte, error) {
	return plist.MarshalIndent(map[string]any{
		"Label":            Label,
		"ProgramArguments": append([]string{BinaryPath, "--owner-uid", strconv.FormatUint(uint64(uid), 10)}, extraArgs...),
		"RunAtLoad":        true, "KeepAlive": true, "ProcessType": "Background", "ThrottleInterval": 10, "ExitTimeOut": 5,
	}, plist.XMLFormat, "  ")
}

// PlanVerifiedInstall pins the source to the client's compiled identity before
// authorization and again after the privileged copy. Cleanup uses that copy.
// A separately verified installed BinaryPath can repair launchd registration
// without a bundled source; callers still verify its installed-file metadata.
func PlanVerifiedInstall(source string, uid uint32, expected identity.Identity) (InstallPlan, error) {
	if err := expected.Validate(); err != nil || expected.Protocol != ProtocolVersion {
		return InstallPlan{}, &api.Error{Code: api.ErrHelperUntrusted, Detail: "invalid helper identity"}
	}
	plan, err := planInstallChecked(source, uid, nil, expected.SHA256)
	if err != nil {
		return InstallPlan{}, &api.Error{Code: api.ErrHelperUntrusted, Detail: "helper source could not be verified before authorization"}
	}
	return plan, nil
}

// ScopedOptions restricts an explicitly authorized development installation to
// individual test hosts. It is never used by the application's installer.
type ScopedOptions struct {
	Prefixes        []netip.Prefix
	ProbeAddress    string // literal IP:port, included in Prefixes
	ProbeServerName string // certificate-verified TLS name
}

// PlanScopedInstall has the same review/authorization boundary as PlanVerifiedInstall.
// Only /32 and /128 capture routes are accepted; launchd still starts idle.
func PlanScopedInstall(source string, uid uint32, opts ScopedOptions) (InstallPlan, error) {
	if len(opts.Prefixes) == 0 || len(opts.Prefixes) > 16 {
		return InstallPlan{}, fmt.Errorf("scoped installation needs one to sixteen test hosts")
	}
	probe, err := netip.ParseAddrPort(opts.ProbeAddress)
	if err != nil || probe.Port() == 0 || !probe.Addr().IsGlobalUnicast() || opts.ProbeServerName == "" || strings.ContainsAny(opts.ProbeServerName, "\x00\r\n") {
		return InstallPlan{}, fmt.Errorf("scoped installation needs a TLS test IP:port and server name")
	}
	args := []string{"--safe-target", opts.ProbeAddress, "--safe-server-name", opts.ProbeServerName}
	containsProbe := false
	for _, p := range opts.Prefixes {
		if !p.IsValid() || p.Bits() != p.Addr().BitLen() || !p.Addr().IsGlobalUnicast() || p.Addr().Is4In6() {
			return InstallPlan{}, fmt.Errorf("scoped installation accepts only unicast /32 or /128 test hosts")
		}
		containsProbe = containsProbe || p.Contains(probe.Addr())
		args = append(args, "--capture", p.String())
	}
	if !containsProbe {
		return InstallPlan{}, fmt.Errorf("SafeStart address is outside the test capture")
	}
	return planInstall(source, uid, args)
}

// PlanTestInstall is visibly distinct from both host-scoped and production
// installs. The profile is immutable launchd configuration, enforced by pithd.
func PlanTestInstall(source string, uid uint32, profile testprofile.Profile) (InstallPlan, error) {
	args, err := profile.Arguments()
	if err != nil {
		return InstallPlan{}, err
	}
	plan, err := planInstall(source, uid, args)
	if err != nil {
		return InstallPlan{}, err
	}
	plan.Prompt = "Pith needs your password to install its full IPv4 test helper (at most 120 seconds per capture)."
	plan.Undo += " Test capture has a helper-enforced " + profile.Lease.String() + " lease; Stop removes only the profile's owned capture routes and utun."
	if profile.Mode == testprofile.HostIPv6 {
		plan.Prompt = "Pith needs your password to install its IPv6 host test helper (at most 60 seconds per capture)."
	}
	return plan, nil
}

func planInstall(source string, uid uint32, args []string) (InstallPlan, error) {
	return planInstallChecked(source, uid, args, "")
}

func planInstallChecked(source string, uid uint32, args []string, expectedHash string) (InstallPlan, error) {
	if uid == 0 {
		return InstallPlan{}, fmt.Errorf("the helper must be installed for an unprivileged user")
	}
	if !filepath.IsAbs(source) || strings.ContainsAny(source, "\x00\r\n") {
		return InstallPlan{}, fmt.Errorf("helper source must be an absolute single-line path")
	}
	before, err := os.Lstat(source)
	if err != nil {
		return InstallPlan{}, err
	}
	if !validHelperSource(before) {
		return InstallPlan{}, fmt.Errorf("helper source must be a bounded regular executable not writable by group or others")
	}
	f, err := os.Open(source)
	if err != nil {
		return InstallPlan{}, err
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return InstallPlan{}, err
	}
	if !os.SameFile(before, info) || !validHelperSource(info) {
		return InstallPlan{}, fmt.Errorf("helper source changed while opening")
	}
	hash := sha256.New()
	if _, err := io.Copy(hash, f); err != nil {
		return InstallPlan{}, err
	}
	checksum := fmt.Sprintf("%x", hash.Sum(nil))
	if expectedHash != "" && checksum != expectedHash {
		return InstallPlan{}, &api.Error{Code: api.ErrHelperUntrusted, Detail: "helper source changed before authorization"}
	}
	launch, err := launchPlist(uid, args...)
	if err != nil {
		return InstallPlan{}, err
	}
	// All destination paths are fixed. The source is shell-quoted and the copied
	// root-owned stage must match the bytes reviewed before elevation.
	cleanup := ""
	if expectedHash != "" {
		cleanup = "if [ -f \"$data/capture.json\" ]; then \"$stage/new-helper\" --cleanup; fi\n"
	}
	script := "set -eu\n" +
		"binary=" + shellQuote(BinaryPath) + "\nplist=" + shellQuote(PlistPath) + "\ndata=" + shellQuote(DataDir) + "\n" +
		"service=" + shellQuote("system/"+Label) + "\n" +
		createSharedDirectory(filepath.Dir(BinaryPath), "1755") +
		createSharedDirectory(filepath.Dir(PlistPath), "0755") +
		"for path in \"$binary\" \"$plist\" \"$data\"; do [ ! -L \"$path\" ] || { echo 'refusing symlink installation path' >&2; exit 1; }; done\n" +
		"for path in \"$binary\" \"$plist\"; do if [ -e \"$path\" ]; then [ -f \"$path\" ] && [ \"$(/usr/bin/stat -f %u \"$path\")\" = 0 ] || exit 1; fi; done\n" +
		"if [ -e \"$data\" ]; then [ -d \"$data\" ] && [ \"$(/usr/bin/stat -f %u \"$data\")\" = 0 ] || exit 1; fi\n" +
		"/usr/bin/install -d -o root -g wheel -m 0700 \"$data\"\n" +
		"stage=$(/usr/bin/mktemp -d \"$data/.install.XXXXXXXX\")\n" +
		"had_binary=0\nhad_plist=0\nwas_loaded=0\nchanged=0\n" +
		"rollback() {\n  code=$?\n  set +e\n  trap - EXIT HUP INT TERM\n  if [ \"$code\" -ne 0 ] && [ \"$changed\" -eq 1 ]; then\n" +
		"    /bin/launchctl bootout \"$service\" >/dev/null 2>&1 || true\n" +
		"    if [ \"$had_binary\" -eq 1 ]; then /bin/mv -f \"$stage/old-helper\" \"$binary\"; else /bin/rm -f \"$binary\"; fi\n" +
		"    if [ \"$had_plist\" -eq 1 ]; then /bin/mv -f \"$stage/old.plist\" \"$plist\"; else /bin/rm -f \"$plist\"; fi\n" +
		"    if [ \"$was_loaded\" -eq 1 ] && [ \"$had_plist\" -eq 1 ]; then /bin/launchctl bootstrap system \"$plist\" || true; fi\n" +
		"  fi\n  /bin/rm -rf \"$stage\"\n  exit \"$code\"\n}\ntrap rollback EXIT\ntrap 'exit 1' HUP INT TERM\n" +
		"[ ! -f \"$binary\" ] || { /bin/cp -p \"$binary\" \"$stage/old-helper\"; had_binary=1; }\n" +
		"[ ! -f \"$plist\" ] || { /bin/cp -p \"$plist\" \"$stage/old.plist\"; had_plist=1; }\n" +
		"if /bin/launchctl print \"$service\" >/dev/null 2>&1; then was_loaded=1; fi\n" +
		"/usr/bin/install -o root -g wheel -m 0755 " + shellQuote(source) + " \"$stage/new-helper\"\n" +
		"actual=$(/usr/bin/shasum -a 256 \"$stage/new-helper\")\n[ \"${actual%% *}\" = " + shellQuote(checksum) + " ] || { echo 'helper changed during authorization' >&2; exit 1; }\n" +
		"/bin/cat >\"$stage/new.plist\" <<'PITH_LAUNCH_PLIST'\n" + string(launch) + "\nPITH_LAUNCH_PLIST\n" +
		"/usr/sbin/chown root:wheel \"$stage/new.plist\"\n/bin/chmod 0644 \"$stage/new.plist\"\n/usr/bin/plutil -lint \"$stage/new.plist\" >/dev/null\n" +
		"changed=1\nif [ \"$was_loaded\" -eq 1 ]; then /bin/launchctl bootout \"$service\"; fi\n" +
		waitForSocket() +
		cleanup +
		"/bin/mv -f \"$stage/new-helper\" \"$binary\"\n/bin/mv -f \"$stage/new.plist\" \"$plist\"\n" +
		"/bin/launchctl bootstrap system \"$plist\"\n"
	return InstallPlan{
		Script: script, Prompt: InstallPrompt, Changes: []string{BinaryPath, PlistPath, DataDir, SocketPath},
		CreatesIfMissing: []string{filepath.Dir(BinaryPath), filepath.Dir(PlistPath)},
		Undo:             "UninstallHelper removes the service and helper data; a failed update restores the previous executable, plist and launchd registration. Shared parent directories are retained, and existing parent permissions and ownership are never changed.",
	}, nil
}

func validHelperSource(info os.FileInfo) bool {
	return info.Mode().IsRegular() && info.Mode().Perm()&0111 != 0 && info.Mode().Perm()&0022 == 0 && info.Size() > 0 && info.Size() <= identity.MaxFileSize
}

// install -d changes metadata even when a directory already exists. Only a
// successful exclusive mkdir permits setting ownership: another installer can
// create the shared directory between the existence check and mkdir.
func createSharedDirectory(path, mode string) string {
	quoted := shellQuote(path)
	return "[ ! -L " + quoted + " ] || { echo 'refusing symlink parent directory' >&2; exit 1; }\n" +
		"if [ ! -e " + quoted + " ]; then\n" +
		"  if /bin/mkdir -m " + mode + " " + quoted + " 2>/dev/null; then\n" +
		"    /usr/sbin/chown root:wheel " + quoted + "\n" +
		"  else\n" +
		"    [ ! -L " + quoted + " ] && [ -d " + quoted + " ] || { echo 'could not create installation parent' >&2; exit 1; }\n" +
		"  fi\n" +
		"else\n" +
		"  [ -d " + quoted + " ] || { echo 'installation parent is not a directory' >&2; exit 1; }\n" +
		"fi\n" +
		"pith_parent_owner=$(/usr/bin/stat -f %u " + quoted + ")\n" +
		"pith_parent_mode=$(/usr/bin/stat -f %OLp " + quoted + ")\n" +
		"[ \"$pith_parent_owner\" = 0 ] && [ \"$((0$pith_parent_mode & 022))\" -eq 0 ] || { echo 'installation parent must be root-owned and not writable by group or others' >&2; exit 1; }\n"
}

// PlanVerifiedUninstall never executes the installed path for recovery. The
// source can be the verified bundle or a separately verified installed file;
// either is copied and hash-checked in a private root-owned stage first.
// Cleanup is a CLI operation, so a frozen identity may use a different IPC protocol.
func PlanVerifiedUninstall(source string, expected identity.Identity) (InstallPlan, error) {
	if err := expected.Validate(); err != nil {
		return InstallPlan{}, &api.Error{Code: api.ErrHelperUntrusted, Detail: "invalid cleanup helper identity"}
	}
	if !filepath.IsAbs(source) || strings.ContainsAny(source, "\x00\r\n") {
		return InstallPlan{}, &api.Error{Code: api.ErrHelperUntrusted, Detail: "invalid cleanup helper path"}
	}
	before, err := os.Lstat(source)
	if err != nil || !validHelperSource(before) {
		return InstallPlan{}, &api.Error{Code: api.ErrHelperUntrusted, Detail: "invalid cleanup helper file"}
	}
	f, err := os.Open(source)
	if err != nil {
		return InstallPlan{}, &api.Error{Code: api.ErrHelperUntrusted, Detail: "cleanup helper could not be opened before authorization"}
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil || !os.SameFile(before, info) || !validHelperSource(info) {
		return InstallPlan{}, &api.Error{Code: api.ErrHelperUntrusted, Detail: "invalid cleanup helper file"}
	}
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return InstallPlan{}, &api.Error{Code: api.ErrHelperUntrusted, Detail: "cleanup helper could not be read before authorization"}
	}
	if fmt.Sprintf("%x", h.Sum(nil)) != expected.SHA256 {
		return InstallPlan{}, &api.Error{Code: api.ErrHelperUntrusted, Detail: "cleanup helper changed before authorization"}
	}
	script := "set -eu\nservice=" + shellQuote("system/"+Label) + "\nplist=" + shellQuote(PlistPath) + "\ndata=" + shellQuote(DataDir) + "\n" +
		"for path in " + shellQuote(BinaryPath) + " \"$plist\" \"$data\"; do [ ! -L \"$path\" ] || { echo 'refusing symlink installation path' >&2; exit 1; }; done\n" +
		"if [ -e \"$data\" ]; then [ -d \"$data\" ] && [ \"$(/usr/bin/stat -f %u \"$data\")\" = 0 ] || exit 1; fi\n" +
		"/usr/bin/install -d -o root -g wheel -m 0700 \"$data\"\n" +
		"stage=$(/usr/bin/mktemp -d \"$data/.uninstall.XXXXXXXX\")\n" +
		"was_loaded=0\nchanged=0\ncleanup_done=0\n" +
		"rollback() {\n  code=$?\n  set +e\n  trap - EXIT HUP INT TERM\n" +
		"  if [ \"$code\" -ne 0 ] && [ \"$changed\" -eq 1 ] && [ \"$was_loaded\" -eq 1 ] && [ \"$cleanup_done\" -eq 0 ]; then\n" +
		"    if ! /bin/launchctl print \"$service\" >/dev/null 2>&1; then /bin/launchctl bootstrap system \"$plist\" || true; fi\n" +
		"  fi\n  /bin/rm -rf \"$stage\"\n  exit \"$code\"\n}\ntrap rollback EXIT\ntrap 'exit 1' HUP INT TERM\n" +
		"/usr/bin/install -o root -g wheel -m 0755 " + shellQuote(source) + " \"$stage/cleanup-helper\"\n" +
		"actual=$(/usr/bin/shasum -a 256 \"$stage/cleanup-helper\")\n[ \"${actual%% *}\" = " + shellQuote(expected.SHA256) + " ] || { echo 'cleanup helper changed during authorization' >&2; exit 1; }\n" +
		"if /bin/launchctl print \"$service\" >/dev/null 2>&1; then\n" +
		"  [ -f \"$plist\" ] && [ \"$(/usr/bin/stat -f %u \"$plist\")\" = 0 ] || { echo 'cannot preserve the loaded helper registration' >&2; exit 1; }\n" +
		"  pith_plist_mode=$(/usr/bin/stat -f %OLp \"$plist\")\n" +
		"  [ \"$((0$pith_plist_mode & 022))\" -eq 0 ] || { echo 'refusing writable helper registration' >&2; exit 1; }\n" +
		"  was_loaded=1\nfi\n" +
		"changed=1\nif [ \"$was_loaded\" -eq 1 ]; then /bin/launchctl bootout \"$service\"; fi\n" +
		waitForSocket() +
		"if [ -f \"$data/capture.json\" ]; then \"$stage/cleanup-helper\" --cleanup; fi\n" +
		"cleanup_done=1\n" +
		"/bin/rm -f " + shellQuote(BinaryPath) + " " + shellQuote(PlistPath) + "\n" +
		"if [ -S " + shellQuote(SocketPath) + " ]; then /bin/rm -f " + shellQuote(SocketPath) + "; fi\n" +
		"/bin/rm -rf \"$data\"\n"
	return InstallPlan{Script: script, Prompt: UninstallPrompt, Changes: []string{BinaryPath, PlistPath, DataDir, SocketPath}, Undo: "InstallHelper reinstalls the bundled helper; removed logs and traffic history are not restored. Cleanup failures preserve the executable, plist and ownership journal and attempt to restore the service only if it was previously loaded."}, nil
}

// A bounded grace period lets the old helper finish route cleanup before its
// executable or data disappears. A crash can leave a stale socket, which the
// next helper recognizes using a refused connection rather than blindly unlinking.
func waitForSocket() string {
	return "attempt=0\nwhile [ -S " + shellQuote(SocketPath) + " ] && [ \"$attempt\" -lt 60 ]; do /bin/sleep 0.1; attempt=$((attempt+1)); done\n"
}

func (p InstallPlan) Execute(ctx context.Context) error {
	if runtime.GOOS != "darwin" {
		return fmt.Errorf("helper installation requires macOS")
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	// Pass one AppleScript argument, with a correctly escaped string literal;
	// neither a shell nor AppleScript re-parses user paths as executable code.
	output, err := exec.CommandContext(ctx, "/usr/bin/osascript", "-e", p.appleScript()).CombinedOutput()
	if ctx.Err() != nil {
		return ctx.Err()
	}
	if err == nil {
		return nil
	}
	if strings.Contains(string(output), "(-128)") {
		return &api.Error{Code: api.ErrCanceled}
	}
	return fmt.Errorf("helper installation: %w: %s", err, strings.TrimSpace(string(output)))
}

func (p InstallPlan) appleScript() string {
	return "do shell script " + strconv.Quote(p.Script) + " with administrator privileges with prompt " + strconv.Quote(p.Prompt)
}
