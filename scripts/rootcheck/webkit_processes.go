package main

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"syscall"
	"time"
)

type webkitProcessOps struct {
	snapshot func(context.Context) (processInventory, error)
	signal   func(context.Context, processRecord, syscall.Signal) error
	now      func() time.Time
	pause    func(context.Context, time.Duration) error
}

func liveWebKitProcesses() webkitProcessOps {
	return webkitProcessOps{liveProcessInventory, liveProcessSignal, time.Now, processPause}
}

type processExit struct {
	Process processRecord `json:"process"`
	Role    string        `json:"role"`
	State   string        `json:"state"`
	Signals []string      `json:"signals,omitempty"`
	Problem string        `json:"problem,omitempty"`
}

type webkitProcessEvidence struct {
	BeforeAt             time.Time       `json:"beforeAt"`
	SafariAlreadyRunning bool            `json:"safariAlreadyRunning"`
	OpenAttemptAt        time.Time       `json:"openAttemptAt,omitempty"`
	Finished             time.Time       `json:"finished,omitempty"`
	Complete             bool            `json:"cleanupComplete"`
	Manual               bool            `json:"manualMode"`
	Processes            []*processExit  `json:"ownedProcesses,omitempty"`
	OtherNew             []processRecord `json:"otherNewProcessesStillPresent,omitempty"`
	Errors               []string        `json:"errors,omitempty"`
}

type webkitProcessWatch struct {
	plan     *webkitPlan
	uid      int
	ops      webkitProcessOps
	before   processInventory
	evidence *webkitProcessEvidence
	browser  *processExit
	lastRead time.Time
}

func safariProcess(p processRecord, plan *webkitPlan) bool {
	// Include other Safari installations too: LaunchServices may reuse a process.
	return p.Path == plan.Executable || p.Name == "Safari" || filepath.Base(p.Path) == "Safari"
}

func beginWebKitProcesses(ctx context.Context, plan *webkitPlan, uid int, launch bool, ops webkitProcessOps, evidence *webkitProcessEvidence) (*webkitProcessWatch, error) {
	if ops.snapshot == nil || ops.signal == nil || ops.now == nil || ops.pause == nil {
		return nil, errors.New("browser process lifecycle dependencies unavailable")
	}
	before, err := ops.snapshot(ctx)
	if err != nil {
		return nil, fmt.Errorf("browser process baseline: %w", err)
	}
	evidence.BeforeAt, evidence.Manual = before.At, !launch
	complete := true
	for _, p := range before.Processes {
		if safariProcess(p, plan) {
			evidence.SafariAlreadyRunning = true
		}
		if p.UID == uid && (p.Problem != "" || p.Path == "") {
			complete = false
		}
	}
	if launch && evidence.SafariAlreadyRunning {
		return nil, errors.New("Safari is already running; use manual mode or close it before --webkit-open")
	}
	if launch && !complete {
		return nil, errors.New("cannot establish absence of Safari and pre-existing coalition membership; use manual mode")
	}
	return &webkitProcessWatch{plan: plan, uid: uid, ops: ops, before: before, evidence: evidence}, nil
}

func (w *webkitProcessWatch) attemptedOpen() { w.evidence.OpenAttemptAt = w.ops.now() }

func (w *webkitProcessWatch) isNew(p processRecord) bool {
	if w.evidence.OpenAttemptAt.IsZero() || p.Started.Before(w.evidence.OpenAttemptAt) || p.Started.After(w.ops.now()) {
		return false
	}
	for _, old := range w.before.Processes {
		// Start identity alone protects processes whose detail sampling failed.
		if old.PID == p.PID && old.Started.Equal(p.Started) {
			return false
		}
	}
	return true
}

