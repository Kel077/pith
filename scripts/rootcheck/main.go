// rootcheck prepares and runs an explicitly authorized, bounded acceptance
// session. Run it as the installing user, never as root. No command is implicit.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"runtime"
	"syscall"
	"time"

	"github.com/Kel077/pith/internal/api"
	"github.com/Kel077/pith/internal/client"
	"github.com/Kel077/pith/internal/helper"
)

const usage = `rootcheck prepares a private, bounded acceptance session.

  prepare   --helper ABSOLUTE_PATH --interface-a NAME --interface-b NAME
            --target IP:PORT --server-name TLS_NAME [--version dev]
            [--dns-target IP:53 [--dns-target IP:53] --dns-name DNS_NAME]
            [--quic-target IP:443 --quic-server-name TLS_NAME]
            [--app-bundle] [--webkit-app ABSOLUTE_SAFARI_APP]
            [--capture-mode full-ipv4 --test-lease 120s]
            [--capture-mode host-ipv6 --test-lease 60s]
  cancel    --session DIRECTORY [--auth keychain:SERVICE]
  install   --session DIRECTORY [--auth keychain:SERVICE]
  check     --session DIRECTORY [--batch | --safe-failure | --preserve | --app-bypass | --observe 90s]
  check     --session DIRECTORY --crash [--auth keychain:SERVICE]
  check     --session DIRECTORY --observe 90s --renew-hotspot
  check     --session DIRECTORY [--udp-dns | --udp-dns-batch]
  check     --session DIRECTORY [--quic | --quic-batch]
  check     --session DIRECTORY [--app-bundle | --app-bundle-batch]
  check     --session DIRECTORY --webkit 45s [--webkit-open]
  stop      --session DIRECTORY
  rebaseline --session DIRECTORY --label MODE [--accept-external-change]
  uninstall --session DIRECTORY [--auth keychain:SERVICE]

prepare writes review material only to a fresh directory in TMPDIR. It reads
system state and makes bounded, unprivileged TLS probes; it never opens an
authorization dialog. All other commands need that session directory.

cancel expects the user to cancel the install dialog. install and uninstall
request administrator authorization. check uses the reviewed installation mode
(host-ipv4 by default), always stops on return, and never installs the helper.
full-ipv4 is a distinct installation: eight split routes plus frozen LAN DNS hosts,
IPv6 disabled, mandatory SafeStart, and a helper-enforced lease at most 120s.
host-ipv6 is another distinct installation: one public IPv6 /128, mandatory
IPv6 SafeStart and tcp6 requests, with a helper-enforced lease at most 60s.
Explicit --auth keychain:SERVICE uses a delegated password via stdin for that
invocation only. It skips the manual cancel case and reports it as skipped.
check --crash requires prior smoke, kills only the reviewed running helper, and
verifies owned capture removal and idle restart. It needs separate authorization.
rebaseline records an approved mode change only while stopped, after verified
cleanup, without an authorization dialog. It requires a new smoke check.
Read docs/ROOT-TEST.md before use. Do not run from an unattended agent.
`

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, "rootcheck:", err)
		os.Exit(1)
	}
}

