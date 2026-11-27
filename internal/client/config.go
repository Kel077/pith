package client

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"os"
	"slices"

	"github.com/Kel077/pith/internal/api"
)

const maxConfigSize = 4 << 20

func (c *Client) LoadConfig(ctx context.Context) (api.Config, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	cfg, err := c.loadConfig(ctx)
	if err != nil {
		return cfg, err
	}
	c.recoverDrain(ctx, cfg)
	c.recoverRouter()
	return cfg, c.syncProxies(ctx, cfg, true)
}

func (c *Client) loadConfig(ctx context.Context) (api.Config, error) {
	if err := ctx.Err(); err != nil {
		return api.Config{}, err
	}
	f, err := os.Open(c.configPath)
	if errors.Is(err, os.ErrNotExist) {
		cfg := api.DefaultConfig()
		c.keepExitConfig(cfg)
		return cfg, nil
	}
	if err != nil {
		return api.Config{}, fmt.Errorf("open config: %w", err)
	}
	defer f.Close()
	data, err := io.ReadAll(io.LimitReader(f, maxConfigSize+1))
	if err != nil {
		return api.Config{}, err
	}
	if len(data) > maxConfigSize {
		return api.Config{}, &api.Error{Code: api.ErrInvalidConfig, Detail: "configuration exceeds 4 MiB"}
	}
	var cfg api.Config
	if err := json.Unmarshal(data, &cfg); err != nil {
		return api.Config{}, &api.Error{Code: api.ErrInvalidConfig, Detail: "decode config: " + err.Error()}
	}
	if err := cfg.Validate(); err != nil {
		return api.Config{}, err
	}
	if err := ctx.Err(); err != nil {
		return cfg, err
	}
	c.keepExitConfig(cfg)
	return cfg, nil
}

// keepExitConfig publishes app-owned settings for port and device operations.
func (c *Client) keepExitConfig(cfg api.Config) {
	aliases := maps.Clone(cfg.Aliases)
	c.exitAliases.Store(&aliases)
	c.logDays.Store(int32(cfg.Settings.LogDays))
	c.exitMu.Lock()
	defer c.exitMu.Unlock()
	if !slices.Equal(c.openWrt, cfg.OpenWrt) {
		c.openWrt = slices.Clone(cfg.OpenWrt)
		for _, state := range c.exits {
			state.capability = nil
		}
	}
}

func (c *Client) SaveConfig(ctx context.Context, cfg api.Config) error {
	if err := cfg.Validate(); err != nil {
		return err
	}
	data, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return err
	}
	data = append(data, '\n')
	if len(data) > maxConfigSize {
		return &api.Error{Code: api.ErrInvalidConfig, Detail: "configuration exceeds 4 MiB"}
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := c.validateProxyInterfaces(ctx, cfg.Proxies); err != nil {
		return err
	}
	previous, previousErr := c.loadConfig(ctx)
	if err := ctx.Err(); err != nil {
		return err
	}
	c.proxyMu.Lock()
	closed := c.closed
	c.proxyMu.Unlock()
	if closed {
		return errClientClosed
	}
	if err := os.MkdirAll(c.opts.ConfigDir, 0700); err != nil {
		return err
	}
	f, err := os.CreateTemp(c.opts.ConfigDir, ".config-*")
	if err != nil {
		return err
	}
	tmp := f.Name()
	defer os.Remove(tmp)
	defer f.Close()
	if _, err := f.Write(data); err != nil {
		return err
	}
	if err := f.Sync(); err != nil {
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := os.Rename(tmp, c.configPath); err != nil {
		return err
	}
	c.keepExitConfig(cfg)
	if cfg.Settings.LogDays == 0 {
		c.pruneHotspotRenewalLogs(c.exitTime(), 0)
	}
	routingUnchanged := previousErr == nil && sameRoutingConfig(previous, cfg)
	localOnly := routingUnchanged && (!sameProxies(previous, cfg) || !slices.Equal(previous.OpenWrt, cfg.OpenWrt))
	c.routingPending = c.routingPending || !routingUnchanged
	if err := c.syncProxies(ctx, cfg, false); err != nil {
		return fmt.Errorf("configuration saved; local proxy update failed: %w", err)
	}
	dir, err := os.Open(c.opts.ConfigDir)
	if err != nil {
		return err
	}
	defer dir.Close()
	if err := dir.Sync(); err != nil {
		return err
	}
	if localOnly && !c.routingPending {
		return nil
	}
	// Disk equality is not proof that an earlier helper update succeeded.
	// An identical explicit Save still retries; a pending routing failure also
	// cannot be hidden by a later edit limited to app-owned settings.
	err = c.applyConfig(ctx, cfg)
	c.routingPending = err != nil
	return err
}
