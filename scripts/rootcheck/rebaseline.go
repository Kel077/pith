package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/netip"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"time"

	"github.com/Kel077/pith/internal/api"
	"github.com/Kel077/pith/internal/engine/capture"
	"github.com/Kel077/pith/internal/helper"
)

// installationID distinguishes separate installations with identical reviewed
// bytes and launchd arguments. It is observed only; the helper needs no change.
type installationID struct {
	Device   uint64 `json:"device"`
	Inode    uint64 `json:"inode"`
	Modified int64  `json:"modifiedUnixNano"`
	Size     int64  `json:"size"`
}

type cleanupProof struct {
	File   string `json:"file"`
	SHA256 string `json:"sha256"`
	Kind   string `json:"kind,omitempty"`
}

type cleanupRecord struct {
	At                    time.Time       `json:"at"`
	Operation             string          `json:"operation"`
	Label                 string          `json:"baselineLabel"`
	Binding               string          `json:"sessionAndBaselineSHA256"`
	Status                api.Status      `json:"status"`
	After                 snapshot        `json:"after"`
	Restored              bool            `json:"restored"`
	Passed                bool            `json:"passed"`
	Error                 string          `json:"error,omitempty"`
	External              *externalChange `json:"externalChange,omitempty"`
	ClassificationProblem string          `json:"classificationProblem,omitempty"`
}

// The production operations have no privileged side effects. Tests replace
// these boundaries without taking live snapshots or opening network sockets.
type rootcheckOps struct {
	snapshot        func(context.Context) (snapshot, error)
	verifyInstalled func(*session) error
	probe           func(context.Context, *session, string) (tlsEvidence, error)
	save            func(*session) error
	archive         func(*session, string, any) (string, error)
}

func liveOps() rootcheckOps {
	return rootcheckOps{
		snapshot: takeSnapshot, verifyInstalled: (*session).verifyOwnedInstallation,
		probe: func(ctx context.Context, s *session, iface string) (tlsEvidence, error) {
			return exchangeTLS(ctx, s, iface, nil)
		},
		save: (*session).save, archive: archiveJSON,
	}
}

func readInstallationID(path string) (installationID, error) {
	info, err := os.Lstat(path)
	if err != nil {
		return installationID{}, err
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !info.Mode().IsRegular() || !ok {
		return installationID{}, errors.New("installed plist must be a regular file")
	}
	return installationID{Device: uint64(stat.Dev), Inode: uint64(stat.Ino), Modified: info.ModTime().UnixNano(), Size: info.Size()}, nil
}

func (s *session) verifyOwnedInstallation() error {
	if !s.Installed || s.Installation == nil {
		return errors.New("this session has no recorded installation identity; use its reviewed uninstall plan")
	}
	if err := s.verifyInstalled(); err != nil {
		return err
	}
	return s.verifyInstallationID(helper.PlistPath, false)
}

func (s *session) verifyInstallationID(path string, allowAbsent bool) error {
	actual, err := readInstallationID(path)
	if allowAbsent && errors.Is(err, os.ErrNotExist) {
		return nil // Removal must still recover a partially removed installation.
	}
	if err != nil {
		return err
	}
	if s.Installation == nil || actual != *s.Installation {
		return errors.New("installed plist belongs to a different installation; review ownership before any action")
	}
	return nil
}

func (s *session) verifyRemovalOwnership(binary, plist string) error {
	if s.Installation != nil {
		if err := s.verifyInstallationID(plist, true); err != nil {
			return err
		}
	}
	// A canceled dialog or interrupted install may leave no instance marker.
	// Inspect every surviving installation file, permitting missing files from
	// partial cleanup but never treating a different normal helper as our own.
	for _, path := range []string{binary, plist} {
		info, err := os.Lstat(path)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return err
		}
		if !info.Mode().IsRegular() {
			return errors.New("refuse removal of an unrecognized installation file")
		}
		if path == binary {
			checksum, err := fileHash(path)
			if err != nil || checksum != s.HelperSHA256 {
				return errors.New("surviving helper is not this session's reviewed executable")
			}
		} else if err := s.verifyInstalledPlist(path); err != nil {
			return err
		}
	}
	return nil
}

