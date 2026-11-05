// rootcheck prepares and runs an explicitly authorized, host-scoped acceptance
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

const usage = `rootcheck prepares a private, scoped acceptance session.

  prepare   --helper ABSOLUTE_PATH --interface-a NAME --interface-b NAME
            --target IP:PORT --server-name TLS_NAME [--version dev]
            [--dns-target IP:53]
  cancel    --session DIRECTORY
  install   --session DIRECTORY
  check     --session DIRECTORY [--batch | --safe-failure | --preserve | --observe 90s]
  stop      --session DIRECTORY
  uninstall --session DIRECTORY

prepare writes review material only to a fresh directory in TMPDIR. It reads
system state and makes bounded, unprivileged TLS probes; it never opens an
authorization dialog. All other commands need that session directory.

cancel expects the user to cancel the install dialog. install and uninstall
request administrator authorization. check starts routing only to the reviewed
IPv4 test host(s), always stops it on return, and never installs the helper.
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
	if args[0] == "prepare" {
		return prepare(ctx, args[1:])
	}
	fs := flag.NewFlagSet(args[0], flag.ContinueOnError)
	dir := fs.String("session", "", "private directory printed by prepare")
	var opts checkOptions
	if args[0] == "check" {
		fs.BoolVar(&opts.batch, "batch", false, "run the remaining automatic cases after the smoke check")
		fs.BoolVar(&opts.safeFailure, "safe-failure", false, "verify that a blocked SafeStart rolls back capture")
		fs.BoolVar(&opts.preserve, "preserve", false, "verify a native TCP connection opened before capture survives activation")
		fs.DurationVar(&opts.observe, "observe", 0, "manual observation window, at most 90s; always stops afterward")
	}
	if err := fs.Parse(args[1:]); err != nil {
		return err
	}
	if fs.NArg() != 0 || *dir == "" {
		return errors.New("supply --session DIRECTORY and no positional arguments")
	}
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
		return cancelInstall(ctx, s)
	case "install":
		return install(ctx, s, c)
	case "check":
		return check(ctx, s, c, opts)
	case "stop":
		return stop(ctx, s, c)
	case "uninstall":
		return uninstall(ctx, s, c)
	default:
		return fmt.Errorf("unknown command %q; use help", args[0])
	}
}

func cancelInstall(ctx context.Context, s *session) error {
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

func install(ctx context.Context, s *session, c api.Service) error {
	if !s.CancelPassed || s.Installed {
		return errors.New("install requires a passed cancellation case and no installation in this session")
	}
	plan, err := s.reviewedPlan(ctx)
	if err != nil {
		return err
	}
	if err := s.expectOriginal(ctx, "before-install"); err != nil {
		return err
	}
	s.AuthorizationAttempted = true
	if err := s.save(); err != nil {
		return err
	}
	fmt.Println("Authorize the reviewed host-scoped helper installation. Routing will remain stopped.")
	authCtx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	if err := plan.Execute(authCtx); err != nil {
		return fmt.Errorf("installation did not complete: %w; inspect the session and use uninstall if managed paths remain", err)
	}
	s.Installed = true
	if err := s.save(); err != nil {
		return err
	}
	if err := s.verifyInstalled(); err != nil {
		return err
	}
	readyCtx, readyCancel := context.WithTimeout(ctx, 10*time.Second)
	defer readyCancel()
	if err := waitUntil(readyCtx, func() (bool, error) {
		st, err := c.Status(readyCtx)
		if err != nil || !st.Helper.Running {
			return false, nil
		}
		if st.Helper.Outdated || st.State != api.Stopped {
			return false, errors.New("installed helper has the wrong version or is not idle")
		}
		return true, nil
	}); err != nil {
		return fmt.Errorf("helper readiness: %w; run uninstall", err)
	}
	after, err := takeSnapshot(ctx)
	if err != nil {
		return err
	}
	if err := writeJSON(filepath.Join(s.Dir, "after-install.json"), after); err != nil {
		return err
	}
	if !sameRoutes(s.Original.Routes, after.Routes) || !sameTunnels(s.Original.Interfaces, after.Interfaces) {
		return errors.New("installation changed routes or tunnel inventory while idle; run uninstall and review the snapshots")
	}
	fmt.Println("PASS: the reviewed helper is installed, authenticated, version-matched, and idle; no capture was added.")
	return nil
}

func stop(ctx context.Context, s *session, c api.Service) error {
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	if err := c.Stop(ctx); err != nil {
		return fmt.Errorf("Stop failed: %w; use the reviewed emergency command", err)
	}
	st, err := c.Status(ctx)
	if err != nil || st.State != api.Stopped {
		return fmt.Errorf("helper did not report stopped: status=%s error=%v", st.State, err)
	}
	after, err := takeSnapshot(ctx)
	if err != nil {
		return err
	}
	if err := writeJSON(filepath.Join(s.Dir, "after-stop.json"), after); err != nil {
		return err
	}
	if !sameRoutes(s.Original.Routes, after.Routes) || !sameTunnels(s.Original.Interfaces, after.Interfaces) {
		return errors.New("Stop returned but routing or tunnel inventory differs; inspect snapshots before proceeding")
	}
	fmt.Println("PASS: routing is stopped and the original routes and tunnel inventory are restored.")
	return nil
}

func uninstall(ctx context.Context, s *session, c api.Service) error {
	if !s.AuthorizationAttempted {
		return errors.New("this session has never attempted installation; there is nothing it is authorized to remove")
	}
	// Uninstall remains usable when IPC, its version check, or Stop has failed.
	// PlanUninstall boots out launchd before ownership-checked journal cleanup.
	stopCtx, stopCancel := context.WithTimeout(ctx, 10*time.Second)
	_ = c.Stop(stopCtx)
	stopCancel()
	authCtx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	fmt.Println("Authorize removal of the reviewed helper, its service, socket, and test data.")
	if err := helper.PlanUninstall().Execute(authCtx); err != nil {
		return fmt.Errorf("uninstall: %w; retain the session and use the reviewed emergency command", err)
	}
	if err := s.expectOriginal(ctx, "after-uninstall"); err != nil {
		return err
	}
	s.Installed = false
	if err := s.save(); err != nil {
		return err
	}
	fmt.Println("PASS: helper, launchd registration, socket, capture journal, and helper data are absent; original routes and tunnels are restored.")
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
