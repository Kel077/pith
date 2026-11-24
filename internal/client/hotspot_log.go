package client

import (
	"bufio"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"syscall"
	"time"

	"github.com/Kel077/pith/internal/api"
	"github.com/Kel077/pith/internal/hotspot"
)

const (
	hotspotLogPrefix   = "hotspot-renewals-"
	hotspotLogSuffix   = ".jsonl"
	maxHotspotLogBytes = 8 << 20
)

type hotspotRenewalLog struct {
	At                time.Time                `json:"at"`
	Interface         string                   `json:"interface"`
	Model             string                   `json:"model,omitempty"`
	OK                bool                     `json:"ok"`
	TotalMS           int64                    `json:"totalMs"`
	InitialMobileData string                   `json:"initialMobileData,omitempty"`
	FinalMobileData   string                   `json:"finalMobileData,omitempty"`
	BeforeIPv4        string                   `json:"beforeIPv4,omitempty"`
	BeforeIPv6        string                   `json:"beforeIPv6,omitempty"`
	AfterIPv4         string                   `json:"afterIPv4,omitempty"`
	AfterIPv6         string                   `json:"afterIPv6,omitempty"`
	Error             string                   `json:"error,omitempty"`
	Stages            []hotspotRenewalStageLog `json:"stages,omitempty"`
}

type hotspotRenewalStageLog struct {
	Stage      string    `json:"stage"`
	At         time.Time `json:"at"`
	DurationMS int64     `json:"durationMs"`
	MobileData string    `json:"mobileData,omitempty"`
	Connected  *bool     `json:"connected,omitempty"`
	Attempt    int       `json:"attempt,omitempty"`
	Skipped    bool      `json:"skipped,omitempty"`
	Detail     string    `json:"detail,omitempty"`
	Error      string    `json:"error,omitempty"`
}

func (c *Client) configuredLogDays() int {
	if days := c.logDays.Load(); days >= 0 {
		return int(days)
	}
	days := api.DefaultSettings().LogDays
	if c.configPath == "" {
		return days
	}
	f, err := os.Open(c.configPath)
	if err != nil {
		return days
	}
	defer f.Close()
	data, err := io.ReadAll(io.LimitReader(f, maxConfigSize+1))
	if err != nil || len(data) > maxConfigSize {
		return days
	}
	var cfg api.Config
	if json.Unmarshal(data, &cfg) == nil && cfg.Settings.LogDays >= 0 {
		days = cfg.Settings.LogDays
		c.logDays.Store(int32(days))
	}
	return days
}

func hotspotLogDate(name string) (time.Time, bool) {
	if !strings.HasPrefix(name, hotspotLogPrefix) || !strings.HasSuffix(name, hotspotLogSuffix) {
		return time.Time{}, false
	}
	raw := strings.TrimSuffix(strings.TrimPrefix(name, hotspotLogPrefix), hotspotLogSuffix)
	date, err := time.Parse("2006-01-02", raw)
	return date, err == nil
}

func (c *Client) pruneHotspotRenewalLogs(now time.Time, days int) {
	if c.opts.ConfigDir == "" {
		return
	}
	c.hotspotLogMu.Lock()
	defer c.hotspotLogMu.Unlock()
	c.pruneHotspotRenewalLogsLocked(now, days)
}

func (c *Client) pruneHotspotRenewalLogsLocked(now time.Time, days int) {
	entries, err := os.ReadDir(c.opts.ConfigDir)
	if err != nil {
		return
	}
	var names []string
	for _, e := range entries {
		if _, ok := hotspotLogDate(e.Name()); ok {
			names = append(names, e.Name())
		}
	}
	sort.Strings(names)
	cut := now.UTC().Truncate(24*time.Hour).AddDate(0, 0, -min(days, 36500)+1)
	for _, name := range names {
		date, _ := hotspotLogDate(name)
		if days <= 0 || date.Before(cut) {
			_ = os.Remove(filepath.Join(c.opts.ConfigDir, name))
		}
	}
}

func (c *Client) writeHotspotRenewalLog(entry hotspotRenewalLog) {
	if c.opts.ConfigDir == "" {
		return
	}
	now := entry.At
	if now.IsZero() {
		now = c.exitTime()
		entry.At = now
	}
	days := c.configuredLogDays()
	c.hotspotLogMu.Lock()
	defer c.hotspotLogMu.Unlock()
	if days <= 0 {
		c.pruneHotspotRenewalLogsLocked(now, 0)
		return
	}
	if err := os.MkdirAll(c.opts.ConfigDir, 0700); err != nil {
		return
	}
	c.pruneHotspotRenewalLogsLocked(now, days)

	name := hotspotLogPrefix + now.UTC().Format("2006-01-02") + hotspotLogSuffix
	path := filepath.Join(c.opts.ConfigDir, name)
	f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE|os.O_APPEND|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0600)
	if err != nil {
		return
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil || !info.Mode().IsRegular() || info.Size() >= maxHotspotLogBytes {
		return
	}
	if info.Size() > 0 {
		var last [1]byte
		if _, err := f.ReadAt(last[:], info.Size()-1); err == nil && last[0] != '\n' {
			_, _ = f.Write([]byte{'\n'})
		}
	}
	if entry.Error != "" {
		entry.Error = hotspot.SanitizeError(errors.New(entry.Error))
	}
	for i := range entry.Stages {
		if entry.Stages[i].Error != "" {
			entry.Stages[i].Error = hotspot.SanitizeError(errors.New(entry.Stages[i].Error))
		}
	}
	w := bufio.NewWriter(f)
	if err := json.NewEncoder(w).Encode(entry); err != nil {
		return
	}
	_ = w.Flush()
}