func idleReviewedStatus(s *session, st api.Status) error {
	if !st.Helper.Installed || !st.Helper.Running || st.Helper.Outdated || st.Helper.Version != s.Version || st.State != api.Stopped {
		return fmt.Errorf("requires a responding, installed helper at version %q in stopped state: version=%q state=%s", s.Version, st.Helper.Version, st.State)
	}
	return nil
}

func (s *session) baselineDigest() string {
	data, _ := json.Marshal(struct {
		Label      string
		File       string
		Snapshot   snapshot
		Preflight  []preflight
		Interfaces [2]api.InterfaceRef
	}{s.BaselineLabel, s.BaselineFile, s.Original, s.Preflight, s.Interfaces})
	return hashBytes(data)
}

func (s *session) cleanupBinding() string {
	data, _ := json.Marshal(struct {
		Directory, Helper, HelperSHA256, InstallScriptSHA256, Version string
		Target, ServerName, DNSTarget, Baseline                       string
		UID                                                           int
		Installation                                                  *installationID
		Ownership                                                     captureHistory
	}{s.Dir, s.Helper, s.HelperSHA256, s.InstallScriptSHA256, s.Version,
		s.Target, s.ServerName, s.DNSTarget, s.baselineDigest(), s.UID, s.Installation, s.Ownership})
	return hashBytes(data)
}

func (s *session) invalidateCleanup(ops rootcheckOps) error {
	s.Cleanup = nil
	// A failed save prevents Start or Stop from running. There is no new capture
	// whose cleanup could be mistaken for the previously persisted evidence.
	return ops.save(s)
}

func (s *session) startCapture(ctx context.Context, c api.Service, scene string, ops rootcheckOps) error {
	if err := s.invalidateCleanup(ops); err != nil {
		return fmt.Errorf("persist invalidated cleanup evidence before Start: %w", err)
	}
	// Recheck after preparation, subscription, or opening a preserved connection:
	// an unrelated installer must not turn a scoped test into ordinary capture.
	if err := ops.verifyInstalled(s); err != nil {
		return err
	}
	st, err := c.Status(ctx)
	if err != nil {
		return err
	}
	if err := idleReviewedStatus(s, st); err != nil {
		return err
	}
	s.rememberEnded(st)
	return s.startWithIdentity(ctx, c, scene, st.LastCapture, ops)
}

func (s *session) requireCleanup(acceptExternal ...bool) error {
	proof := s.Cleanup
	if proof == nil || filepath.Base(proof.File) != proof.File || !strings.HasPrefix(proof.File, "cleanup-") || !strings.HasSuffix(proof.File, ".json") {
		return errors.New("rebaseline requires persisted successful stop/check cleanup under the current baseline")
	}
	path := filepath.Join(s.Dir, proof.File)
	info, err := os.Lstat(path)
	if err != nil || !info.Mode().IsRegular() || info.Mode().Perm() != 0600 {
		return errors.New("cleanup evidence is missing or is not a private regular file")
	}
	data, err := os.ReadFile(path)
	if err != nil || hashBytes(data) != proof.SHA256 {
		return errors.New("cleanup evidence checksum changed")
	}
	var record cleanupRecord
	if err := json.Unmarshal(data, &record); err != nil {
		return err
	}
	if (record.Operation != "stop" && record.Operation != "check") || record.Binding != s.cleanupBinding() || idleReviewedStatus(s, record.Status) != nil {
		return errors.New("cleanup evidence does not belong to this idle installation and baseline")
	}
	if proof.Kind == externalProof {
		if len(acceptExternal) == 0 || !acceptExternal[0] {
			return errExternalChange
		}
		if record.Passed || record.Restored || record.External == nil || record.Error != errExternalChange.Error() {
			return errors.New("invalid external-change cleanup evidence")
		}
		return s.recheckExternal(record.After, record.Status, record.External)
	}
	if proof.Kind != "" || record.External != nil || !record.Passed || !record.Restored || record.Error != "" || !sameCleanupBaseline(s.Original, record.After) {
		return errors.New("cleanup evidence does not prove this installation restored the current session baseline")
	}
	return nil
}

