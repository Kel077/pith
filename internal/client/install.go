package client

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"time"

	"github.com/Kel077/pith/internal/api"
	"github.com/Kel077/pith/internal/helper"
)

func (c *Client) InstallHelper(ctx context.Context) error {
	c.installMu.Lock()
	defer c.installMu.Unlock()
	if err := ctx.Err(); err != nil {
		return err
	}
	source, err := c.checkBundledHelper()
	if err != nil {
		return err
	}
	status, statusErr := c.Status(ctx)
	if statusErr != nil {
		return statusErr
	}
	if status.Helper.Running && !status.Helper.Outdated {
		return nil
	}
	if status.Helper.Installed && !status.Helper.Outdated && status.Error == nil {
		return &api.Error{Code: api.ErrInternal, Detail: "expected helper is not responding; retry without reinstalling"}
	}
	plan, err := helper.PlanVerifiedInstall(source, uint32(os.Getuid()), c.expectedHelper)
	if err != nil {
		return err
	}
	plan.Prompt = c.authorizationPrompt(status.Helper.Installed)
	if err := c.executePlan(ctx, plan); err != nil {
		return err
	}
	c.artifacts.invalidate()
	// launchctl bootstrap returns before the new process has opened its socket.
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	for attempt := 0; attempt < 20; attempt++ {
		status, err := c.Status(ctx)
		if err == nil && status.Helper.Running && !status.Helper.Outdated {
			return nil
		}
		timer := time.NewTimer(250 * time.Millisecond)
		select {
		case <-ctx.Done():
			timer.Stop()
			return ctx.Err()
		case <-timer.C:
		}
	}
	return &api.Error{Code: api.ErrInternal, Detail: "helper installed but did not answer with the expected artifact and protocol"}
}

// MyGo flattens platform resources in a bundle but exposes the source resources
// directory under go run. Prefer the packaged path; fall back only if absent.
func helperSource(resources string) (string, error) {
	path := filepath.Join(resources, "bin", "pithd")
	if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
		return path, err
	}
	path = filepath.Join(resources, "darwin-arm64", "bin", "pithd")
	_, err := os.Stat(path)
	return path, err
}

func (c *Client) UninstallHelper(ctx context.Context) error {
	c.installMu.Lock()
	defer c.installMu.Unlock()
	if err := ctx.Err(); err != nil {
		return err
	}
	status, err := c.Status(ctx)
	if err == nil && status.Helper.Running && !status.Helper.Outdated {
		// Explicit removal still has verified staged recovery if ordinary Stop
		// fails. Cancellation, however, never proceeds to authorization.
		_ = c.Stop(ctx)
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	source, err := c.checkBundledHelper()
	if err != nil {
		// A damaged bundle may still be removed using the installed file, but
		// only when its complete bytes and root metadata match the compiled anchor.
		if c.identityErr != nil || c.checkFile(c.binaryPath, true) != nil {
			return untrustedHelper("no verified cleanup helper is available; reinstall Pith before removing the helper")
		}
		source = c.binaryPath
	}
	plan, err := helper.PlanVerifiedUninstall(source, c.expectedHelper)
	if err != nil {
		return err
	}
	if c.opts.Prompts.Uninstall != "" {
		plan.Prompt = c.opts.Prompts.Uninstall
	}
	err = c.executePlan(ctx, plan)
	c.artifacts.invalidate()
	return err
}

func (c *Client) authorizationPrompt(update bool) string {
	if update {
		if c.opts.Prompts.Update != "" {
			return c.opts.Prompts.Update
		}
		return helper.UpdatePrompt
	}
	if c.opts.Prompts.Install != "" {
		return c.opts.Prompts.Install
	}
	return helper.InstallPrompt
}
