package client

import (
	"context"
	"errors"
	"reflect"
	"time"

	"github.com/Kel077/pith/internal/api"
)

type helperEvents struct {
	events <-chan api.Event
	cancel context.CancelFunc
	status api.Status
	links  []api.Interface
	err    error
}

// The helper is a background daemon: on a busy Mac, macOS gives it little
// time, and it may answer late. Only a helper that doesn't answer within
// these is reported as not responding.
const (
	streamSetup  = 5 * time.Second // to open a stream and read its two snapshots
	statusAnswer = 5 * time.Second // to answer its status, when no stream opened
)

// openHelperEvents bounds setup without attaching a short deadline to a healthy
// long-lived stream. Every stream still inherits the App/subscriber lifetime.
func (c *Client) openHelperEvents(parent context.Context) helperEvents {
	ctx, cancel := context.WithCancel(parent)
	timer := time.AfterFunc(streamSetup, cancel)
	defer timer.Stop()
	result := helperEvents{cancel: cancel}
	result.events, result.err = c.subscribeHelper(ctx)
	if result.err == nil {
		for i := range 2 {
			select {
			case event, ok := <-result.events:
				if !ok || i == 0 && event.Status == nil || i == 1 && event.Interfaces == nil {
					result.err = errors.New("helper event stream omitted its initial snapshots")
				} else if i == 0 {
					result.status = c.authenticatedStatus(*event.Status)
				} else {
					result.links = event.Interfaces
				}
			case <-ctx.Done():
				result.err = ctx.Err()
			}
			if result.err != nil {
				break
			}
		}
	}
	if result.err == nil && ctx.Err() == nil && timer.Stop() {
		return result
	}
	if result.err == nil {
		result.err = ctx.Err()
		if result.err == nil {
			result.err = context.DeadlineExceeded
		}
	}
	cancel()
	result.events = nil
	result.status = c.unavailableStatus(parent, result.err)
	return result
}

func (c *Client) authenticatedStatus(status api.Status) api.Status {
	status.Helper = api.HelperStatus{Installed: true, Running: true, Version: c.expectedHelper.Version}
	status.Proxies, status.Devices = nil, nil
	return status
}

func (c *Client) unavailableStatus(parent context.Context, failure error) api.Status {
	if status, ok := c.helperProblem(failure); ok {
		return status
	}
	ctx, cancel := context.WithTimeout(parent, statusAnswer)
	defer cancel()
	status, err := c.helperStatus(ctx)
	if err == nil {
		return status
	}
	code := api.ErrInternal
	if status.Helper.Installed {
		code = api.ErrHelperNotResponding
	}
	return api.Status{State: api.Failed, Helper: api.HelperStatus{Installed: status.Helper.Installed}, Error: &api.Error{Code: code, Detail: "helper status unavailable; routing state is unknown"}}
}

// Subscribe remains useful with no helper: local listener state belongs to the
// App. IPC attempts are authenticated separately and never gate proxy forwarding.
func (c *Client) Subscribe(parent context.Context) (<-chan api.Event, error) {
	c.proxyMu.Lock()
	if c.closed {
		c.proxyMu.Unlock()
		return nil, errClientClosed
	}
	ctx, cancel := context.WithCancel(parent)
	stopClose := context.AfterFunc(c.lifetime, cancel)
	c.subscriptions.Add(1)
	c.proxyMu.Unlock()
	finish := func() { cancel(); stopClose(); c.subscriptions.Done() }
	stream := c.openHelperEvents(ctx)
	if err := ctx.Err(); err != nil {
		stream.cancel()
		finish()
		return nil, err
	}
	if stream.events == nil {
		linksCtx, stop := context.WithTimeout(ctx, time.Second)
		stream.links, _ = c.Interfaces(linksCtx)
		stop()
		if stream.links == nil {
			stream.links = []api.Interface{}
		}
	}
	status := c.withProxyStatus(ctx, stream.status)
	if err := ctx.Err(); err != nil {
		stream.cancel()
		finish()
		return nil, err
	}
	output := make(chan api.Event, 32)
	if stream.links != nil {
		c.observeExits(stream.links)
	}
	output <- api.Event{Status: &status}
	output <- api.Event{Interfaces: stream.links}
	go func() {
		defer finish()
		defer close(output)
		defer func() { stream.cancel() }()
		c.events(ctx, output, stream, status)
	}()
	return output, nil
}