// collectCleanup attempts Stop only for this installation and records the
// independent observations even when identity or cleanup verification fails.
// Callers must invalidate old evidence before any capture or cleanup attempt.
func collectCleanup(ctx context.Context, s *session, c api.Service, ops rootcheckOps, operation string) (cleanupRecord, error) {
	record := cleanupRecord{At: time.Now(), Operation: operation, Label: s.BaselineLabel, Binding: s.cleanupBinding()}
	var resultErr error
	if err := ops.verifyInstalled(s); err != nil {
		resultErr = fmt.Errorf("refuse to stop an unverified installation: %w", err)
	} else if err := c.Stop(ctx); err != nil {
		resultErr = fmt.Errorf("Stop failed: %w; use the reviewed emergency command", err)
	}
	var err error
	record.Status, err = c.Status(ctx)
	if err == nil {
		err = idleReviewedStatus(s, record.Status)
	}
	resultErr = errors.Join(resultErr, err)
	if err := ops.verifyInstalled(s); err != nil {
		resultErr = errors.Join(resultErr, err)
	}
	record.After, err = ops.snapshot(ctx)
	if err == nil {
		st, statusErr := c.Status(ctx)
		if statusErr == nil {
			statusErr = idleReviewedStatus(s, st)
		}
		resultErr = errors.Join(resultErr, statusErr, ops.verifyInstalled(s))
		if statusErr == nil {
			record.Status = st
		}
		if resultErr == nil {
			s.rememberEnded(record.Status)
			record.Binding = s.cleanupBinding()
		}
		record.Restored = sameCleanupBaseline(s.Original, record.After)
		if !record.Restored {
			if resultErr == nil {
				if diff, classifyErr := s.externalDifferences(record.After, record.Status); classifyErr == nil {
					record.External = diff
					return record, errExternalChange
				} else {
					record.ClassificationProblem = classifyErr.Error()
				}
			}
			err = errors.New("routes or tunnel inventory differ from the current baseline; inspect the private snapshots")
		}
	}
	return record, errors.Join(resultErr, err)
}

func (s *session) recordCleanup(record cleanupRecord, resultErr error, ops rootcheckOps) error {
	s.Cleanup = nil
	record.Passed = resultErr == nil
	if resultErr != nil {
		record.Error = resultErr.Error()
	}
	name, err := ops.archive(s, "cleanup-"+record.Operation, record)
	if err != nil {
		return err
	}
	external := record.External != nil && resultErr == errExternalChange && s.recheckExternal(record.After, record.Status, record.External) == nil
	if record.Passed || external {
		checksum, err := fileHash(filepath.Join(s.Dir, name))
		if err != nil {
			return err
		}
		s.Cleanup = &cleanupProof{File: name, SHA256: checksum}
		if external {
			s.Cleanup.Kind = externalProof
			fmt.Println("External-change cleanup evidence (not a test pass):", filepath.Join(s.Dir, name))
		}
	}
	if err := ops.save(s); err != nil {
		s.Cleanup = nil
		return err
	}
	return nil
}

// archiveJSON never replaces a previous report, even within one clock tick.
// session.json alone chooses which immutable candidate is the active baseline.
func archiveJSON(s *session, prefix string, value any) (name string, resultErr error) {
	if err := privateTempDirectory(s.Dir); err != nil {
		return "", err
	}
	data, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return "", err
	}
	f, err := os.CreateTemp(s.Dir, prefix+"-"+time.Now().Format("20060102T150405.000000000")+"-*.json")
	if err != nil {
		return "", err
	}
	defer func() { resultErr = errors.Join(resultErr, f.Close()) }()
	if _, err := f.Write(append(data, '\n')); err != nil {
		return "", err
	}
	if err := f.Sync(); err != nil {
		return "", err
	}
	return filepath.Base(f.Name()), nil
}

