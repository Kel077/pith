// pithd is the privileged routing helper. Launchd starts it idle; only an
// authenticated local request can start a routing scene.
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"runtime"
	"syscall"

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
	server := helper.NewServer(helper.ServerOptions{Version: version, OwnerUID: uint32(*uid), Backend: helper.IdleBackend{}})
	return server.Run(ctx)
}
