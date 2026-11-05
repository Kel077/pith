package client

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"

	"github.com/Kel077/pith/internal/api"
)

const maxConfigSize = 4 << 20

func (c *Client) LoadConfig(ctx context.Context) (api.Config, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.loadConfig(ctx)
}

func (c *Client) loadConfig(ctx context.Context) (api.Config, error) {
	if err := ctx.Err(); err != nil {
		return api.Config{}, err
	}
	f, err := os.Open(c.configPath)
	if errors.Is(err, os.ErrNotExist) {
		return api.DefaultConfig(), nil
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
	return cfg, ctx.Err()
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
	dir, err := os.Open(c.opts.ConfigDir)
	if err != nil {
		return err
	}
	defer dir.Close()
	return dir.Sync()
}
