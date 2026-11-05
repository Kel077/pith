package main

import (
	"bytes"
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/netip"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/Kel077/pith/internal/api"
	"github.com/Kel077/pith/internal/engine/capture"
	"github.com/Kel077/pith/internal/helper"
)

func TestManagedAuthorizationKeepsSecretsOnlyOnStdin(t *testing.T) {
	for _, fail := range []bool{false, true} {
		t.Run(map[bool]string{false: "success", true: "child-error"}[fail], func(t *testing.T) {
			const secret = "synthetic password $() with spaces"
			a, err := parseAuthorization("keychain:example-admin")
			if err != nil {
				t.Fatal(err)
			}
			var retained *secretBuffer
			var input *bytes.Reader
			var calls []string
			plan := helper.InstallPlan{Script: "reviewed synthetic script"}
			a.run = func(ctx context.Context, path string, args []string, stdin io.Reader, stdout io.Writer) error {
				calls = append(calls, path)
				if strings.Contains(strings.Join(args, " "), secret) {
					t.Fatal("secret in argv")
				}
				switch path {
				case "/usr/bin/security":
					if !slices.Equal(args, []string{"find-generic-password", "-s", "example-admin", "-w"}) || stdin != nil {
						t.Fatal(args)
					}
					retained = stdout.(*secretBuffer)
					_, err := io.WriteString(stdout, secret+"\n")
					return err
				case "/usr/bin/sudo":
					if !slices.Equal(args, []string{"-S", "-k", "-p", "", "--", "/bin/sh", "-c", plan.Script}) || stdout != io.Discard {
						t.Fatal(args)
					}
					input = stdin.(*bytes.Reader)
					data, _ := io.ReadAll(input)
					if string(data) != secret+"\n" {
						t.Fatal("incorrect stdin")
					}
					if fail {
						return errors.New(secret)
					}
					return nil
				default:
					t.Fatal("unexpected executable", path)
					return nil
				}
			}
			err = a.execute(context.Background(), plan)
			if (err != nil) != fail || err != nil && strings.Contains(err.Error(), secret) {
				t.Fatal("error leaked secret or wrong result")
			}
			if len(calls) != 2 {
				t.Fatal(calls)
			}
			if bytes.Count(retained.data, []byte{0}) != len(retained.data) {
				t.Fatal("read buffer not cleared")
			}
			input.Seek(0, io.SeekStart)
			data, _ := io.ReadAll(input)
			if bytes.Count(data, []byte{0}) != len(data) {
				t.Fatal("stdin buffer not cleared")
			}
		})
	}
}

func TestManagedAuthorizationRejectsBadValuesBeforeSudo(t *testing.T) {
	for _, value := range []string{"", "\n", "first\nsecond\n", "value\x00\n", strings.Repeat("x", 4097)} {
		t.Run("value", func(t *testing.T) {
			a, _ := parseAuthorization("keychain:example-admin")
			calls := 0
			a.run = func(_ context.Context, path string, _ []string, _ io.Reader, out io.Writer) error {
				calls++
				if path != "/usr/bin/security" {
					t.Fatal("attempted sudo")
				}
				_, err := io.WriteString(out, value)
				return err
			}
			if a.execute(context.Background(), crashPlan()) == nil || calls != 1 {
				t.Fatal("invalid value accepted")
			}
		})
	}
	for _, value := range []string{"keychain:", "keychain:bad\nname", "sudo", "keychain:$(example)", "keychain:../item"} {
		if _, err := parseAuthorization(value); err == nil {
			t.Fatal("accepted mode", value)
		}
	}
	a, _ := parseAuthorization("keychain:example-admin")
	a.run = func(context.Context, string, []string, io.Reader, io.Writer) error {
		t.Fatal("canceled invocation ran")
		return nil
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if !errors.Is(a.execute(ctx, crashPlan()), context.Canceled) {
		t.Fatal("cancellation lost")
	}
}

func TestManagedAuthorizationDistinguishesEncodedAndLiteralHex(t *testing.T) {
	const unicodePassword = "\u00e9 \u7a7a\u683c \"\\$"
	encoded := hex.EncodeToString([]byte(unicodePassword))
	for _, tt := range []struct {
		name, raw, metadata, want string
		readError, cancel, fail   bool
	}{
		{name: "unicode", raw: encoded, metadata: "password: 0x" + encoded + "  \"representation\"\n", want: unicodePassword},
		{name: "literal-hex", raw: "deadbeef", metadata: "password: \"deadbeef\"\n", want: "deadbeef"},
		{name: "uppercase", raw: "C3A9", metadata: "password: 0xc3a9\n", want: "\u00e9"},
		{name: "changed", raw: encoded, metadata: "password: 0x010203\n", fail: true},
		{name: "changed-literal", raw: "deadbeef", metadata: "password: \"abcdefab\"\n", fail: true},
		{name: "ambiguous", raw: encoded, metadata: "unrecognized output\n", fail: true},
		{name: "read-error", raw: encoded, readError: true, fail: true},
		{name: "cancellation", raw: encoded, cancel: true, fail: true},
		{name: "decoded-newline", raw: "61620a63", metadata: "password: 0x61620a63\n", fail: true},
		{name: "decoded-nul", raw: "61620063", metadata: "password: 0x61620063\n", fail: true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			a, _ := parseAuthorization("keychain:example-admin")
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			var buffers []*secretBuffer
			sudo := false
			a.run = func(_ context.Context, path string, args []string, in io.Reader, out io.Writer) error {
				if path == "/usr/bin/security" {
					buffers = append(buffers, out.(*secretBuffer))
					if args[len(args)-1] == "-w" {
						_, err := io.WriteString(out, tt.raw+"\n")
						return err
					}
					if !slices.Equal(args, []string{"find-generic-password", "-s", "example-admin", "-g"}) {
						t.Fatal(args)
					}
					if tt.cancel {
						cancel()
					}
					if tt.readError {
						return errors.New(unicodePassword)
					}
					_, err := io.WriteString(out, tt.metadata)
					return err
				}
				if path != "/usr/bin/sudo" || tt.fail {
					t.Fatal("unexpected privileged execution")
				}
				sudo = true
				data, err := io.ReadAll(in)
				if err != nil || string(data) != tt.want+"\n" {
					t.Fatal("incorrect password bytes")
				}
				return nil
			}
			err := a.execute(ctx, crashPlan())
			if (err != nil) != tt.fail || sudo == tt.fail || len(buffers) != 2 {
				t.Fatal("incorrect authorization outcome")
			}
			if err != nil && (strings.Contains(err.Error(), unicodePassword) || strings.Contains(err.Error(), encoded)) {
				t.Fatal("secret leaked in error")
			}
			for _, b := range buffers {
				if bytes.Count(b.data[:cap(b.data)], []byte{0}) != cap(b.data) {
					t.Fatal("private buffer not cleared")
				}
			}
		})
	}
}