// Trust only the operating system's WebKit services, not a matching basename in
// an arbitrary application. Coalition membership independently ties each service
// to the newly launched Safari, including launchd-parented ExtensionKit services.
func webkitService(path string) bool {
	if filepath.Clean(path) != path {
		return false
	}
	trusted := false
	for _, root := range []string{"/System/Library/", "/System/Cryptexes/App/System/Library/", "/System/Volumes/Preboot/Cryptexes/App/System/Library/"} {
		if strings.HasPrefix(path, root) {
			trusted = true
			break
		}
	}
	if !trusted {
		return false
	}
	name := filepath.Base(path)
	for _, prefix := range []string{"com.apple.WebKit.", "WebKit."} {
		for _, role := range []string{"WebContent", "WebContent.EnhancedSecurity", "WebContent.CaptivePortal", "Networking", "GPU"} {
			if name != prefix+role {
				continue
			}
			if strings.HasSuffix(path, "/XPCServices/"+name+".xpc/Contents/MacOS/"+name) || strings.HasSuffix(path, "/ExtensionKit/Extensions/"+name+".appex/Contents/MacOS/"+name) {
				return true
			}
		}
	}
	return false
}

func (w *webkitProcessWatch) coalitionIsNew(p processRecord) bool {
	if p.Resource == 0 || p.Jetsam == 0 {
		return false
	}
	for _, old := range w.before.Processes {
		if old.Jetsam == p.Jetsam {
			return false
		}
	}
	return true
}

func (w *webkitProcessWatch) observe(ctx context.Context, force bool) (processInventory, error) {
	if !force && w.ops.now().Sub(w.lastRead) < 250*time.Millisecond {
		return processInventory{}, nil
	}
	inv, err := w.ops.snapshot(ctx)
	if err != nil {
		return inv, err
	}
	w.lastRead = w.ops.now()
	if w.evidence.Manual || w.evidence.OpenAttemptAt.IsZero() {
		return inv, nil
	}
	var candidates []processRecord
	for _, p := range inv.Processes {
		if (p.Path == w.plan.Executable || canonicalSame(p.Path, w.plan.Executable)) && p.UID == w.uid && p.Parent == 1 && w.isNew(p) && p.Problem == "" && p.UniqueID != 0 && p.Version != 0 {
			candidates = append(candidates, p)
		}
	}
	if len(candidates) > 1 {
		return inv, errors.New("multiple new Safari processes; cleanup ownership is ambiguous")
	}
	if w.browser == nil && len(candidates) == 1 {
		w.browser = &processExit{Process: candidates[0], Role: "Safari", State: "observed"}
		w.evidence.Processes = append(w.evidence.Processes, w.browser)
	}
	if w.browser != nil {
		browser := w.browser.Process
		for _, p := range inv.Processes {
			if !webkitService(p.Path) || !w.isNew(p) || p.Parent != 1 || p.UID != w.uid || p.Problem != "" || p.UniqueID == 0 || p.Version == 0 || p.Jetsam != browser.Jetsam || p.Resource != browser.Resource || !w.coalitionIsNew(p) {
				continue
			}
			found := false
			// An already observed PID cannot be readopted after exec or reuse.
			for _, record := range w.evidence.Processes {
				found = found || record.Process.PID == p.PID
			}
			if !found {
				w.evidence.Processes = append(w.evidence.Processes, &processExit{Process: p, Role: "WebKit", State: "observed"})
			}
		}
	}
	for _, record := range w.evidence.Processes {
		if processPresent(inv, record.Process) && processExited(record) {
			record.State = "present-again"
		}
		if !processPresent(inv, record.Process) && !processExited(record) {
			if len(record.Signals) != 0 && record.Signals[len(record.Signals)-1] == "SIGKILL" {
				record.State = "forced-exit"
			} else {
				record.State = "normal-exit"
			}
		}
	}
	return inv, nil
}

func processPresent(inv processInventory, wanted processRecord) bool {
	for _, p := range inv.Processes {
		if p.PID == wanted.PID && p.Started.Equal(wanted.Started) {
			// An unreadable or exec-changed identity is still a live process, not an
			// exit. It cannot be signaled until exact identity is independently known.
			return true
		}
	}
	return false
}

func (w *webkitProcessWatch) send(ctx context.Context, record *processExit, sig syscall.Signal) error {
	inv, err := w.observe(ctx, true)
	if err != nil {
		return err
	}
	if !processPresent(inv, record.Process) {
		return nil
	}
	var current *processRecord
	for _, p := range inv.Processes {
		if sameProcess(record.Process, p) && p.Problem == "" && p.Resource == record.Process.Resource && p.Jetsam == record.Process.Jetsam {
			copy := p
			current = &copy
			break
		}
	}
	if current == nil {
		return errProcessChanged
	}
	if err := w.ops.signal(ctx, *current, sig); err != nil {
		if errors.Is(err, syscall.ESRCH) {
			return nil
		}
		return err
	}
	if sig == syscall.SIGTERM {
		record.Signals = append(record.Signals, "SIGTERM")
		record.State = "term-sent"
	} else {
		record.Signals = append(record.Signals, "SIGKILL")
		record.State = "kill-sent"
	}
	return nil
}

