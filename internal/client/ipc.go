package client

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strconv"
	"syscall"
	"time"

	"github.com/Kel077/pith/internal/api"
	"github.com/Kel077/pith/internal/helper"
)

func helperMissing() error { return &api.Error{Code: api.ErrHelperMissing} }

func (c *Client) initIPC() {
	if c.opts.SocketPath == "" {
		c.opts.SocketPath = helper.SocketPath
	}
	transport := &http.Transport{Proxy: nil, DisableKeepAlives: true, ResponseHeaderTimeout: 15 * time.Second, DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
		if err := c.checkHelperArtifacts(); err != nil {
			return nil, err
		}
		conn, err := (&net.Dialer{Timeout: 2 * time.Second}).DialContext(ctx, "unix", c.opts.SocketPath)
		if err != nil {
			return nil, err
		}
		unixConn, ok := conn.(*net.UnixConn)
		if !ok {
			conn.Close()
			return nil, untrustedHelper("helper did not provide a Unix socket")
		}
		if err := helper.VerifyPeer(unixConn, c.peerUID, c.expectedHelper.CDHash); err != nil {
			conn.Close()
			return nil, untrustedHelper("helper executing identity could not be verified")
		}
		return conn, nil
	}}
	c.http = &http.Client{Transport: transport, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
}

func (c *Client) open(ctx context.Context, method, path string, in any) (*http.Response, error) {
	var data []byte
	if in != nil {
		var err error
		data, err = json.Marshal(in)
		if err != nil {
			return nil, err
		}
	}
	req, err := http.NewRequestWithContext(ctx, method, "http://pith"+path, bytes.NewReader(data))
	if err != nil {
		return nil, err
	}
	req.Header.Set(helper.ProtocolHeader, strconv.Itoa(helper.ProtocolVersion))
	req.Header.Set("Content-Type", "application/json")
	return c.http.Do(req)
}

func responseError(res *http.Response) error {
	data, err := io.ReadAll(io.LimitReader(res.Body, 64<<10))
	if err != nil {
		return err
	}
	var apiErr api.Error
	if err := json.Unmarshal(data, &apiErr); err == nil && apiErr.Code != "" {
		return &apiErr
	}
	return fmt.Errorf("helper returned HTTP %d", res.StatusCode)
}

func (c *Client) request(ctx context.Context, method, path string, in, out any) error {
	ctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	res, err := c.open(ctx, method, path, in)
	if err != nil {
		return err
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		return responseError(res)
	}
	data, err := io.ReadAll(io.LimitReader(res.Body, helper.MaxMessage+1))
	if err != nil {
		return err
	}
	if len(data) > helper.MaxMessage {
		return errors.New("oversized helper response")
	}
	if out == nil {
		return nil
	}
	return json.Unmarshal(data, out)
}

func (c *Client) Status(ctx context.Context) (api.Status, error) {
	status := api.Status{State: api.Stopped}
	if err := ctx.Err(); err != nil {
		return status, err
	}
	if err := c.checkHelperArtifacts(); err != nil {
		if problem, ok := helperProblem(err); ok {
			return problem, nil
		}
		return status, err
	}
	status.Helper.Installed = true
	var response helper.StatusResponse
	err := c.request(ctx, http.MethodGet, "/v1/status", nil, &response)
	if errors.Is(err, syscall.ENOENT) || errors.Is(err, syscall.ECONNREFUSED) {
		return status, nil
	}
	if err != nil {
		if problem, ok := helperProblem(err); ok {
			return problem, nil
		}
		return status, err
	}
	if response.Protocol != c.expectedHelper.Protocol {
		problem, _ := helperProblem(&api.Error{Code: api.ErrHelperOutdated})
		return problem, nil
	}
	status = response.Status
	status.Helper = api.HelperStatus{Installed: true, Running: true, Version: c.expectedHelper.Version}
	return status, nil
}

