package main

import (
	"context"
	"encoding/binary"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Kel077/pith/internal/helper"
)

// This minimal Mach-O is parsed as data only; no test executes it.
func identityFixtureBytes() []byte {
	be, le := binary.BigEndian, binary.LittleEndian
	directory := make([]byte, 64)
	be.PutUint32(directory[0:4], 0xfade0c02)
	be.PutUint32(directory[4:8], uint32(len(directory)))
	be.PutUint32(directory[8:12], 0x20400)
	be.PutUint32(directory[12:16], 2)
	directory[36], directory[37] = 32, 2
	signature := make([]byte, 20)
	be.PutUint32(signature[0:4], 0xfade0cc0)
	be.PutUint32(signature[4:8], uint32(len(signature)+len(directory)))
	be.PutUint32(signature[8:12], 1)
	be.PutUint32(signature[16:20], 20)
	signature = append(signature, directory...)
	file := make([]byte, 48)
	le.PutUint32(file[0:4], 0xfeedfacf)
	le.PutUint32(file[4:8], 0x0100000c)
	le.PutUint32(file[12:16], 2)
	le.PutUint32(file[16:20], 1)
	le.PutUint32(file[20:24], 16)
	le.PutUint32(file[32:36], 0x1d)
	le.PutUint32(file[36:40], 16)
	le.PutUint32(file[40:44], uint32(len(file)))
	le.PutUint32(file[44:48], uint32(len(signature)))
	return append(file, signature...)
}

func identitySessionFixture(t *testing.T) (*session, *baselineService, rootcheckOps) {
	t.Helper()
	s, c, ops := baselineFixture(t)
	data := identityFixtureBytes()
	if err := os.WriteFile(s.Helper, data, 0700); err != nil {
		t.Fatal(err)
	}
	s.HelperSHA256 = hashBytes(data)
	if err := s.freezeHelperIdentity(); err != nil {
		t.Fatal(err)
	}
	plan, err := s.freezeCleanupHelper()
	if err != nil {
		t.Fatal(err)
	}
	s.UninstallScriptSHA256 = hashBytes([]byte(plan.Script))
	if err := writePrivate(filepath.Join(s.Dir, "uninstall.sh"), []byte(plan.Script)); err != nil {
		t.Fatal(err)
	}
	if err := s.save(); err != nil {
		t.Fatal(err)
	}
	return s, c, ops
}

func assertVerifiedCleanupPlan(t *testing.T, plan helper.InstallPlan, source, checksum string) {
	t.Helper()
	for _, want := range []string{shellQuote(source) + ` "$stage/cleanup-helper"`, shellQuote(checksum), `"$stage/cleanup-helper" --cleanup`} {
		if !strings.Contains(plan.Script, want) {
			t.Fatalf("verified cleanup plan omits %q", want)
		}
	}
	for _, path := range []string{source, helper.BinaryPath} {
		if strings.Contains(plan.Script, shellQuote(path)+" --cleanup") {
			t.Fatal("cleanup executes a source or installed path without a verified stage")
		}
	}
	if strings.Index(plan.Script, `actual=$(/usr/bin/shasum`) > strings.Index(plan.Script, `"$stage/cleanup-helper" --cleanup`) {
		t.Fatal("cleanup runs before the privileged copy is verified")
	}
}

func TestFrozenHelperIdentitySurvivesRebuiltOrMissingSource(t *testing.T) {
	for _, source := range []string{"rebuilt", "deleted"} {
		t.Run(source, func(t *testing.T) {
			s, c, ops := identitySessionFixture(t)
			want := *s.HelperIdentity
			if source == "rebuilt" {
				if err := os.WriteFile(s.Helper, []byte("different build; never executed"), 0700); err != nil {
					t.Fatal(err)
				}
			} else if err := os.Remove(s.Helper); err != nil {
				t.Fatal(err)
			}
			loaded, err := loadSession(s.Dir)
			if err != nil {
				t.Fatal(err)
			}
			*s = *loaded
			id, err := s.expectedHelper()
			if err != nil || id != want {
				t.Fatalf("frozen identity = %+v, %v", id, err)
			}
			if _, err := sessionClient(s); err != nil {
				t.Fatalf("controller depends on its original source: %v", err)
			}
			if err := stopWithOps(context.Background(), s, c, ops); err != nil {
				t.Fatalf("Stop depends on its original source: %v", err)
			}
			plan, err := prepareUninstall(context.Background(), s, c, ops, filepath.Join(s.Dir, "absent-installed"), filepath.Join(s.Dir, "absent.plist"))
			if err != nil || c.stops != 2 {
				t.Fatalf("uninstall preparation: stops=%d err=%v", c.stops, err)
			}
			if hashBytes([]byte(plan.Script)) != s.UninstallScriptSHA256 {
				t.Fatal("source replacement changed the reviewed uninstall script")
			}
			assertVerifiedCleanupPlan(t, plan, filepath.Join(s.Dir, "cleanup-helper"), s.HelperSHA256)
		})
	}
}

