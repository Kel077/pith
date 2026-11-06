package client

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/Kel077/pith/internal/api"
	"github.com/Kel077/pith/internal/helper"
)

func (c *Client) InstallHelper(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if _, err := c.checkBundledHelper(); err != nil {
		return err
	}
	status, statusErr := c.Status(ctx)
	if statusErr == nil && status.Helper.Running && !status.Helper.Outdated {
		return nil
	}
	if c.opts.ResourcesDir == "" {
		return fmt.Errorf("application resources directory is not set")
	}
	source, err := helperSource(c.opts.ResourcesDir)
	if err != nil {
		return err
	}
	plan, err := helper.PlanInstall(source, uint32(os.Getuid()))
	if err != nil {
		return err
	}
	plan.Prompt = c.authorizationPrompt(status.Helper.Installed)
	if err := plan.Execute(ctx); err != nil {
		return err
	}
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
	return fmt.Errorf("helper installed but did not answer with the expected version")
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
	status, err := c.Status(ctx)
	if err != nil {
		return err
	}
	// The installed executable cannot perform cleanup unless it was verified.
	if status.Error != nil && status.Error.Code == api.ErrHelperUntrusted {
		return status.Error
	}
	if status.Helper.Outdated {
		return &api.Error{Code: api.ErrHelperOutdated}
	}
	if status.Helper.Running && !status.Helper.Outdated {
		if err := c.Stop(ctx); err != nil {
			return err
		}
	}
	plan := helper.PlanUninstall()
	if c.opts.Prompts.Uninstall != "" {
		plan.Prompt = c.opts.Prompts.Uninstall
	}
	return plan.Execute(ctx)
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
