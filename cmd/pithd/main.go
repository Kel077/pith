// pithd is the privileged routing helper. Launchd starts it idle; only an
// authenticated local request can start a routing scene.
package main

import (
	"context"
	"flag"
	"fmt"
	"net/netip"
	"os"
	"os/signal"
	"path/filepath"
	"runtime"
	"syscall"
	"time"

	"github.com/Kel077/pith/internal/engine"
	"github.com/Kel077/pith/internal/engine/capture"
	"github.com/Kel077/pith/internal/helper"
	"github.com/Kel077/pith/internal/testprofile"
)

var version = helper.Version

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "pithd:", err)
		os.Exit(1)
	}
}

func run() error {
	uid := flag.Uint("owner-uid", 0, "UID allowed to control this helper")
	showVersion := flag.Bool("version", false, "print the helper version")
	cleanup := flag.Bool("cleanup", false, "remove only stale journaled capture routes after launchd bootout, then exit")
	dataDir := flag.String("data-dir", helper.DataDir, "private helper data directory")
	socket := flag.String("socket", helper.SocketPath, "authenticated Unix socket path")
	probe := flag.String("safe-target", "", "SafeStart TLS host:port, resolved before capture")
	serverName := flag.String("safe-server-name", "", "TLS server name when safe-target is an IP address")
	testMode := flag.String("test-profile", "", "explicit acceptance installation: full-ipv4 or host-ipv6")
	testLease := flag.Duration("test-lease", 0, "test-profile lease: at most 120s for full-ipv4, 60s for host-ipv6; never extended by reload")
	var testDNS []netip.Addr
	flag.Func("test-dns-host", "reviewed test-profile LAN DNS capture host (repeatable)", func(raw string) error {
		addr, err := netip.ParseAddr(raw)
		if err == nil {
			testDNS = append(testDNS, addr)
		}
		return err
	})
	var scopes []netip.Prefix
	flag.Func("capture", "development: capture only this CIDR for at most five minutes (repeatable); empty uses full split capture", func(raw string) error {
		prefix, err := netip.ParsePrefix(raw)
		if err != nil || prefix.Bits() == 0 {
			return fmt.Errorf("invalid scoped capture CIDR %q", raw)
		}
		scopes = append(scopes, prefix.Masked())
		return nil
	})
	flag.Parse()
	var profile *testprofile.Profile
	testSpecified := false
	flag.Visit(func(f *flag.Flag) {
		if f.Name == "test-profile" || f.Name == "test-lease" || f.Name == "test-dns-host" {
			testSpecified = true
		}
	})
	if testSpecified {
		profile = &testprofile.Profile{Mode: *testMode, Lease: *testLease, Target: *probe, ServerName: *serverName, DNSHosts: testDNS}
		if err := profile.Validate(); err != nil {
			return err
		}
		if len(scopes) != 0 {
			return fmt.Errorf("--test-profile and --capture are mutually exclusive")
		}
	}
	if *showVersion {
		fmt.Println(version)
		return nil
	}
	if runtime.GOOS != "darwin" || runtime.GOARCH != "arm64" {
		return fmt.Errorf("Pith requires macOS on Apple Silicon")
	}
	if os.Geteuid() != 0 {
		return fmt.Errorf("the helper must be started by launchd as root")
	}
	if *cleanup {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		return capture.CleanupStale(ctx, filepath.Join(*dataDir, "capture.json"), nil)
	}
	if *uid == 0 || uint64(*uid) > uint64(^uint32(0)) {
		return fmt.Errorf("--owner-uid must name an unprivileged user")
	}
	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()
	backend, err := engine.New(engine.Options{DataDir: *dataDir, Scopes: scopes, ProbeAddress: *probe, ProbeServerName: *serverName, TestProfile: profile})
	if err != nil {
		return err
	}
	defer backend.Close()
	server := helper.NewServer(helper.ServerOptions{Version: version, OwnerUID: uint32(*uid), Backend: backend, SocketPath: *socket})
	return server.Run(ctx)
}