func TestLegacyUninstallUsesOnlyReviewedInstalledBytesWithoutIPC(t *testing.T) {
	for _, variant := range []string{"missing-identity", "wrong-sha", "invalid-mach-o"} {
		t.Run(variant, func(t *testing.T) {
			s, c, ops := baselineFixture(t)
			installed := filepath.Join(s.Dir, "installed-helper")
			data := identityFixtureBytes()
			s.HelperSHA256 = hashBytes(data)
			if variant == "wrong-sha" {
				data[100] ^= 1
			}
			if variant == "invalid-mach-o" {
				data = []byte("unrecognized executable; never executed")
				s.HelperSHA256 = hashBytes(data)
			}
			if err := os.WriteFile(installed, data, 0700); err != nil {
				t.Fatal(err)
			}
			if _, err := sessionClient(s); err == nil {
				t.Fatal("legacy session constructed an unanchored IPC client")
			}
			plan, err := prepareUninstall(context.Background(), s, c, ops, installed, filepath.Join(s.Dir, "absent.plist"))
			if (err != nil) != (variant != "missing-identity") || c.stops != 0 || c.statuses != 0 {
				t.Fatalf("legacy removal: stops=%d status=%d err=%v", c.stops, c.statuses, err)
			}
			if err == nil {
				assertVerifiedCleanupPlan(t, plan, installed, s.HelperSHA256)
				// A dialog can review the stronger replacement for a legacy script.
				// Stop before execution so this test never invokes authorization.
				sentinel := errors.New("synthetic audit boundary")
				ops.archive = func(*session, string, any) (string, error) { return "", sentinel }
				if err := s.authorize(context.Background(), "uninstall", plan, authorization{}, ops); !errors.Is(err, sentinel) {
					t.Fatalf("legacy removal was blocked before its authorization boundary: %v", err)
				}
			}
		})
	}
}

func TestUninstallSkipsIncompatibleProtocolIPC(t *testing.T) {
	s, c, ops := identitySessionFixture(t)
	s.HelperIdentity.Protocol = helper.ProtocolVersion + 1
	if _, err := s.expectedHelper(); err == nil {
		t.Fatal("unsupported protocol accepted for IPC")
	}
	plan, err := prepareUninstall(context.Background(), s, c, ops, filepath.Join(s.Dir, "absent-installed"), filepath.Join(s.Dir, "absent.plist"))
	if err != nil || c.stops != 0 || c.statuses != 0 {
		t.Fatalf("offline recovery depends on IPC compatibility: stops=%d status=%d err=%v", c.stops, c.statuses, err)
	}
	assertVerifiedCleanupPlan(t, plan, filepath.Join(s.Dir, "cleanup-helper"), s.HelperSHA256)
}

func TestFrozenIdentityAndCleanupSourceChangesAreRejected(t *testing.T) {
	for _, variant := range []string{"sha", "cdhash", "schema", "cleanup-bytes"} {
		t.Run(variant, func(t *testing.T) {
			s, c, ops := identitySessionFixture(t)
			switch variant {
			case "sha":
				s.HelperIdentity.SHA256 = strings.Repeat("b", 64)
			case "cdhash":
				s.HelperIdentity.CDHash = strings.Repeat("c", 40)
			case "schema":
				s.HelperIdentity.Schema++
			case "cleanup-bytes":
				if err := os.WriteFile(filepath.Join(s.Dir, "cleanup-helper"), []byte("changed cleanup bytes"), 0700); err != nil {
					t.Fatal(err)
				}
			}
			if _, err := prepareUninstall(context.Background(), s, c, ops, filepath.Join(s.Dir, "absent-installed"), filepath.Join(s.Dir, "absent.plist")); err == nil || c.stops != 0 {
				t.Fatalf("changed cleanup identity accepted: stops=%d err=%v", c.stops, err)
			}
		})
	}
	s, _, _ := identitySessionFixture(t)
	s.HelperSHA256 = strings.Repeat("b", 64)
	if err := s.freezeHelperIdentity(); err == nil {
		t.Fatal("preparation accepted a helper differing from the original SHA-256")
	}
}

func TestFrozenUninstallScriptIsCheckedForDialogAndManagedAuthorization(t *testing.T) {
	for _, mode := range []string{"dialog", "managed"} {
		for _, variant := range []string{"unchanged", "missing", "changed", "different-plan", "missing-checksum"} {
			t.Run(mode+"/"+variant, func(t *testing.T) {
				s, _, ops := identitySessionFixture(t)
				plan, err := s.reviewedUninstallPlan(filepath.Join(s.Dir, "absent-installed"))
				if err != nil {
					t.Fatal(err)
				}
				switch variant {
				case "missing":
					if err := os.Remove(filepath.Join(s.Dir, "uninstall.sh")); err != nil {
						t.Fatal(err)
					}
				case "changed":
					if err := writePrivate(filepath.Join(s.Dir, "uninstall.sh"), []byte("modified")); err != nil {
						t.Fatal(err)
					}
				case "different-plan":
					plan.Script += "\n"
				case "missing-checksum":
					s.UninstallScriptSHA256 = ""
				}
				a := authorization{}
				if mode == "managed" {
					a.service = "example-admin"
				}
				a.run = func(context.Context, string, []string, io.Reader, io.Writer) error {
					t.Fatal("authorization must not execute")
					return nil
				}
				sentinel := errors.New("synthetic audit boundary")
				audited := false
				ops.archive = func(*session, string, any) (string, error) { audited = true; return "", sentinel }
				err = s.authorize(context.Background(), "uninstall", plan, a, ops)
				if err == nil || audited != (variant == "unchanged") {
					t.Fatalf("review validation: audited=%v err=%v", audited, err)
				}
			})
		}
	}
}

func TestHelperDisplayVersionDoesNotControlStopOrStart(t *testing.T) {
	for _, version := range []string{"", "different-display-version"} {
		t.Run(version, func(t *testing.T) {
			s, c, ops := baselineFixture(t)
			c.status.Helper.Version = version
			if err := s.startCapture(context.Background(), c, "test", ops); err != nil {
				t.Fatal(err)
			}
			if err := stopWithOps(context.Background(), s, c, ops); err != nil {
				t.Fatal(err)
			}
			if c.starts != 1 || c.stops != 1 {
				t.Fatalf("unexpected mutations: starts=%d stops=%d", c.starts, c.stops)
			}
		})
	}
}