func (c *Client) events(ctx context.Context, output chan<- api.Event, stream helperEvents, latest api.Status) {
	defer func() { stream.cancel() }()
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	connected := stream.events
	results := make(chan helperEvents, 1)
	var attempting bool
	lastAttempt := time.Now()
	var lastLoss time.Time
	links := stream.links
	ticks := 0
	proxies, devices := latest.Proxies, latest.Devices
	send := func(event api.Event) bool {
		if ctx.Err() != nil {
			return false
		}
		select {
		case output <- event:
			return true
		case <-ctx.Done():
			return false
		}
	}
	publish := func(status api.Status) bool {
		status.Proxies, status.Devices = proxies, devices
		latest = status
		return send(api.Event{Status: &status})
	}
	reconnect := func(now time.Time) {
		attempting, lastAttempt = true, now
		c.subscriptions.Add(1) // the parent subscription still holds its reference
		go func() {
			defer c.subscriptions.Done()
			result := c.openHelperEvents(ctx)
			select {
			case results <- result:
			case <-ctx.Done():
				result.cancel()
			}
		}()
	}
	for {
		select {
		case <-ctx.Done():
			return
		case event, ok := <-connected:
			if !ok {
				stream.cancel()
				connected = nil
				// A helper that still answers can end a stream, as when it drops a
				// reader that fell behind, or its interface watcher can't read the
				// network configuration. Reopen it at once; when streams keep
				// ending, at the next attempt, three seconds after the last. An
				// attempt that opens no stream asks the helper's status: only a
				// helper that doesn't answer is reported.
				now := time.Now()
				if now.Sub(lastLoss) >= 3*time.Second {
					reconnect(now)
				}
				lastLoss = now
				continue
			}
			if event.Status != nil {
				if !publish(c.authenticatedStatus(*event.Status)) {
					return
				}
			} else {
				if event.Interfaces != nil {
					links = event.Interfaces
					c.observeExits(links)
				}
				if !send(event) {
					return
				}
			}
		case result := <-results:
			attempting = false
			stream.cancel()
			stream, connected = result, result.events
			result.status.Proxies, result.status.Devices = proxies, devices
			if !reflect.DeepEqual(result.status, latest) || connected != nil {
				if !publish(result.status) {
					return
				}
			}
			if connected != nil && !reflect.DeepEqual(links, result.links) {
				links = result.links
				c.observeExits(links)
				if !send(api.Event{Interfaces: links}) {
					return
				}
			}
		case now := <-ticker.C:
			ticks++
			statusCtx, cancel := context.WithTimeout(ctx, time.Second)
			next, nextDevices := c.proxyStatus(statusCtx), c.deviceStatus()
			cancel()
			if !reflect.DeepEqual(proxies, next) || !reflect.DeepEqual(devices, nextDevices) {
				proxies, devices = next, nextDevices
				if !publish(latest) {
					return
				}
			}
			if connected == nil && !attempting && now.Sub(lastAttempt) >= 3*time.Second {
				reconnect(now)
			}
			if connected == nil && len(proxies) > 0 && ticks%5 == 0 {
				linksCtx, cancel := context.WithTimeout(ctx, time.Second)
				next, err := c.Interfaces(linksCtx)
				cancel()
				if err == nil && !reflect.DeepEqual(links, next) {
					if next == nil {
						next = []api.Interface{}
					}
					links = next
					if !send(api.Event{Interfaces: links}) {
						return
					}
				}
			}
		}
	}
}