func run(args []string) error {
	if len(args) == 0 || args[0] == "help" || args[0] == "--help" || args[0] == "-h" {
		fmt.Print(usage)
		return nil
	}
	if runtime.GOOS != "darwin" || runtime.GOARCH != "arm64" {
		return errors.New("acceptance commands require macOS on Apple Silicon")
	}
	if os.Getuid() == 0 || os.Geteuid() == 0 {
		return errors.New("run as the installing user; administrator dialogs perform the privileged steps")
	}
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	if args[0] == "_request" {
		return requestCommand(ctx, args[1:])
	}
	if args[0] == "prepare" {
		return prepare(ctx, args[1:])
	}
	fs := flag.NewFlagSet(args[0], flag.ContinueOnError)
	dir := fs.String("session", "", "private directory printed by prepare")
	var label string
	var acceptExternal bool
	if args[0] == "rebaseline" {
		fs.StringVar(&label, "label", "", "generic phase label, such as vpn-off, tailnet, or exit-node")
		fs.BoolVar(&acceptExternal, "accept-external-change", false, "accept reviewed external-only differences after proven Pith cleanup")
	}
	var opts checkOptions
	authFlag := fs.String("auth", "dialog", "explicit authorization for this invocation: dialog or keychain:SERVICE")
	if args[0] == "check" {
		fs.BoolVar(&opts.batch, "batch", false, "run the remaining automatic cases after the smoke check")
		fs.BoolVar(&opts.safeFailure, "safe-failure", false, "verify that a blocked SafeStart rolls back capture")
		fs.BoolVar(&opts.preserve, "preserve", false, "verify a native TCP connection opened before capture survives activation")
		fs.BoolVar(&opts.appBypass, "app-bypass", false, "verify app-specific IP/domain routing and an unmatched process using the same frozen target")
		fs.BoolVar(&opts.crash, "crash", false, "deliberately crash the reviewed active helper once, then verify cleanup and idle restart")
		fs.DurationVar(&opts.observe, "observe", 0, "manual observation window, at most 90s; always stops afterward")
		fs.BoolVar(&opts.renewHotspot, "renew-hotspot", false, "explicitly renew hotspot A during --observe, then verify a new routed connection")
		fs.BoolVar(&opts.udpDNS, "udp-dns", false, "one UDP DNS smoke request through A to the first frozen resolver")
		fs.BoolVar(&opts.udpDNSBatch, "udp-dns-batch", false, "bounded DNS cases after a passed UDP DNS smoke check")
		fs.BoolVar(&opts.quic, "quic", false, "one verified HTTP/3 request through A to the frozen QUIC target")
		fs.BoolVar(&opts.quicBatch, "quic-batch", false, "three HTTP/3 cases after a passed QUIC smoke check")
		fs.BoolVar(&opts.appBundle, "app-bundle", false, "one request from the frozen disposable bundle's main executable")
		fs.BoolVar(&opts.appBundleBatch, "app-bundle-batch", false, "nested helper and outside-bundle controls after bundle smoke")
		fs.DurationVar(&opts.webkit, "webkit", 0, "observe one manually triggered Safari navigation for at most 60s")
		fs.BoolVar(&opts.webkitOpen, "webkit-open", false, "after READY, open the frozen Safari URL once using LaunchServices; requires --webkit")
	}
	if err := fs.Parse(args[1:]); err != nil {
		return err
	}
	if fs.NArg() != 0 || *dir == "" {
		return errors.New("supply --session DIRECTORY and no positional arguments")
	}
	auth, err := parseAuthorization(*authFlag)
	if err != nil {
		return err
	}
	if auth.service != "" && args[0] != "cancel" && args[0] != "install" && args[0] != "uninstall" && !(args[0] == "check" && opts.crash) {
		return errors.New("managed auth applies only to cancel, install, uninstall, or check --crash")
	}
	opts.auth = auth
	s, err := loadSession(*dir)
	if err != nil {
		return err
	}
	lock, err := s.lock()
	if err != nil {
		return err
	}
	defer lock.Close()
	// Re-read mutable phase state after acquiring the advisory lock.
	s, err = loadSession(*dir)
	if err != nil {
		return err
	}
	// Deliberately never use the application's config directory or installer:
	// the normal installer does not constrain capture to the reviewed hosts.
	c, err := client.New(client.Options{ConfigDir: filepath.Join(s.Dir, "config"), Version: s.Version})
	if err != nil {
		return err
	}
	switch args[0] {
	case "cancel":
		return cancelInstall(ctx, s, auth)
	case "install":
		return install(ctx, s, c, auth)
	case "check":
		return check(ctx, s, c, opts)
	case "stop":
		return stop(ctx, s, c)
	case "rebaseline":
		return rebaseline(ctx, s, c, label, acceptExternal)
	case "uninstall":
		return uninstall(ctx, s, c, auth)
	default:
		return fmt.Errorf("unknown command %q; use help", args[0])
	}
}

func cancelInstall(ctx context.Context, s *session, auth authorization) error {
	if s.CancelPassed {
		return errors.New("the cancellation case already passed; continue with install")
	}
	plan, err := s.reviewedPlan(ctx)
	if err != nil {
		return err
	}
	if err := s.expectOriginal(ctx, "before-cancel"); err != nil {
		return err
	}
	if auth.service != "" {
		return s.skipCancellation(auth, liveOps())
	}
	// Record intent first: entering a password by mistake still leaves a session
	// that can run uninstall, without claiming cancellation passed.
	s.AuthorizationAttempted = true
	if err := s.save(); err != nil {
		return err
	}
	fmt.Println("Cancel the administrator authorization dialog. Do not enter a password.")
	authCtx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	err = plan.Execute(authCtx)
	if api.Code(err) != api.ErrCanceled {
		return fmt.Errorf("cancellation did not return ErrCanceled (result: %v); if installation occurred, run uninstall or the reviewed emergency command", err)
	}
	if err := s.expectOriginal(ctx, "after-cancel"); err != nil {
		return err
	}
	s.CancelPassed = true
	if err := s.save(); err != nil {
		return err
	}
	fmt.Println("PASS: cancellation returned ErrCanceled; managed paths and the route snapshot are unchanged.")
	return nil
}

