// Package keychain keeps secrets in the login keychain through
// /usr/bin/security, without cgo.
package keychain

import (
	"bytes"
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"strings"
)

// ErrNotFound: the keychain has no such item.
var ErrNotFound = errors.New("keychain: no such item")

// notFound is how security exits when it finds no item.
const notFound = 44

// run runs security and returns what it printed and its exit code. Tests
// replace it.
var run = func(ctx context.Context, input []byte, args ...string) ([]byte, int, error) {
	var out bytes.Buffer
	cmd := exec.CommandContext(ctx, "/usr/bin/security", args...)
	cmd.Stdout, cmd.Stderr = &out, io.Discard
	if input != nil {
		cmd.Stdin = bytes.NewReader(input)
		cmd.Stdout = io.Discard
	}
	err := cmd.Run()
	var exit *exec.ExitError
	if errors.As(err, &exit) {
		return out.Bytes(), exit.ExitCode(), nil
	}
	return out.Bytes(), 0, err
}

// Get returns the secret kept for service and account, or ErrNotFound.
func Get(ctx context.Context, service, account string) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	out, code, err := run(ctx, nil, "find-generic-password", "-s", service, "-a", account, "-w")
	defer clear(out)
	if ctx.Err() != nil {
		return "", ctx.Err()
	}
	switch {
	case err != nil:
		return "", errors.New("keychain: security could not read the item")
	case code == notFound:
		return "", ErrNotFound
	case code != 0:
		return "", fmt.Errorf("keychain: security exited with %d", code)
	}
	secret := strings.TrimSuffix(string(out), "\n")
	// security prints a secret that isn't printable ASCII in hexadecimal; only
	// its -g output tells that from a secret of hexadecimal digits.
	if isHex(secret) && printedAsHex(ctx, service, account) {
		raw, err := hex.DecodeString(secret)
		if err != nil {
			return "", errors.New("keychain: security printed an unreadable item")
		}
		return string(raw), nil
	}
	return secret, nil
}

// printedAsHex reports whether security prints the item's secret in
// hexadecimal. Tests replace it.
var printedAsHex = func(ctx context.Context, service, account string) bool {
	var errOut bytes.Buffer
	cmd := exec.CommandContext(ctx, "/usr/bin/security", "find-generic-password", "-s", service, "-a", account, "-g")
	cmd.Stdout, cmd.Stderr = io.Discard, &errOut
	_ = cmd.Run()
	defer clear(errOut.Bytes())
	return bytes.HasPrefix(errOut.Bytes(), []byte("password: 0x"))
}

func isHex(s string) bool {
	if s == "" || len(s)%2 != 0 {
		return false
	}
	for i := 0; i < len(s); i++ {
		if !strings.ContainsRune("0123456789abcdefABCDEF", rune(s[i])) {
			return false
		}
	}
	return true
}

// Set keeps secret for service and account, replacing what was kept. Only
// security itself may read it back without macOS asking the user.
func Set(ctx context.Context, service, account, secret string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	input, err := setInput(service, account, secret)
	if err != nil {
		return err
	}
	defer clear(input)
	// Changing an item in place (-U, with its access list) has macOS ask the
	// user to authorize it in a dialog. Removing it, which needs no
	// authorization, and adding it anew doesn't.
	_, code, err := run(ctx, nil, "delete-generic-password", "-s", service, "-a", account)
	switch {
	case ctx.Err() != nil:
		return ctx.Err()
	case err != nil:
		return errors.New("keychain: security could not replace the item")
	case code != 0 && code != notFound:
		return fmt.Errorf("keychain: security exited with %d", code)
	}
	// security's documented interactive mode accepts one command on stdin, then
	// EOF. Do not add a trailing command: it would overwrite the operation's exit
	// status. Never enable verbose mode, which prints the parsed arguments.
	_, code, err = run(ctx, input, "-i")
	if ctx.Err() != nil {
		return ctx.Err()
	}
	switch {
	case err != nil:
		return errors.New("keychain: security could not store the item")
	case code != 0:
		return fmt.Errorf("keychain: security exited with %d", code)
	}
	return nil
}

// This is security's command-input quoting, not shell syntax. Its documented
// -i interface and Apple's parser accept quoted arguments with backslash escapes:
// https://github.com/apple-oss-distributions/Security/blob/main/SecurityTool/macOS/security.c
func setInput(service, account, secret string) ([]byte, error) {
	if service == "" || account == "" {
		return nil, errors.New("keychain: service and account are required")
	}
	args := []string{"add-generic-password", "-s", service, "-a", account, "-T", "/usr/bin/security", "-w", secret}
	input := make([]byte, 0, 4096)
	for i, arg := range args {
		if strings.ContainsAny(arg, "\x00\r\n") {
			clear(input)
			return nil, errors.New("keychain: item values must fit one command line")
		}
		if i > 0 {
			input = append(input, ' ')
		}
		input = append(input, '"')
		for j := 0; j < len(arg); j++ {
			if len(input) >= 4000 {
				clear(input)
				return nil, errors.New("keychain: item values exceed the command input limit")
			}
			if arg[j] == '"' || arg[j] == '\\' {
				input = append(input, '\\')
			}
			input = append(input, arg[j])
		}
		input = append(input, '"')
	}
	input = append(input, '\n')
	return input, nil
}
