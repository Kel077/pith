package main

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/Kel077/pith/internal/api"
)

type hotspotObservation struct {
	Interface string             `json:"interface"`
	Started   time.Time          `json:"started"`
	Finished  time.Time          `json:"finished"`
	Renewal   api.HotspotRenewal `json:"renewal"`
	BoundEcho tlsEvidence        `json:"boundEchoAfterRenewal"`
	Statuses  []api.Status       `json:"statuses"`
	Error     string             `json:"error,omitempty"`
}

// The caller reaches this only after explicit --renew-hotspot, prior smoke and
// the current capture's verified system/A requests. Tests inject the Service and
// probe; neither the keychain nor a device is accessed by unit tests.
func observeHotspot(parent context.Context, s *session, c api.Service, initial api.Status, duration time.Duration, report *checkReport, ops rootcheckOps, verify func(context.Context, tlsEvidence) error) (resultErr error) {
	ctx, cancel := context.WithTimeout(parent, duration)
	defer cancel()
	record := &hotspotObservation{Interface: s.Interfaces[0].Name, Started: time.Now()}
	report.Hotspot = record
	defer func() {
		record.Finished = time.Now()
		if resultErr != nil {
			record.Error = resultErr.Error()
		}
	}()
	if !s.validCapture(initial.LastCapture) || initial.State != api.Running {
		return errors.New("hotspot observation requires the current reviewed capture identity")
	}
	check := func() error {
		st, err := c.Status(ctx)
		record.Statuses = append(record.Statuses, st)
		if err != nil || st.State != api.Running || !st.Since.Equal(initial.Since) || st.LastCapture == nil || !sameCapture(*st.LastCapture, *initial.LastCapture) || !st.LastCapture.EndedAt.IsZero() || !st.Helper.Running || st.Helper.Outdated {
			return fmt.Errorf("capture stopped, restarted, or changed during hotspot renewal: state=%s error=%v", st.State, err)
		}
		return ops.verifyInstalled(s)
	}
	if err := check(); err != nil {
		return err
	}
	hotspot, err := c.Hotspot(ctx, record.Interface)
	if err != nil {
		return err
	}
	if hotspot.Interface != record.Interface || hotspot.Model != "M2000" || hotspot.Locked || !hotspot.Connected {
		return errors.New("hotspot renewal requires an unlocked, connected M2000 on reviewed interface A")
	}
	if err := check(); err != nil {
		return err
	}
	// Persist intent before any device mutation. This is not administrator auth;
	// the client uses the app's existing hotspot credential only when opted in.
	if _, err := ops.archive(s, "hotspot-renewal-attempt", record); err != nil {
		return err
	}
	type result struct {
		renewal api.HotspotRenewal
		err     error
	}
	done := make(chan result, 1)
	joined := false
	go func() {
		r, err := c.RenewHotspot(ctx, record.Interface)
		done <- result{r, err}
	}()
	defer func() {
		cancel()
		if !joined {
			// RenewHotspot may need its independent bounded cellular-data restore.
			// Join it before returning to Stop; never leave device work behind.
			r := <-done
			record.Renewal = r.renewal
			resultErr = errors.Join(resultErr, r.err)
		}
	}()
	ticker := time.NewTicker(250 * time.Millisecond)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
			if err := check(); err != nil {
				return err
			}
		case r := <-done:
			joined, record.Renewal = true, r.renewal
			if r.err != nil {
				return r.err
			}
			if !r.renewal.After.OK || r.renewal.After.Interface != record.Interface {
				return errors.New("hotspot renewal has no successful interface-A connectivity probe")
			}
			if err := check(); err != nil {
				return err
			}
			record.BoundEcho, err = ops.probe(ctx, s, record.Interface)
			if err != nil {
				return err
			}
			entry := checkCase{TLS: record.BoundEcho}
			if record.BoundEcho.PublicIP == "" {
				return errors.New("hotspot observation requires a public-IP echo from the frozen endpoint")
			}
			if err := s.checkEcho(&entry, record.Interface); err != nil {
				return err
			}
			if err := verify(ctx, record.BoundEcho); err != nil {
				return err
			}
			return check()
		}
	}
}
