package main

import (
	"context"
	"errors"
	"time"

	"github.com/Kel077/pith/internal/api"
)

// crashCapture runs inside check's existing session lock and cleanup defer.
// It cannot race a different controller command or authorize an idle service.
func crashCapture(parent context.Context, s *session, c api.Service, a authorization, ops rootcheckOps) error {
	if !s.SmokePassed || !s.Installed || !s.cancellationRecorded() || !s.Ownership.Complete || s.Ownership.Pending {
		return errors.New("crash requires passed smoke and complete active capture ownership")
	}
	ctx, cancel := context.WithTimeout(parent, 2*time.Minute)
	defer cancel()
	if err := ops.verifyInstalled(s); err != nil {
		return err
	}
	st, err := c.Status(ctx)
	if err != nil {
		return err
	}
	if st.State != api.Running || !st.Helper.Running || !st.Helper.Installed || st.Helper.Outdated || st.Helper.Version != s.Version || !s.validCapture(st.LastCapture) || !s.knownCapture(st.LastCapture) || !st.LastCapture.EndedAt.IsZero() {
		return errors.New("crash target is not this session's active reviewed helper")
	}
	id := *st.LastCapture
	before, err := ops.snapshot(ctx)
	if err != nil {
		return err
	}
	if before.BootSession != id.BootSession || before.InterfaceIndices[id.Interface] != id.Index {
		return errors.New("crash capture identity differs from independent snapshot")
	}
	for _, prefix := range id.Prefixes {
		count := 0
		for _, route := range before.Routes {
			if route.Prefix.String() == prefix && !route.Scoped && !route.Local && !route.Reject && !route.Gateway.IsValid() && route.Interface == id.Interface && route.Index == id.Index {
				count++
			}
		}
		if count != 1 {
			return errors.New("crash requires every reviewed capture route to have verified ownership")
		}
	}
	if _, err := ops.archive(s, "before-crash", before); err != nil {
		return err
	}
	if err := s.authorize(ctx, "crash", crashPlan(), a, ops); err != nil {
		return err
	}
	verifyCtx, verifyCancel := context.WithTimeout(parent, 10*time.Second)
	defer verifyCancel()
	return waitUntil(verifyCtx, func() (bool, error) {
		if err := ops.verifyInstalled(s); err != nil {
			return false, err
		}
		after, err := ops.snapshot(verifyCtx)
		if err != nil {
			return false, err
		}
		if after.BootSession != id.BootSession {
			return false, errors.New("boot changed during crash check")
		}
		for name, index := range after.InterfaceIndices {
			if name == id.Interface || index == id.Index {
				return false, nil
			}
		}
		for _, route := range after.Routes {
			if route.Interface == id.Interface || route.Index == id.Index {
				return false, nil
			}
			for _, prefix := range id.Prefixes {
				if route.Prefix.String() == prefix {
					return false, nil
				}
			}
		}
		status, err := c.Status(verifyCtx)
		if err != nil {
			return false, nil
		}
		if err := idleReviewedStatus(s, status); err != nil {
			return false, nil
		}
		if status.LastCapture != nil {
			return false, nil
		}
		_, err = ops.archive(s, "after-crash", after)
		return err == nil, err
	})
}
