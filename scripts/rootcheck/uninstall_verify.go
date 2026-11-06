package main

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/Kel077/pith/internal/helper"
)

type uninstallVerification struct {
	Started          time.Time       `json:"started"`
	Finished         time.Time       `json:"finished"`
	Baseline         string          `json:"baselineLabel"`
	BaselineSHA256   string          `json:"baselineSHA256"`
	Binding          string          `json:"sessionAndBaselineSHA256"`
	Ownership        captureHistory  `json:"captureOwnership"`
	Snapshots        []snapshot      `json:"snapshots"`
	External         *externalChange `json:"external,omitempty"`
	ExplicitAccepted bool            `json:"explicitAccepted"`
	Restored         bool            `json:"restored"`
	Error            string          `json:"error,omitempty"`
}

func (s snapshot) requireUninstalledAbsent() error {
	if err := s.requireAbsent(); err != nil {
		return err
	}
	if s.LaunchExit <= 0 || !strings.Contains(s.Launchd, fmt.Sprintf("Could not find service %q in domain for system", helper.Label)) {
		return errors.New("launchd service absence was not positively identified")
	}
	return nil
}

// Stored EndedAt values were retained from authenticated idle status while the
// helper existed. Removal cannot invent a replacement status or refresh them.
func (s *session) uninstalledHistory(after snapshot) error {
	if err := s.externalSnapshotHistory(after); err != nil {
		return err
	}
	if s.Original.At.IsZero() || after.At.IsZero() || !s.Original.At.Before(after.At) || !s.Original.completeAddresses() || !after.completeAddresses() {
		return errors.New("post-removal acceptance requires complete timed baseline and current address inventories")
	}
	tokens := map[string]bool{}
	for _, id := range s.Ownership.Captures {
		if tokens[id.Token] || id.EndedAt.IsZero() || !id.EndedAt.Before(after.At) {
			return errors.New("every unique capture receipt requires an authenticated end before the post-removal snapshot")
		}
		tokens[id.Token] = true
		for _, inventory := range []snapshot{s.Original, after} {
			for name, index := range inventory.InterfaceIndices {
				if name == id.Interface || index == id.Index {
					return errors.New("post-removal acceptance cannot establish capture interface name or index reuse")
				}
			}
		}
	}
	if err := s.Original.noCaptureAddresses(s.Ownership.Captures); err != nil {
		return err
	}
	scopes, _ := s.scopes()
	for _, route := range after.Routes {
		if slices.Contains(scopes, route.Prefix) {
			return errors.New("an exact capture prefix remains after removal")
		}
	}
	return after.noCaptureAddresses(s.Ownership.Captures)
}

// This path has no client, authorization, installation or probe boundary. The
// only operations are independent snapshots and private evidence/state writes.
func verifyUninstalledReport(parent context.Context, s *session, ops rootcheckOps, acceptExternal bool) (record uninstallVerification, resultErr error) {
	if !s.AuthorizationAttempted {
		return record, errors.New("this session has never attempted installation")
	}
	ctx, cancel := context.WithTimeout(parent, 15*time.Second)
	defer cancel()
	record = uninstallVerification{Started: time.Now(), Baseline: s.BaselineLabel, BaselineSHA256: s.baselineDigest(), Binding: s.cleanupBinding(), Ownership: s.Ownership}
	defer func() {
		if resultErr != nil {
			record.Finished, record.Error = time.Now(), resultErr.Error()
			record.ExplicitAccepted, record.Restored = false, false
			_, archiveErr := ops.archive(s, "uninstall-verification-failed", record)
			resultErr = errors.Join(resultErr, archiveErr)
		}
	}()
	first, err := ops.snapshot(ctx)
	record.Snapshots = append(record.Snapshots, first)
	if err != nil {
		return record, err
	}
	if err := first.requireUninstalledAbsent(); err != nil {
		return record, err
	}
	restored := sameCleanupBaseline(s.Original, first)
	if !acceptExternal && !restored {
		return record, errors.New("routes, addresses or tunnels differ from the retained baseline; review the snapshots, then use verify-uninstalled --accept-external-change explicitly for external-only changes")
	}
	if acceptExternal {
		if err := s.uninstalledHistory(first); err != nil {
			return record, err
		}
		interval := time.NewTimer(100 * time.Millisecond)
		defer interval.Stop()
		select {
		case <-ctx.Done():
			return record, ctx.Err()
		case <-interval.C:
		}
		second, err := ops.snapshot(ctx)
		record.Snapshots = append(record.Snapshots, second)
		if err != nil {
			return record, err
		}
		if err := second.requireUninstalledAbsent(); err != nil {
			return record, err
		}
		if err := s.uninstalledHistory(second); err != nil {
			return record, err
		}
		if !second.At.After(first.At) || !stableSnapshots(first, second) || len(addressDifference(first.InterfaceAddresses, second.InterfaceAddresses)) != 0 || len(addressDifference(second.InterfaceAddresses, first.InterfaceAddresses)) != 0 {
			return record, errors.New("post-removal snapshots are not stable complete observations")
		}
		if !restored {
			diff, err := s.classifyExternalDifferences(second, nil, nil, nil)
			if err != nil {
				return record, fmt.Errorf("post-removal differences are not proven external-only: %w", err)
			}
			record.External, record.ExplicitAccepted = diff, true
		}
	}
	if err := ctx.Err(); err != nil {
		return record, err
	}
	record.Finished, record.Restored = time.Now(), restored
	// Evidence is archived before committing the new phase. A failed commit
	// retains the old flags and produces a separate failed-attempt record.
	if _, err := ops.archive(s, "uninstall-verification", record); err != nil {
		return record, err
	}
	next := *s
	next.Installed, next.SmokePassed, next.DNSPassed, next.QUICPassed, next.BundlePassed = false, false, false, false, false
	next.Cleanup = nil
	if err := ops.save(&next); err != nil {
		return record, err
	}
	*s = next
	return record, nil
}
