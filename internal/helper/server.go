package helper

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"strconv"
	"syscall"
	"time"

	"github.com/Kel077/pith/internal/api"
	"github.com/Kel077/pith/internal/netif"
)

type ServerOptions struct {
	SocketPath string
	Version    string
	OwnerUID   uint32
	Backend    Backend
	// WatchInterfaces may be injected for deterministic tests.
	WatchInterfaces func(context.Context) (<-chan []api.Interface, error)
}

type Server struct{ opts ServerOptions }

func NewServer(opts ServerOptions) *Server {
	if opts.SocketPath == "" {
		opts.SocketPath = SocketPath
	}
	if opts.Version == "" {
		opts.Version = "dev"
	}
	if opts.Backend == nil {
		opts.Backend = IdleBackend{}
	}
	if opts.WatchInterfaces == nil {
		opts.WatchInterfaces = netif.Watch
	}
	return &Server{opts: opts}
}

type authenticatedListener struct {
	*net.UnixListener
	owner uint32
}

func (l *authenticatedListener) Accept() (net.Conn, error) {
	for {
		conn, err := l.AcceptUnix()
		if err != nil {
			return nil, err
		}
		uid, err := PeerUID(conn)
		if err == nil && (uid == 0 || uid == l.owner) {
			return conn, nil
		}
		conn.Close()
	}
}

// Run serves until ctx ends, stops routing, closes every stream and socket,
// and removes the socket it created. It never installs a daemon or starts
// routing on its own. A pre-existing live socket or non-socket file is an error.
func (s *Server) Run(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if info, err := os.Lstat(s.opts.SocketPath); err == nil {
		if info.Mode()&os.ModeSocket == 0 {
			return errors.New("helper socket path is not a socket")
		}
		conn, dialErr := net.DialTimeout("unix", s.opts.SocketPath, 250*time.Millisecond)
		if dialErr == nil {
			conn.Close()
			return errors.New("helper is already listening")
		}
		if !errors.Is(dialErr, syscall.ECONNREFUSED) && !errors.Is(dialErr, os.ErrNotExist) {
			return fmt.Errorf("check existing helper socket: %w", dialErr)
		}
		if err := os.Remove(s.opts.SocketPath); err != nil {
			return err
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	listener, err := net.ListenUnix("unix", &net.UnixAddr{Name: s.opts.SocketPath, Net: "unix"})
	if err != nil {
		return err
	}
	listener.SetUnlinkOnClose(false)
	created, err := os.Lstat(s.opts.SocketPath)
	if err != nil {
		listener.Close()
		return err
	}
	defer func() {
		listener.Close()
		if current, err := os.Lstat(s.opts.SocketPath); err == nil && os.SameFile(created, current) {
			_ = os.Remove(s.opts.SocketPath)
		}
	}()
	if err := os.Chmod(s.opts.SocketPath, 0600); err != nil {
		return err
	}
	if os.Geteuid() == 0 {
		if err := os.Chown(s.opts.SocketPath, int(s.opts.OwnerUID), 0); err != nil {
			return err
		}
	}
	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	server := &http.Server{Handler: s.handler(), ReadHeaderTimeout: 5 * time.Second, ReadTimeout: 15 * time.Second, IdleTimeout: 30 * time.Second, MaxHeaderBytes: 8192, BaseContext: func(net.Listener) context.Context { return runCtx }}
	finished := make(chan error, 1)
	go func() {
		finished <- server.Serve(&authenticatedListener{UnixListener: listener, owner: s.opts.OwnerUID})
	}()
	var serveErr error
	select {
	case <-ctx.Done():
	case serveErr = <-finished:
		finished = nil
	}
	cancel()
	stopCtx, stopCancel := context.WithTimeout(context.Background(), 5*time.Second)
	stopErr := s.opts.Backend.Stop(stopCtx)
	stopCancel()
	_ = server.Close()
	if finished != nil {
		serveErr = <-finished
	}
	if errors.Is(serveErr, http.ErrServerClosed) {
		serveErr = nil
	}
	return errors.Join(serveErr, stopErr)
}

func (s *Server) decorate(status api.Status) api.Status {
	status.Helper = api.HelperStatus{Installed: true, Running: true, Version: s.opts.Version}
	return status
}

func (s *Server) handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /v1/status", func(w http.ResponseWriter, r *http.Request) {
		status, err := s.opts.Backend.Status(r.Context())
		if err != nil {
			replyError(w, err)
			return
		}
		reply(w, StatusResponse{Protocol: ProtocolVersion, Status: s.decorate(status)})
	})
	for _, action := range []string{"start", "reload"} {
		mux.HandleFunc("POST /v1/"+action, func(w http.ResponseWriter, r *http.Request) {
			var in StartRequest
			if !decode(w, r, &in) {
				return
			}
			if err := in.Validate(); err != nil {
				replyError(w, err)
				return
			}
			var err error
			if action == "start" {
				err = s.opts.Backend.Start(r.Context(), in.Scene, in.Settings)
			} else {
				err = s.opts.Backend.Reload(r.Context(), in.Scene, in.Settings)
			}
			if err != nil {
				replyError(w, err)
				return
			}
			reply(w, struct{}{})
		})
	}
	mux.HandleFunc("POST /v1/stop", func(w http.ResponseWriter, r *http.Request) {
		if err := s.opts.Backend.Stop(r.Context()); err != nil {
			replyError(w, err)
			return
		}
		reply(w, struct{}{})
	})
	mux.HandleFunc("POST /v1/connections", func(w http.ResponseWriter, r *http.Request) {
		var q api.ConnectionQuery
		if !decode(w, r, &q) {
			return
		}
		if q.Limit < 0 {
			replyError(w, fmt.Errorf("negative connection limit"))
			return
		}
		if q.Limit == 0 {
			q.Limit = 500
		}
		if q.Limit > 10000 {
			q.Limit = 10000
		}
		connections, err := s.opts.Backend.Connections(r.Context(), q)
		if err != nil {
			replyError(w, err)
			return
		}
		reply(w, connections)
	})
	mux.HandleFunc("POST /v1/connections/clear", func(w http.ResponseWriter, r *http.Request) {
		if err := s.opts.Backend.ClearConnections(r.Context()); err != nil {
			replyError(w, err)
			return
		}
		reply(w, struct{}{})
	})
	mux.HandleFunc("POST /v1/traffic", func(w http.ResponseWriter, r *http.Request) {
		var q api.TrafficQuery
		if !decode(w, r, &q) {
			return
		}
		if q.Step < 0 {
			replyError(w, fmt.Errorf("negative traffic step"))
			return
		}
		traffic, err := s.opts.Backend.Traffic(r.Context(), q)
		if err != nil {
			replyError(w, err)
			return
		}
		reply(w, traffic)
	})
	mux.HandleFunc("GET /v1/events", s.events)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		if r.URL.Path != "/v1/status" && r.Header.Get(ProtocolHeader) != strconv.Itoa(ProtocolVersion) {
			replyError(w, &api.Error{Code: api.ErrHelperOutdated, Detail: "incompatible IPC protocol"})
			return
		}
		mux.ServeHTTP(w, r)
	})
}

