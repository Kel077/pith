package main

import (
	"context"
	"errors"
	"net/netip"
	"os"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/Kel077/pith/internal/api"
)

const testWebContent = "/System/Library/ExtensionKit/Extensions/WebKit.WebContent.EnhancedSecurity.appex/Contents/MacOS/WebKit.WebContent.EnhancedSecurity"

type fakeBrowserProcesses struct {
	now      time.Time
	rows     []processRecord
	signals  []string
	onSignal func(processRecord, syscall.Signal)
	onPause  func()
	problem  error
}

func (f *fakeBrowserProcesses) ops() webkitProcessOps {
	return webkitProcessOps{
		now: func() time.Time { return f.now },
		snapshot: func(ctx context.Context) (processInventory, error) {
			if err := ctx.Err(); err != nil {
				return processInventory{}, err
			}
			return processInventory{At: f.now, Processes: append([]processRecord(nil), f.rows...)}, f.problem
		},
		pause: func(ctx context.Context, d time.Duration) error {
			if err := ctx.Err(); err != nil {
				return err
			}
			f.now = f.now.Add(d)
			if f.onPause != nil {
				f.onPause()
			}
			return nil
		},
		signal: func(_ context.Context, p processRecord, sig syscall.Signal) error {
			f.signals = append(f.signals, p.Name+":"+sig.String())
			if f.onSignal != nil {
				f.onSignal(p, sig)
			} else {
				f.remove(p.PID)
			}
			return nil
		},
	}
}

func (f *fakeBrowserProcesses) remove(pid int) {
	for i, p := range f.rows {
		if p.PID == pid {
			f.rows = append(f.rows[:i], f.rows[i+1:]...)
			return
		}
	}
}

func fakeProcess(pid int, name, path string, started time.Time) processRecord {
	return processRecord{PID: pid, Parent: 1, UID: os.Getuid(), Started: started, Name: name, Path: path, UniqueID: uint64(pid) + 1000, Version: uint32(pid) + 2000, Resource: 100, Jetsam: 200}
}

// Existing PCB tests inject the process lifecycle too. They never enumerate,
// open, or signal a real browser, even when testing --webkit-open behavior.
func fakeWebKitLifecycle(t *testing.T, s *session, deps webkitCheckDeps) webkitCheckDeps {
	t.Helper()
	f := &fakeBrowserProcesses{now: time.Now()}
	deps.processes = f.ops()
	open := deps.open
	deps.open = func(ctx context.Context, app, url string) error {
		f.rows = []processRecord{fakeProcess(701, "Safari", s.WebKit.Executable, f.now)}
		if open != nil {
			return open(ctx, app, url)
		}
		return nil
	}
	return deps
}

func TestWebKitRejectsRunningSafariAndUncertainBaseline(t *testing.T) {
	plan := &webkitPlan{Executable: "/Applications/Safari.app/Contents/MacOS/Safari"}
	for _, mode := range []string{"running", "path-unreadable", "metadata-unreadable", "snapshot-failure"} {
		t.Run(mode, func(t *testing.T) {
			f := &fakeBrowserProcesses{now: time.Now()}
			p := fakeProcess(700, "Safari", plan.Executable, f.now.Add(-time.Hour))
			switch mode {
			case "path-unreadable":
				p.Path, p.Name = "", "unknown"
			case "metadata-unreadable":
				p.Problem, p.Name = "permission denied", "unknown"
				p.Path = "/Applications/Example.app/Contents/MacOS/Example"
			case "snapshot-failure":
				f.problem = errors.New("snapshot unavailable")
			}
			f.rows = []processRecord{p}
			var evidence webkitProcessEvidence
			if _, err := beginWebKitProcesses(context.Background(), plan, os.Getuid(), true, f.ops(), &evidence); err == nil {
				t.Fatal("unsafe automatic launch admitted")
			}
			if mode == "running" && !evidence.SafariAlreadyRunning {
				t.Fatal("pre-existing browser omitted")
			}
			if len(f.signals) != 0 {
				t.Fatal(f.signals)
			}
		})
	}
}

