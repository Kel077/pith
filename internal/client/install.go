package client

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/Kel077/pith/internal/helper"
)

func (c *Client) InstallHelper(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if status, err := c.Status(ctx); err == nil && status.Helper.Running && !status.Helper.Outdated {
		return nil
	}
	if c.opts.ResourcesDir == "" {
		return fmt.Errorf("application resources directory is not set")
	}
	plan, err := helper.PlanInstall(filepath.Join(c.opts.ResourcesDir, "bin", "pithd"), uint32(os.Getuid()))
	if err != nil {
		return err
	}
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

func (c *Client) UninstallHelper(ctx context.Context) error {
	status, err := c.Status(ctx)
	if err != nil {
		return err
	}
	if status.Helper.Running && !status.Helper.Outdated {
		if err := c.Stop(ctx); err != nil {
			return err
		}
	}
	return helper.PlanUninstall().Execute(ctx)
}