func (s *Server) events(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithCancel(r.Context())
	defer cancel()
	// Backend subscription supplies its status snapshot before updates.
	events, err := s.opts.Backend.Subscribe(ctx)
	if err != nil {
		replyError(w, err)
		return
	}
	interfaces, err := s.opts.WatchInterfaces(ctx)
	if err != nil {
		replyError(w, err)
		return
	}
	var first api.Event
	select {
	case event, ok := <-events:
		if !ok || event.Status == nil {
			replyError(w, errors.New("backend did not supply its initial status"))
			return
		}
		first = event
	case <-ctx.Done():
		return
	}
	var links []api.Interface
	select {
	case value, ok := <-interfaces:
		if !ok {
			replyError(w, errors.New("interface watcher closed"))
			return
		}
		links = value
	case <-ctx.Done():
		return
	}
	w.Header().Set("Content-Type", "application/x-ndjson")
	controller := http.NewResponseController(w)
	write := func(event api.Event) error {
		if event.Status != nil {
			status := s.decorate(*event.Status)
			event.Status = &status
		}
		if err := controller.SetWriteDeadline(time.Now().Add(5 * time.Second)); err != nil {
			return err
		}
		var payload any = event
		// api.Event uses omitempty; preserve an empty interface snapshot as [].
		if event.Interfaces != nil {
			payload = struct {
				Interfaces []api.Interface `json:"interfaces"`
			}{event.Interfaces}
		}
		if err := json.NewEncoder(w).Encode(payload); err != nil {
			return err
		}
		return controller.Flush()
	}
	if err := write(first); err != nil {
		return
	}
	if links == nil {
		links = []api.Interface{}
	}
	if err := write(api.Event{Interfaces: links}); err != nil {
		return
	}
	for {
		select {
		case <-ctx.Done():
			return
		case event, ok := <-events:
			if !ok {
				return
			}
			if err := write(event); err != nil {
				return
			}
		case links, ok := <-interfaces:
			if !ok {
				return
			}
			if links == nil {
				links = []api.Interface{}
			}
			if err := write(api.Event{Interfaces: links}); err != nil {
				return
			}
		}
	}
}

func decode(w http.ResponseWriter, r *http.Request, value any) bool {
	r.Body = http.MaxBytesReader(w, r.Body, MaxMessage)
	d := json.NewDecoder(r.Body)
	if err := d.Decode(value); err != nil {
		replyError(w, &api.Error{Code: api.ErrInvalidConfig, Detail: "invalid IPC request: " + err.Error()})
		return false
	}
	if err := d.Decode(&struct{}{}); err != io.EOF {
		replyError(w, &api.Error{Code: api.ErrInvalidConfig, Detail: "extra IPC request data"})
		return false
	}
	return true
}

func reply(w http.ResponseWriter, value any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(value)
}
func replyError(w http.ResponseWriter, err error) {
	var apiErr *api.Error
	if !errors.As(err, &apiErr) {
		apiErr = &api.Error{Code: api.ErrInternal, Detail: err.Error()}
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusBadRequest)
	_ = json.NewEncoder(w).Encode(apiErr)
}