func install(ctx context.Context, s *session, c api.Service, auth authorization) error {
	if (!s.CancelPassed && auth.service == "") || s.Installed {
		return errors.New("install requires a passed cancellation case and no installation in this session")
	}
	plan, err := s.reviewedPlan(ctx)
	if err != nil {
		return err
	}
	if err := s.expectOriginal(ctx, "before-install"); err != nil {
		return err
	}
	if auth.service != "" {
		if err := s.skipCancellation(auth, liveOps()); err != nil {
			return err
		}
	}
	s.AuthorizationAttempted = true
	if err := s.save(); err != nil {
		return err
	}
	fmt.Println("Authorize the reviewed helper installation (" + s.captureMode() + "). Routing will remain stopped.")
	authCtx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	if err := s.authorize(authCtx, "install", plan, auth, liveOps()); err != nil {
		return fmt.Errorf("installation did not complete: %w; inspect the session and use uninstall if managed paths remain", err)
	}
	s.Installed = true
	if err := s.save(); err != nil {
		return err
	}
	if err := s.verifyInstalled(); err != nil {
		return err
	}
	identity, err := readInstallationID(helper.PlistPath)
	if err != nil {
		return err
	}
	s.Installation = &identity
	if err := s.save(); err != nil {
		return err
	}
	readyCtx, readyCancel := context.WithTimeout(ctx, 10*time.Second)
	defer readyCancel()
	if err := waitUntil(readyCtx, func() (bool, error) {
		st, err := c.Status(readyCtx)
		if err != nil || !st.Helper.Running {
			return false, nil
		}
		if err := idleReviewedStatus(s, st); err != nil {
			return false, err
		}
		return true, nil
	}); err != nil {
		return fmt.Errorf("helper readiness: %w; run uninstall", err)
	}
	after, err := takeSnapshot(ctx)
	if err != nil {
		return err
	}
	if _, err := archiveJSON(s, "after-install", after); err != nil {
		return err
	}
	if !sameCleanupBaseline(s.Original, after) {
		return errors.New("installation changed routes or tunnel inventory while idle; run uninstall and review the snapshots")
	}
	fmt.Println("PASS: the reviewed helper is installed, authenticated, version-matched, and idle; no capture was added.")
	return nil
}

func stop(ctx context.Context, s *session, c api.Service) error {
	return stopWithOps(ctx, s, c, liveOps())
}

func stopWithOps(ctx context.Context, s *session, c api.Service, ops rootcheckOps) error {
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	if err := s.invalidateCleanup(ops); err != nil {
		return fmt.Errorf("persist invalidated cleanup evidence before Stop: %w", err)
	}
	if err := ops.verifyInstalled(s); err != nil {
		return err
	}
	record, resultErr := collectCleanup(ctx, s, c, ops, "stop")
	if err := s.recordCleanup(record, resultErr, ops); err != nil {
		resultErr = errors.Join(resultErr, err)
	}
	if resultErr != nil {
		return resultErr
	}
	fmt.Println("PASS: routing is stopped and the current baseline's routes and tunnel inventory are restored.")
	return nil
}

func uninstall(ctx context.Context, s *session, c api.Service, auth authorization) error {
	if !s.AuthorizationAttempted {
		return errors.New("this session has never attempted installation; there is nothing it is authorized to remove")
	}
	// A known replacement installation is not this session's cleanup target.
	// Missing files after a partial uninstall remain recoverable, without IPC.
	if err := s.verifyRemovalOwnership(helper.BinaryPath, helper.PlistPath); err != nil {
		return fmt.Errorf("refuse to remove another installation: %w; review the standalone emergency command", err)
	}
	if err := s.invalidateCleanup(liveOps()); err != nil {
		return err
	}
	// Uninstall remains usable when IPC, its version check, or Stop has failed.
	// PlanUninstall boots out launchd before ownership-checked journal cleanup.
	if s.verifyOwnedInstallation() == nil {
		stopCtx, stopCancel := context.WithTimeout(ctx, 10*time.Second)
		_ = c.Stop(stopCtx)
		stopCancel()
	}
	authCtx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	fmt.Println("Authorize removal of the reviewed helper, its service, socket, and test data.")
	if err := s.authorize(authCtx, "uninstall", helper.PlanUninstall(), auth, liveOps()); err != nil {
		return fmt.Errorf("uninstall: %w; retain the session and use the reviewed emergency command", err)
	}
	if err := s.expectOriginal(ctx, "after-uninstall"); err != nil {
		return err
	}
	s.Installed = false
	s.SmokePassed = false
	s.DNSPassed = false
	s.QUICPassed = false
	s.BundlePassed = false
	if err := s.save(); err != nil {
		return err
	}
	fmt.Println("PASS: helper, launchd registration, socket, capture journal, and helper data are absent; current baseline routes and tunnels are restored.")
	fmt.Println("Private review artifacts remain in the session directory for inspection; remove that directory when finished.")
	return nil
}

func waitUntil(ctx context.Context, fn func() (bool, error)) error {
	for {
		ok, err := fn()
		if err != nil || ok {
			return err
		}
		timer := time.NewTimer(100 * time.Millisecond)
		select {
		case <-ctx.Done():
			timer.Stop()
			return ctx.Err()
		case <-timer.C:
		}
	}
}
