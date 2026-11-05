package helper

import (
	"context"
	"crypto/sha256"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"

	"github.com/Kel077/pith/internal/api"
	"howett.net/plist"
)

// InstallPlan is reviewable without privileges. Generating it only reads the
// supplied executable; Execute is the only operation that opens a password
// dialog or modifies the system. Update failures restore the previous files
// and the previous launchd registration.
type InstallPlan struct {
	Script  string
	Changes []string
	Undo    string
}

func shellQuote(s string) string { return "'" + strings.ReplaceAll(s, "'", "'\"'\"'") + "'" }

func launchPlist(uid uint32) ([]byte, error) {
	return plist.MarshalIndent(map[string]any{
		"Label":            Label,
		"ProgramArguments": []string{BinaryPath, "--owner-uid", strconv.FormatUint(uint64(uid), 10)},
		"RunAtLoad":        true, "KeepAlive": true, "ProcessType": "Background", "ThrottleInterval": 10, "ExitTimeOut": 5,
	}, plist.XMLFormat, "  ")
}

func PlanInstall(source string, uid uint32) (InstallPlan, error) {
	if uid == 0 {
		return InstallPlan{}, fmt.Errorf("the helper must be installed for an unprivileged user")
	}
	if !filepath.IsAbs(source) || strings.ContainsAny(source, "\x00\r\n") {
		return InstallPlan{}, fmt.Errorf("helper source must be an absolute single-line path")
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
	if !info.Mode().IsRegular() || info.Mode().Perm()&0111 == 0 {
		return InstallPlan{}, fmt.Errorf("helper source is not an executable regular file")
	}
	hash := sha256.New()
	if _, err := io.Copy(hash, f); err != nil {
		return InstallPlan{}, err
	}
	checksum := fmt.Sprintf("%x", hash.Sum(nil))
	launch, err := launchPlist(uid)
	if err != nil {
		return InstallPlan{}, err
	}
	// All destination paths are fixed. The source is shell-quoted and the copied
	// root-owned stage must match the bytes reviewed before elevation.
	script := "set -eu\n" +
		"binary=" + shellQuote(BinaryPath) + "\nplist=" + shellQuote(PlistPath) + "\ndata=" + shellQuote(DataDir) + "\n" +
		"service=" + shellQuote("system/"+Label) + "\n" +
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
		"/usr/bin/install -d -o root -g wheel -m 0755 /Library/PrivilegedHelperTools /Library/LaunchDaemons\n" +
		"/bin/mv -f \"$stage/new-helper\" \"$binary\"\n/bin/mv -f \"$stage/new.plist\" \"$plist\"\n" +
		"/bin/launchctl bootstrap system \"$plist\"\n"
	return InstallPlan{Script: script, Changes: []string{BinaryPath, PlistPath, DataDir, SocketPath}, Undo: "UninstallHelper removes the service and helper data; a failed update restores the previous executable, plist and launchd registration."}, nil
}

func PlanUninstall() InstallPlan {
	script := "set -eu\nservice=" + shellQuote("system/"+Label) + "\n" +
		"for path in " + shellQuote(BinaryPath) + " " + shellQuote(PlistPath) + " " + shellQuote(DataDir) + "; do [ ! -L \"$path\" ] || { echo 'refusing symlink installation path' >&2; exit 1; }; done\n" +
		"if /bin/launchctl print \"$service\" >/dev/null 2>&1; then /bin/launchctl bootout \"$service\"; fi\n" +
		waitForSocket() +
		"/bin/rm -f " + shellQuote(BinaryPath) + " " + shellQuote(PlistPath) + "\n" +
		"if [ -S " + shellQuote(SocketPath) + " ]; then /bin/rm -f " + shellQuote(SocketPath) + "; fi\n" +
		"/bin/rm -rf " + shellQuote(DataDir) + "\n"
	return InstallPlan{Script: script, Changes: []string{BinaryPath, PlistPath, DataDir, SocketPath}, Undo: "InstallHelper reinstalls the bundled helper; removed connection logs and traffic history are not restored."}
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
	program := "do shell script " + strconv.Quote(p.Script) + " with administrator privileges"
	output, err := exec.CommandContext(ctx, "/usr/bin/osascript", "-e", program).CombinedOutput()
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