func TestWebKitCleanupOwnsOnlyNewBrowserCoalition(t *testing.T) {
	plan := &webkitPlan{Executable: "/Applications/Safari.app/Contents/MacOS/Safari"}
	f := &fakeBrowserProcesses{now: time.Now()}
	old := fakeProcess(610, "old WebKit", testWebContent, f.now.Add(-time.Hour))
	old.Jetsam = 99
	f.rows = []processRecord{old}
	var evidence webkitProcessEvidence
	w, err := beginWebKitProcesses(context.Background(), plan, os.Getuid(), true, f.ops(), &evidence)
	if err != nil {
		t.Fatal(err)
	}
	w.attemptedOpen()
	browser := fakeProcess(701, "Safari", plan.Executable, f.now)
	web := fakeProcess(702, "WebKit", testWebContent, f.now)
	other := fakeProcess(703, "other app WebKit", testWebContent, f.now)
	other.Jetsam = 333
	f.rows = append(f.rows, browser, web, other)
	if _, err := w.observe(context.Background(), true); err != nil {
		t.Fatal(err)
	}
	f.onSignal = func(p processRecord, sig syscall.Signal) {
		if p.PID == browser.PID && sig == syscall.SIGTERM || p.PID == web.PID && sig == syscall.SIGKILL {
			f.remove(p.PID)
			return
		}
		t.Fatal("unexpected signal", p, sig)
	}
	if err := w.finish(); err != nil {
		t.Fatal(err)
	}
	if !evidence.Complete || len(evidence.Processes) != 2 || evidence.Processes[0].State != "normal-exit" || evidence.Processes[1].State != "forced-exit" || len(evidence.OtherNew) != 1 || evidence.OtherNew[0].PID != other.PID {
		t.Fatalf("%+v", evidence)
	}
	if len(f.rows) != 2 || f.rows[0].PID != old.PID || f.rows[1].PID != other.PID || len(f.signals) != 2 || f.now.Sub(evidence.OpenAttemptAt) != 3*time.Second {
		t.Fatal(f.rows, f.signals, f.now.Sub(evidence.OpenAttemptAt))
	}
}

func TestWebKitNaturalExitAndPIDReuse(t *testing.T) {
	for _, mode := range []string{"natural", "pid-reuse", "exec-change", "unreadable", "no-coalition", "shared-coalition", "untrusted-path", "wrong-parent", "safari-stubborn", "manual"} {
		t.Run(mode, func(t *testing.T) {
			plan := &webkitPlan{Executable: "/Applications/Safari.app/Contents/MacOS/Safari"}
			f := &fakeBrowserProcesses{now: time.Now()}
			if mode == "shared-coalition" {
				f.rows = []processRecord{fakeProcess(600, "other app", "/Applications/Example.app/Contents/MacOS/Example", f.now.Add(-time.Hour))}
			}
			var evidence webkitProcessEvidence
			w, err := beginWebKitProcesses(context.Background(), plan, os.Getuid(), mode != "manual", f.ops(), &evidence)
			if err != nil {
				t.Fatal(err)
			}
			if mode != "manual" {
				w.attemptedOpen()
			}
			browser := fakeProcess(701, "Safari", plan.Executable, f.now)
			web := fakeProcess(702, "WebKit", testWebContent, f.now)
			if mode == "no-coalition" {
				web.Jetsam = 0
			}
			if mode == "untrusted-path" {
				web.Path = "/tmp/WebKit.WebContent.EnhancedSecurity"
			}
			if mode == "wrong-parent" {
				web.Parent = 888
			}
			f.rows = append(f.rows, browser, web)
			if _, err := w.observe(context.Background(), true); err != nil {
				t.Fatal(err)
			}
			f.onSignal = func(p processRecord, sig syscall.Signal) {
				if p.PID != 701 || sig != syscall.SIGTERM {
					t.Fatal("unexpected signal", p, sig)
				}
				if mode == "safari-stubborn" {
					return
				}
				f.remove(701)
				if mode == "natural" {
					f.remove(702)
				}
				for i := range f.rows {
					if f.rows[i].PID != 702 {
						continue
					}
					switch mode {
					case "pid-reuse":
						f.rows[i].Started = f.now.Add(time.Second)
						f.rows[i].UniqueID++
						f.rows[i].Version++
						f.rows[i].Jetsam = 555
					case "exec-change":
						f.rows[i].Version++
					case "unreadable":
						f.rows[i].Problem = "identity unavailable"
					}
				}
			}
			err = w.finish()
			if mode == "natural" || mode == "pid-reuse" || mode == "manual" || mode == "untrusted-path" {
				if err != nil {
					t.Fatal(err)
				}
			} else if err == nil {
				t.Fatal("cleanup uncertainty claimed success", mode)
			}
			want := 1
			if mode == "manual" {
				want = 0
			}
			if len(f.signals) != want {
				t.Fatal(f.signals)
			}
		})
	}
}

