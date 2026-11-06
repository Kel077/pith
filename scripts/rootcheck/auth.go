package main

import (
	"bytes"
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/Kel077/pith/internal/helper"
)

// Authorization is explicit on every privileged invocation; a saved skip never
// enables access to a keychain item on a later command.
type authorization struct {
	service string
	run     authCommand
}
type authCommand func(context.Context, string, []string, io.Reader, io.Writer) error

func parseAuthorization(value string) (authorization, error) {
	if value == "" || value == "dialog" {
		return authorization{}, nil
	}
	service, ok := strings.CutPrefix(value, "keychain:")
	if !ok || len(service) == 0 || len(service) > 128 || strings.IndexFunc(service, func(r rune) bool {
		return !(r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '.' || r == '_' || r == '-')
	}) >= 0 {
		return authorization{}, errors.New("auth must be dialog or keychain:SERVICE (letters, digits, '.', '_' or '-')")
	}
	return authorization{service: service}, nil
}
func (a authorization) mode() string {
	if a.service == "" {
		return "dialog"
	}
	return "keychain:" + a.service
}

func runAuthCommand(ctx context.Context, path string, args []string, input io.Reader, output io.Writer) error {
	cmd := exec.CommandContext(ctx, path, args...)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = input, output, io.Discard
	// security writes the -g password representation to stderr. Keep it in the
	// same bounded, private buffer as -w; never forward it to a terminal.
	if path == "/usr/bin/security" && len(args) > 0 && args[len(args)-1] == "-g" {
		cmd.Stdout, cmd.Stderr = io.Discard, output
	}
	return cmd.Run()
}

type secretBuffer struct{ data []byte }

func (b *secretBuffer) Write(p []byte) (int, error) {
	if len(b.data)+len(p) > 4096 {
		return 0, errors.New("keychain value is too long")
	}
	b.data = append(b.data, p...)
	return len(p), nil
}

// The only secret-bearing transport is stdin. Never wrap subprocess errors or
// relay output: even a failed child may have echoed authentication input.
func (a authorization) execute(ctx context.Context, plan helper.InstallPlan) error {
	if a.service == "" {
		return plan.Execute(ctx)
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	run := a.run
	if run == nil {
		run = runAuthCommand
	}
	b := secretBuffer{data: make([]byte, 0, 4096)}
	defer func() { clear(b.data[:cap(b.data)]) }()
	if err := run(ctx, "/usr/bin/security", []string{"find-generic-password", "-s", a.service, "-w"}, nil, &b); err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return errors.New("managed authorization: keychain read failed")
	}
	secret := bytes.TrimSuffix(b.data, []byte{'\n'})
	decoded := make([]byte, hex.DecodedLen(len(secret)))
	defer clear(decoded)
	if n, err := hex.Decode(decoded, secret); err == nil && len(secret) > 0 {
		// An ASCII password can itself consist entirely of hex digits. Ask for
		// the representation, and require both reads to describe the same value.
		metadata := secretBuffer{data: make([]byte, 0, 4096)}
		defer func() { clear(metadata.data[:cap(metadata.data)]) }()
		if err := run(ctx, "/usr/bin/security", []string{"find-generic-password", "-s", a.service, "-g"}, nil, &metadata); err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			return errors.New("managed authorization: keychain representation read failed")
		}
		line := bytes.TrimSuffix(metadata.data, []byte{'\n'})
		if value, ok := bytes.CutPrefix(line, []byte("password: 0x")); ok {
			printed, _, _ := bytes.Cut(value, []byte{' '})
			if !bytes.EqualFold(printed, secret) {
				return errors.New("managed authorization: keychain value changed between reads")
			}
			secret = decoded[:n]
		} else if value, ok := bytes.CutPrefix(line, []byte("password: \"")); !ok || len(value) != len(secret)+1 || value[len(value)-1] != '"' || !bytes.Equal(value[:len(value)-1], secret) {
			return errors.New("managed authorization: ambiguous keychain representation")
		}
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if len(secret) == 0 || bytes.ContainsAny(secret, "\x00\r\n") {
		return errors.New("managed authorization requires a nonempty single-line password")
	}
	input := make([]byte, len(secret)+1)
	copy(input, secret)
	input[len(secret)] = '\n'
	defer clear(input)
	args := []string{"-S", "-k", "-p", "", "--", "/bin/sh", "-c", plan.Script}
	if err := run(ctx, "/usr/bin/sudo", args, bytes.NewReader(input), io.Discard); err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return errors.New("managed authorization: privileged command failed; inspect independent session checks")
	}
	return nil
}

type authorizationRecord struct {
	At           time.Time `json:"at"`
	Operation    string    `json:"operation"`
	Mode         string    `json:"mode"`
	ScriptSHA256 string    `json:"scriptSHA256,omitempty"`
	Result       string    `json:"result"`
}

func (s *session) cancellationRecorded() bool { return s.CancelPassed || s.CancelSkipped }

func (s *session) skipCancellation(a authorization, ops rootcheckOps) error {
	if a.service == "" {
		return errors.New("only explicit managed authorization may skip the cancellation dialog")
	}
	if s.CancelPassed || s.CancelSkipped {
		return nil
	}
	_, err := ops.archive(s, "authorization-cancel-skipped", authorizationRecord{At: time.Now(), Operation: "cancel", Mode: a.mode(), Result: "skipped: managed authorization has no manual cancel dialog"})
	if err != nil {
		return err
	}
	s.CancelSkipped = true
	if err := ops.save(s); err != nil {
		s.CancelSkipped = false
		return err
	}
	fmt.Println("SKIPPED: manual cancellation dialog (explicit managed authorization); cancellation is not recorded as passed.")
	return nil
}

func (s *session) authorize(ctx context.Context, operation string, plan helper.InstallPlan, a authorization, ops rootcheckOps) error {
	want := s.InstallScriptSHA256
	if operation == "uninstall" {
		want = s.UninstallScriptSHA256
	}
	if operation == "crash" {
		want = s.CrashScriptSHA256
	}
	if operation != "install" && operation != "uninstall" && operation != "crash" {
		return errors.New("unknown authorization operation")
	}
	// Legacy dialog removal stays available; frozen-identity sessions and managed
	// execution require the concrete reviewed script on every invocation.
	if a.service != "" || operation == "crash" || operation == "uninstall" && s.HelperIdentity != nil {
		saved, err := fileHash(filepath.Join(s.Dir, operation+".sh"))
		if err != nil || want == "" || saved != want || hashBytes([]byte(plan.Script)) != want {
			return errors.New("reviewed authorization script is absent or changed; prepare and review a new session")
		}
	}
	record := authorizationRecord{At: time.Now(), Operation: operation, Mode: a.mode(), ScriptSHA256: hashBytes([]byte(plan.Script)), Result: "attempted"}
	if _, err := ops.archive(s, "authorization-"+operation+"-attempt", record); err != nil {
		return err
	}
	err := a.execute(ctx, plan)
	record.At = time.Now()
	record.Result = "command completed; independent verification required"
	if err != nil {
		record.Result = "command failed; independent verification required"
	}
	_, archiveErr := ops.archive(s, "authorization-"+operation+"-result", record)
	return errors.Join(err, archiveErr)
}

func crashPlan() helper.InstallPlan {
	return helper.InstallPlan{Script: "/bin/launchctl kill SIGKILL system/" + helper.Label, Prompt: "Pith needs your password to test recovery from a helper crash."}
}