func TestManagedSkipIsPersistedWithoutClaimingPass(t *testing.T) {
	s, _, ops := baselineFixture(t)
	s.CancelPassed = false
	a, _ := parseAuthorization("keychain:example-admin")
	if err := s.skipCancellation(a, ops); err != nil {
		t.Fatal(err)
	}
	if !s.CancelSkipped || s.CancelPassed || !s.cancellationRecorded() {
		t.Fatal("incorrect cancellation semantics")
	}
	data, err := os.ReadFile(filepath.Join(s.Dir, "session.json"))
	if err != nil {
		t.Fatal(err)
	}
	var saved session
	if err := json.Unmarshal(data, &saved); err != nil || saved.CancelPassed || !saved.CancelSkipped {
		t.Fatal(err)
	}
	s.CancelSkipped = false
	if err := s.skipCancellation(authorization{}, ops); err == nil {
		t.Fatal("dialog mode silently skipped")
	}
}

func freezeCrash(t *testing.T, s *session) {
	t.Helper()
	plan := crashPlan()
	s.CrashScriptSHA256 = hashBytes([]byte(plan.Script))
	if err := writePrivate(filepath.Join(s.Dir, "crash.sh"), []byte(plan.Script)); err != nil {
		t.Fatal(err)
	}
}

func TestManagedAuthorizationRequiresFrozenScriptAndAudit(t *testing.T) {
	for _, mode := range []string{"missing", "changed", "audit-failure"} {
		t.Run(mode, func(t *testing.T) {
			s, _, ops := baselineFixture(t)
			a, _ := parseAuthorization("keychain:example-admin")
			a.run = func(context.Context, string, []string, io.Reader, io.Writer) error {
				t.Fatal("read secret before approval evidence")
				return nil
			}
			if mode != "missing" {
				freezeCrash(t, s)
			}
			if mode == "changed" {
				if err := writePrivate(filepath.Join(s.Dir, "crash.sh"), []byte("modified")); err != nil {
					t.Fatal(err)
				}
			}
			if mode == "audit-failure" {
				ops.archive = func(*session, string, any) (string, error) { return "", errors.New("audit failed") }
			}
			if err := s.authorize(context.Background(), "crash", crashPlan(), a, ops); err == nil {
				t.Fatal("unreviewed execution")
			}
		})
	}
}

func TestManagedCrashChecksOwnedActiveCaptureAndIdleRestart(t *testing.T) {
	s, c, ops := baselineFixture(t)
	freezeCrash(t, s)
	if err := c.Start(context.Background(), "test"); err != nil {
		t.Fatal(err)
	}
	id := *c.status.LastCapture
	s.Ownership.Captures = []api.CaptureIdentity{id}
	before := cloneSnapshot(s.Original)
	before.InterfaceIndices[id.Interface] = id.Index
	before.Routes = append(before.Routes, capture.Route{Prefix: netip.MustParsePrefix(id.Prefixes[0]), Interface: id.Interface, Index: id.Index})
	crashed := false
	ops.snapshot = func(context.Context) (snapshot, error) {
		if crashed {
			return s.Original, nil
		}
		return before, nil
	}
	a, _ := parseAuthorization("keychain:example-admin")
	a.run = func(_ context.Context, path string, args []string, _ io.Reader, out io.Writer) error {
		if path == "/usr/bin/security" {
			_, err := io.WriteString(out, "synthetic-password\n")
			return err
		}
		if path != "/usr/bin/sudo" || args[len(args)-1] != crashPlan().Script {
			t.Fatal("wrong crash command")
		}
		crashed = true
		c.status.State = api.Stopped
		c.status.LastCapture = nil
		return nil
	}
	if err := crashCapture(context.Background(), s, c, a, ops); err != nil {
		t.Fatal(err)
	}
	if !crashed {
		t.Fatal("crash not exercised")
	}
	entries, err := os.ReadDir(s.Dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		data, _ := os.ReadFile(filepath.Join(s.Dir, entry.Name()))
		if bytes.Contains(data, []byte("synthetic-password")) {
			t.Fatal("secret in private artifact")
		}
	}
	crashed = false
	if err := crashCapture(context.Background(), s, c, a, ops); err == nil || crashed {
		t.Fatal("idle helper was a crash target")
	}
}