func TestWebKitCleanupAfterFailureAndCancellation(t *testing.T) {
	for _, mode := range []string{"passed", "incomplete", "open-error", "canceled", "wrong-route", "already-running", "started-after-ready"} {
		t.Run(mode, func(t *testing.T) {
			s, c, cfg, row, proof := webkitFixture(t)
			f := &fakeBrowserProcesses{now: time.Now()}
			browser := fakeProcess(701, "Safari", s.WebKit.Executable, f.now)
			if mode == "already-running" {
				browser.Started = f.now.Add(-time.Hour)
				f.rows = []processRecord{browser}
			}
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			calls := 0
			deps := webkitCheckDeps{processes: f.ops(), ready: func(string) error {
				if mode == "started-after-ready" {
					f.rows = []processRecord{browser}
				}
				row.Started, proof.At = time.Now(), time.Now()
				c.logs = []api.Connection{row}
				return nil
			}, open: func(context.Context, string, string) error {
				calls++
				f.rows = []processRecord{browser}
				if mode == "open-error" {
					return errors.New("open failed after submission")
				}
				if mode == "canceled" {
					cancel()
				}
				return nil
			}, lookup: func(context.Context, api.Protocol, netip.AddrPort, netip.AddrPort) (webkitPCB, error) {
				if mode == "incomplete" {
					return webkitPCB{}, errors.New("no PCB")
				}
				return proof, nil
			}}
			if mode == "wrong-route" {
				row.Interface = s.Interfaces[1].Name
			}
			status, _ := c.Status(context.Background())
			var report checkReport
			err := runWebKitCheck(ctx, s, c, cfg, status, &report, 100*time.Millisecond, true, deps)
			if mode == "passed" {
				if err != nil {
					t.Fatal(err)
				}
			} else if err == nil {
				t.Fatal("expected failure")
			}
			if mode == "already-running" || mode == "started-after-ready" {
				if calls != 0 || len(f.signals) != 0 || !report.WebKit.Cleanup.SafariAlreadyRunning {
					t.Fatal(calls, f.signals, report.WebKit)
				}
			} else if calls != 1 || len(f.signals) != 1 || !report.WebKit.Cleanup.Complete || len(f.rows) != 0 || len(s.ProcessWindows) != 1 || s.ProcessWindows[0].Ended.IsZero() {
				t.Fatal(calls, f.signals, report.WebKit)
			}
		})
	}
}

func TestProcessAuditUsesStartWindowForAllOwners(t *testing.T) {
	now := time.Now()
	windows := []processWindow{{Started: now, Ended: now.Add(time.Minute)}}
	old := fakeProcess(700, "old", "old", now.Add(-time.Second))
	newRoot := fakeProcess(701, "system service", "", now.Add(time.Second))
	newRoot.UID = 0
	newApp := fakeProcess(702, "unrelated app", "app", now.Add(30*time.Second))
	later := fakeProcess(703, "audit process", "audit", now.Add(2*time.Minute))
	report := auditProcesses(windows, processInventory{At: now.Add(3 * time.Minute), Processes: []processRecord{old, newRoot, newApp, later}})
	if len(report.Remaining) != 2 || report.Remaining[0].PID != 701 || report.Remaining[1].PID != 702 || !strings.Contains(report.Notes, "never authorizes a signal") {
		t.Fatal(report)
	}
}
