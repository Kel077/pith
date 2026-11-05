// Package keychain keeps secrets in the login keychain through
// /usr/bin/security, without cgo.
package keychain

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os/exec"
	"strings"
)

// ErrNotFound: the keychain has no such item.
var ErrNotFound = errors.New("keychain: no such item")

// notFound is how security exits when it finds no item.
const notFound = 44

// run runs security and returns what it printed and its exit code. Tests
// replace it.
var run = func(ctx context.Context, args ...string) ([]byte, int, error) {
	var out, errOut bytes.Buffer
	cmd := exec.CommandContext(ctx, "/usr/bin/security", args...)
	cmd.Stdout, cmd.Stderr = &out, &errOut
	err := cmd.Run()
	var exit *exec.ExitError
	if errors.As(err, &exit) {
		return out.Bytes(), exit.ExitCode(), nil
	}
	return out.Bytes(), 0, err
}

// Get returns the secret kept for service and account, or ErrNotFound.
func Get(ctx context.Context, service, account string) (string, error) {
	out, code, err := run(ctx, "find-generic-password", "-s", service, "-a", account, "-w")
	switch {
	case err != nil:
		return "", fmt.Errorf("keychain: %w", err)
	case code == notFound:
		return "", ErrNotFound
	case code != 0:
		return "", fmt.Errorf("keychain: security exited with %d", code)
	}
	return strings.TrimSuffix(string(out), "\n"), nil
}

// Set keeps secret for service and account, replacing what was kept. Only
// security itself may read it back without macOS asking the user.
func Set(ctx context.Context, service, account, secret string) error {
	_, code, err := run(ctx, "add-generic-password", "-s", service, "-a", account, "-T", "/usr/bin/security", "-U", "-w", secret)
	switch {
	case err != nil:
		return fmt.Errorf("keychain: %w", err)
	case code != 0:
		return fmt.Errorf("keychain: security exited with %d", code)
	}
	return nil
}