type rebaselineReport struct {
	Started                time.Time              `json:"started"`
	Finished               time.Time              `json:"finished"`
	Label                  string                 `json:"label"`
	PreviousFile           string                 `json:"previousBaselineFile"`
	PreviousSHA256         string                 `json:"previousBaselineSHA256"`
	Before                 snapshot               `json:"before"`
	After                  snapshot               `json:"after"`
	Preflight              []preflight            `json:"preflight"`
	Error                  string                 `json:"error,omitempty"`
	AcceptedExternalChange bool                   `json:"acceptedExternalChange,omitempty"`
	Cleanup                *cleanupProof          `json:"cleanupProof,omitempty"`
	External               *externalChange        `json:"externalChange,omitempty"`
	SkippedDNS             []capture.DNSExclusion `json:"skippedDNS,omitempty"`
}

func rebaseline(ctx context.Context, s *session, c api.Service, label string, acceptExternal bool) error {
	return rebaselineWithOps(ctx, s, c, label, liveOps(), acceptExternal)
}

func rebaselineWithOps(parent context.Context, s *session, c api.Service, label string, ops rootcheckOps, acceptExternal ...bool) (resultErr error) {
	if len(label) == 0 || len(label) > 40 || label[0] < 'a' || label[0] > 'z' || strings.IndexFunc(label, func(r rune) bool {
		return !(r >= 'a' && r <= 'z' || r >= '0' && r <= '9' || r == '-' || r == '_')
	}) != -1 {
		return errors.New("label must start with a lowercase letter and contain 1-40 lowercase letters, digits, hyphens, or underscores")
	}
	if !s.Installed || !s.cancellationRecorded() || s.Installation == nil {
		return errors.New("rebaseline requires recorded cancellation or a managed skip, and the recorded installation")
	}
	ctx, cancel := context.WithTimeout(parent, 40*time.Second)
	defer cancel()
	if err := ops.verifyInstalled(s); err != nil {
		return err
	}
	st, err := c.Status(ctx)
	if err != nil {
		return err
	}
	if err := idleReviewedStatus(s, st); err != nil {
		return err
	}
	if err := s.requireCleanup(acceptExternal...); err != nil {
		return err
	}
	report := rebaselineReport{Started: time.Now(), Label: label, PreviousFile: s.BaselineFile, PreviousSHA256: s.baselineDigest()}
	report.Cleanup = s.Cleanup
	report.AcceptedExternalChange = s.Cleanup.Kind == externalProof
	defer func() {
		if resultErr != nil {
			report.Finished, report.Error = time.Now(), resultErr.Error()
			_, err := ops.archive(s, "rebaseline-"+label+"-failed", report)
			resultErr = errors.Join(resultErr, err)
		}
	}()
	report.Before, err = ops.snapshot(ctx)
	if err != nil {
		return err
	}
	if report.AcceptedExternalChange {
		report.External, err = s.externalDifferences(report.Before, st)
		if err != nil {
			return fmt.Errorf("current snapshot cannot accept external changes: %w", err)
		}
	}
	var resolved [2]api.InterfaceRef
	for i, ref := range s.Interfaces {
		iface := ref.Resolve(report.Before.Interfaces)
		if iface == nil || !iface.Up {
			return errors.New("a reviewed interface is missing or down; rebaseline cannot replace the reviewed adapters")
		}
		if s.ipv6() && !s.usableInterface(iface) {
			return errors.New("a reviewed interface has no global IPv6 address; rebaseline cannot substitute IPv4")
		}
		resolved[i] = api.InterfaceRef{Name: iface.Name, HardwareAddr: iface.HardwareAddr, Label: iface.Label}
	}
	if resolved[0].Name == resolved[1].Name {
		return errors.New("reviewed interfaces no longer resolve to two distinct adapters")
	}
	plan, err := s.snapshotPlan(report.Before)
	if err != nil {
		return fmt.Errorf("new baseline conflicts with the frozen capture: %w", err)
	}
	report.SkippedDNS = plan.SkippedDNS
	target, _ := netip.ParseAddrPort(s.Target)
	if capture.SystemInterface(report.Before.Routes, target.Addr()) == "" {
		return errors.New("the frozen test destination has no effective system route")
	}
	for _, iface := range []string{"", resolved[0].Name, resolved[1].Name} {
		probeCtx, probeCancel := context.WithTimeout(ctx, 8*time.Second)
		evidence, err := ops.probe(probeCtx, s, iface)
		probeCancel()
		if err != nil {
			return fmt.Errorf("rebaseline TLS preflight through %q: %w", iface, err)
		}
		report.Preflight = append(report.Preflight, preflight{Interface: iface, TLS: evidence})
	}
	report.After, err = ops.snapshot(ctx)
	if err != nil {
		return err
	}
	if !stableSnapshots(report.Before, report.After) {
		return errors.New("routes or interfaces changed during rebaseline preflight; previous baseline remains active")
	}
	plan, err = s.snapshotPlan(report.After)
	if err != nil {
		return err
	}
	report.SkippedDNS = plan.SkippedDNS
	if err := ops.verifyInstalled(s); err != nil {
		return err
	}
	st, err = c.Status(ctx)
	if err != nil {
		return err
	}
	if err := idleReviewedStatus(s, st); err != nil {
		return err
	}
	if report.AcceptedExternalChange {
		// stableSnapshots already checked semantic equality. Retain the final
		// full observations without treating IPv6 address-only churn as a new
		// topology change merely because the diff contains unnormalized values.
		report.External, err = s.externalDifferences(report.After, st)
		if err != nil {
			return err
		}
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	report.Finished = time.Now()
	name, err := ops.archive(s, "rebaseline-"+label, report)
	if err != nil {
		return err
	}
	// Commit one copy only after every read and archive succeeded. On any failure,
	// both the in-memory session and its atomic on-disk version keep the old phase.
	next := *s
	next.Original, next.Preflight, next.Interfaces = report.After, report.Preflight, resolved
	next.SkippedDNS = report.SkippedDNS
	next.BaselineLabel, next.BaselineFile = label, name
	next.SmokePassed, next.Cleanup = false, nil
	if err := ops.save(&next); err != nil {
		return err
	}
	*s = next
	fmt.Println("PASS: adopted stable baseline", label, "without authorization or routing changes.")
	fmt.Println("Private baseline candidate:", filepath.Join(s.Dir, name))
	fmt.Println("Run the default smoke check again before any additional case. Older baselines remain archived.")
	return nil
}

// sameInterfaces compares projected interface metadata. Interface has
// no IPv6 prefix, lifetime, or temporary-address metadata, so it cannot classify
// an address as stable or temporary. Preserve IPv6 availability and Router6,
// but ignore individual IPv6 address values and counts. sameRoutes separately
// remains strict, including any non-cache IPv6 host or prefix route changes.
// Complete prefix inventories are also checked by sameAddressNetworks.
func sameInterfaces(a, b []api.Interface) bool {
	keys := func(ifaces []api.Interface) []string {
		result := make([]string, 0, len(ifaces))
		for _, iface := range ifaces {
			hasIPv6 := len(iface.IPv6) > 0
			iface.IPv6 = nil
			iface.IPv4 = slices.Clone(iface.IPv4)
			slices.Sort(iface.IPv4)
			// DNS ordering can express resolver priority, so retain it exactly.
			data, _ := json.Marshal(struct {
				Interface api.Interface
				HasIPv6   bool
			}{iface, hasIPv6})
			result = append(result, string(data))
		}
		slices.Sort(result)
		return result
	}
	return slices.Equal(keys(a), keys(b))
}
