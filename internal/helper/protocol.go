// Package helper implements Pith's authenticated, versioned local IPC and
// prepares the privileged installation. Importing it has no system effects.
package helper

import (
	"context"
	"time"

	"github.com/Kel077/pith/internal/api"
)

const (
	ProtocolVersion = 1
	ProtocolHeader  = "X-Pith-Protocol"
	Label           = "io.github.kel077.pith.helper"
	BinaryPath      = "/Library/PrivilegedHelperTools/" + Label
	PlistPath       = "/Library/LaunchDaemons/" + Label + ".plist"
	DataDir         = "/Library/Application Support/Pith"
	SocketPath      = "/var/run/" + Label + ".sock"
	MaxMessage      = 4 << 20
)

type StatusResponse struct {
	Protocol int        `json:"protocol"`
	Status   api.Status `json:"status"`
}

type StartRequest struct {
	Scene    api.Scene    `json:"scene"`
	Settings api.Settings `json:"settings"`
}

func (r StartRequest) Validate() error {
	return (api.Config{Version: api.ConfigVersion, Scenes: []api.Scene{r.Scene}, Selected: r.Scene.ID, Settings: r.Settings}).Validate()
}

// YieldRequest asks the helper to keep an interface from being macOS's
// primary while the request lasts, as while the router behind it renews its
// address. The response streams one YieldEvent once another interface is
// primary; the interface gets its rank back when the client closes the
// response, or after MaxYield.
type YieldRequest struct {
	Interface string `json:"interface"`
}

// YieldEvent names the interface that is macOS's primary instead.
type YieldEvent struct {
	Primary string `json:"primary"`
}

// MaxYield bounds a yield whose client never ends it.
const MaxYield = 10 * time.Minute

// Backend is the helper's routing and data implementation. Engine lifecycle
// calls must be serialized by the backend, including subscriber snapshots.
type Backend interface {
	Status(context.Context) (api.Status, error)
	Start(context.Context, api.Scene, api.Settings) error
	Reload(context.Context, api.Scene, api.Settings) error
	Stop(context.Context) error
	Subscribe(context.Context) (<-chan api.Event, error)
	Connections(context.Context, api.ConnectionQuery) ([]api.Connection, error)
	ClearConnections(context.Context) error
	Traffic(context.Context, api.TrafficQuery) (api.Traffic, error)
}

// IdleBackend makes a helper-only build safe to install: it remains idle and
// explicitly refuses Start until a routing implementation is linked in.
type IdleBackend struct{}

func (IdleBackend) Status(context.Context) (api.Status, error) {
	return api.Status{State: api.Stopped}, nil
}
func (IdleBackend) Start(context.Context, api.Scene, api.Settings) error {
	return &api.Error{Code: api.ErrInternal, Detail: "routing engine is not available in this build"}
}
func (IdleBackend) Reload(context.Context, api.Scene, api.Settings) error {
	return &api.Error{Code: api.ErrInternal, Detail: "routing engine is not available in this build"}
}
func (IdleBackend) Stop(context.Context) error { return nil }
func (IdleBackend) Subscribe(ctx context.Context) (<-chan api.Event, error) {
	out := make(chan api.Event, 1)
	out <- api.Event{Status: &api.Status{State: api.Stopped}}
	go func() { <-ctx.Done(); close(out) }()
	return out, nil
}
func (IdleBackend) Connections(context.Context, api.ConnectionQuery) ([]api.Connection, error) {
	return []api.Connection{}, nil
}
func (IdleBackend) ClearConnections(context.Context) error { return nil }
func (IdleBackend) Traffic(context.Context, api.TrafficQuery) (api.Traffic, error) {
	return api.Traffic{Apps: []api.AppTraffic{}, Interfaces: []api.InterfaceTraffic{}}, nil
}