func (c *Client) requireHelper(ctx context.Context) error {
	status, err := c.Status(ctx)
	if err != nil {
		return err
	}
	if status.Error != nil && status.Error.Code == api.ErrHelperUntrusted {
		return status.Error
	}
	if !status.Helper.Installed {
		return helperMissing()
	}
	if status.Helper.Outdated {
		return &api.Error{Code: api.ErrHelperOutdated}
	}
	if !status.Helper.Running {
		return &api.Error{Code: api.ErrInternal, Detail: "installed helper is not responding"}
	}
	return nil
}

func (c *Client) Start(ctx context.Context, sceneID string) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	cfg, err := c.loadConfig(ctx)
	if err != nil {
		return err
	}
	var scene *api.Scene
	for i := range cfg.Scenes {
		if cfg.Scenes[i].ID == sceneID {
			scene = &cfg.Scenes[i]
			break
		}
	}
	if scene == nil {
		return &api.Error{Code: api.ErrNoScene, Detail: sceneID}
	}
	if err := c.requireHelper(ctx); err != nil {
		return err
	}
	return c.request(ctx, http.MethodPost, "/v1/start", helper.StartRequest{Scene: *scene, Settings: cfg.Settings}, nil)
}

func (c *Client) Stop(ctx context.Context) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if err := c.requireHelper(ctx); err != nil {
		return err
	}
	return c.request(ctx, http.MethodPost, "/v1/stop", struct{}{}, nil)
}

func (c *Client) applyConfig(ctx context.Context, cfg api.Config) error {
	status, err := c.Status(ctx)
	if err != nil {
		return fmt.Errorf("configuration saved; helper status failed: %w", err)
	}
	if !status.Helper.Running || (status.State != api.Running && status.State != api.Starting) {
		return nil
	}
	if status.Helper.Outdated {
		return &api.Error{Code: api.ErrHelperOutdated, Detail: "configuration saved; update the helper to apply it"}
	}
	for _, scene := range cfg.Scenes {
		if scene.ID == status.SceneID {
			if err := c.request(ctx, http.MethodPost, "/v1/reload", helper.StartRequest{Scene: scene, Settings: cfg.Settings}, nil); err != nil {
				return fmt.Errorf("configuration saved; hot reload failed: %w", err)
			}
			return nil
		}
	}
	// The running scene was deleted. Stop rather than retain invisible rules.
	return c.request(ctx, http.MethodPost, "/v1/stop", struct{}{}, nil)
}

func (c *Client) Connections(ctx context.Context, q api.ConnectionQuery) ([]api.Connection, error) {
	if err := c.requireHelper(ctx); err != nil {
		return nil, err
	}
	var result []api.Connection
	err := c.request(ctx, http.MethodPost, "/v1/connections", q, &result)
	return result, err
}
func (c *Client) ClearConnections(ctx context.Context) error {
	if err := c.requireHelper(ctx); err != nil {
		return err
	}
	return c.request(ctx, http.MethodPost, "/v1/connections/clear", struct{}{}, nil)
}
func (c *Client) Traffic(ctx context.Context, q api.TrafficQuery) (api.Traffic, error) {
	if err := c.requireHelper(ctx); err != nil {
		return api.Traffic{}, err
	}
	var result api.Traffic
	err := c.request(ctx, http.MethodPost, "/v1/traffic", q, &result)
	return result, err
}

func (c *Client) Subscribe(ctx context.Context) (<-chan api.Event, error) {
	if err := c.requireHelper(ctx); err != nil {
		return nil, err
	}
	res, err := c.open(ctx, http.MethodGet, "/v1/events", nil)
	if err != nil {
		return nil, err
	}
	if res.StatusCode != http.StatusOK {
		defer res.Body.Close()
		return nil, responseError(res)
	}
	output := make(chan api.Event, 32)
	go func() {
		defer close(output)
		defer res.Body.Close()
		scanner := bufio.NewScanner(res.Body)
		scanner.Buffer(make([]byte, 4096), helper.MaxMessage)
		for scanner.Scan() {
			var event api.Event
			if err := json.Unmarshal(scanner.Bytes(), &event); err != nil {
				return
			}
			if event.Status == nil && event.Connection == nil && event.Interfaces == nil {
				return
			}
			select {
			case output <- event:
			case <-ctx.Done():
				return
			}
		}
	}()
	return output, nil
}