func (w *webkitProcessWatch) wait(ctx context.Context, duration time.Duration, done func() bool) error {
	deadline := w.ops.now().Add(duration)
	for {
		if _, err := w.observe(ctx, true); err != nil {
			return err
		}
		if done() {
			return nil
		}
		if !w.ops.now().Before(deadline) {
			return context.DeadlineExceeded
		}
		if err := w.ops.pause(ctx, 100*time.Millisecond); err != nil {
			return err
		}
	}
}

func processExited(p *processExit) bool { return p.State == "normal-exit" || p.State == "forced-exit" }

// Cleanup has an independent deadline so request cancellation cannot skip it.
// Safari receives TERM only; new, independently owned WebKit services get KILL
// only after Safari exits and their natural-exit grace expires. No global kill.
func (w *webkitProcessWatch) finish() (resultErr error) {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	defer func() {
		w.evidence.Finished = w.ops.now()
		inv, err := w.observe(ctx, true)
		if err != nil {
			resultErr = errors.Join(resultErr, err)
		} else {
			for _, p := range inv.Processes {
				if !w.isNew(p) {
					continue
				}
				owned := false
				for _, record := range w.evidence.Processes {
					owned = owned || sameProcess(p, record.Process)
				}
				if !owned {
					w.evidence.OtherNew = append(w.evidence.OtherNew, p)
				}
				if !w.evidence.Manual && (safariProcess(p, w.plan) || webkitService(p.Path)) && (!owned || p.Problem != "") {
					// Another application's proven different coalition remains listed,
					// but does not make this browser's cleanup fail.
					if w.browser == nil || p.Jetsam == 0 || p.Jetsam == w.browser.Process.Jetsam {
						resultErr = errors.Join(resultErr, fmt.Errorf("browser-related PID %d remains without safe cleanup identity", p.PID))
					}
				}
			}
		}
		for _, record := range w.evidence.Processes {
			if !processExited(record) {
				resultErr = errors.Join(resultErr, fmt.Errorf("owned %s PID %d remains", record.Role, record.Process.PID))
			}
		}
		w.evidence.Complete = resultErr == nil
		if resultErr != nil {
			w.evidence.Errors = append(w.evidence.Errors, resultErr.Error())
		}
	}()
	if w.evidence.Manual || w.evidence.OpenAttemptAt.IsZero() {
		return nil
	}
	if err := w.wait(ctx, 2*time.Second, func() bool { return w.browser != nil }); err != nil {
		return fmt.Errorf("cannot establish launched Safari identity: %w", err)
	}
	if err := w.send(ctx, w.browser, syscall.SIGTERM); err != nil {
		return err
	}
	if err := w.wait(ctx, 5*time.Second, func() bool { return processExited(w.browser) }); err != nil {
		return fmt.Errorf("Safari did not exit after SIGTERM: %w", err)
	}
	done := func() bool {
		for _, record := range w.evidence.Processes {
			if !processExited(record) {
				return false
			}
		}
		return true
	}
	// Observe the entire grace period even if known children already exited:
	// launchd may still deliver a service launch queued just before Safari exited.
	if err := w.wait(ctx, 3*time.Second, func() bool { return false }); err != nil && !errors.Is(err, context.DeadlineExceeded) {
		return err
	}
	for _, record := range w.evidence.Processes {
		if record.Role != "WebKit" || processExited(record) {
			continue
		}
		if err := w.send(ctx, record, syscall.SIGKILL); err != nil {
			record.Problem = err.Error()
			resultErr = errors.Join(resultErr, err)
		}
	}
	if err := w.wait(ctx, 2*time.Second, done); err != nil {
		resultErr = errors.Join(resultErr, fmt.Errorf("WebKit cleanup wait: %w", err))
	}
	return resultErr
}
