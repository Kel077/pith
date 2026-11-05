// Package keychain keeps secrets in the login keychain through
// /usr/bin/security, without cgo.
package keychain

import (
	"bytes"
	"context"
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
	return strings.TrimSuffix(string(out), "\n"), nil
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
	// security's documented interactive mode accepts one command on stdin, then
	// EOF. Do not add a trailing command: it would overwrite the operation's exit
	// status. Never enable verbose mode, which prints the parsed arguments.
	_, code, err := run(ctx, input, "-i")
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
	args := []string{"add-generic-password", "-s", service, "-a", account, "-T", "/usr/bin/security", "-U", "-w", secret}
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
