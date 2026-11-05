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
	"runtime"
	"syscall"

	"github.com/Kel077/pith/internal/engine"
	"github.com/Kel077/pith/internal/helper"
)

var version = "dev"

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "pithd:", err)
		os.Exit(1)
	}
}

func run() error {
	uid := flag.Uint("owner-uid", 0, "UID allowed to control this helper")
	showVersion := flag.Bool("version", false, "print the helper version")
	dataDir := flag.String("data-dir", helper.DataDir, "private helper data directory")
	socket := flag.String("socket", helper.SocketPath, "authenticated Unix socket path")
	probe := flag.String("safe-target", "", "SafeStart TLS host:port, resolved before capture")
	serverName := flag.String("safe-server-name", "", "TLS server name when safe-target is an IP address")
	var scopes []netip.Prefix
	flag.Func("capture", "development: capture only this CIDR (repeatable); empty captures both /1 routes", func(raw string) error {
		prefix, err := netip.ParsePrefix(raw)
		if err != nil || prefix.Bits() == 0 {
			return fmt.Errorf("invalid scoped capture CIDR %q", raw)
		}
		scopes = append(scopes, prefix.Masked())
		return nil
	})
	flag.Parse()
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
	if *uid == 0 || uint64(*uid) > uint64(^uint32(0)) {
		return fmt.Errorf("--owner-uid must name an unprivileged user")
	}
	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()
	backend, err := engine.New(engine.Options{DataDir: *dataDir, Scopes: scopes, ProbeAddress: *probe, ProbeServerName: *serverName})
	if err != nil {
		return err
	}
	defer backend.Close()
	server := helper.NewServer(helper.ServerOptions{Version: version, OwnerUID: uint32(*uid), Backend: backend, SocketPath: *socket})
	return server.Run(ctx)
}
