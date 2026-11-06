package main

import (
	"context"
	"errors"
	"io"
	"net"
	"os"
	"syscall"
	"testing"
	"time"

	"github.com/Kel077/pith/internal/api"
)

type tcpReloadService struct {
	baselineService
	before, after []api.Connection
	reads, saves  int
	beforeSave    func()
	saveErr       error
}

func (c *tcpReloadService) Connections(context.Context, api.ConnectionQuery) ([]api.Connection, error) {
	c.reads++
	if c.saves == 0 {
		return c.before, nil
	}
	return c.after, nil
}

func (c *tcpReloadService) SaveConfig(context.Context, api.Config) error {
	if c.beforeSave != nil {
		c.beforeSave()
	}
	c.saves++
	return c.saveErr
}

type tcpResetConn struct {
	local    net.Addr
	readErr  error
	readByte bool
	onRead   func()
	closed   bool
}

func (c *tcpResetConn) Read(p []byte) (int, error) {
	if c.onRead != nil {
		c.onRead()
	}
	if c.readByte {
		p[0] = 1
		return 1, c.readErr
	}
	return 0, c.readErr
}
func (c *tcpResetConn) Write([]byte) (int, error) {
	return 0, errors.New("unexpected application write")
}
func (c *tcpResetConn) Close() error                     { c.closed = true; return nil }
func (c *tcpResetConn) LocalAddr() net.Addr              { return c.local }
func (c *tcpResetConn) RemoteAddr() net.Addr             { return &net.TCPAddr{} }
func (c *tcpResetConn) SetDeadline(time.Time) error      { return nil }
func (c *tcpResetConn) SetReadDeadline(time.Time) error  { return nil }
func (c *tcpResetConn) SetWriteDeadline(time.Time) error { return nil }

func tcpReloadFixture(t *testing.T) (*session, *tcpReloadService, *tcpResetConn, api.Config) {
	t.Helper()
	s, baseline, _ := baselineFixture(t)
	path, err := executablePath()
	if err != nil {
		t.Fatal(err)
	}
	baseline.status.State, baseline.status.Since = api.Running, time.Now()
	baseline.status.LastCapture = &api.CaptureIdentity{Token: "reload-capture", Interface: "utun90", Index: 90, Prefixes: []string{"192.0.2.20/32"}}
	row := api.Connection{ID: 1, Started: time.Now(), Process: api.Process{PID: os.Getpid(), Path: path}, Protocol: api.TCP,
		Local: "192.0.2.10:51000", Remote: s.Target, OutboundLocal: "198.51.100.10:50001", Outcome: api.Routed, Interface: s.Interfaces[0].Name, RuleID: "route-a", Sent: 150, Received: 250}
	decoy := row
	decoy.ID, decoy.Local = 99, "192.0.2.10:51001"
	ended := row
	ended.Ended, ended.EndReason = time.Now(), api.EndRulesChanged
	// Delayed TCP relay accounting may include the last handshake bytes.
	ended.Sent, ended.Received = ended.Sent+1, ended.Received+1
	c := &tcpReloadService{baselineService: *baseline, before: []api.Connection{decoy, row}, after: []api.Connection{decoy, ended}}
	c.beforeSave = func() {
		if c.reads == 0 {
			t.Fatal("SaveConfig ran before identifying the original connection")
		}
	}
	conn := &tcpResetConn{local: &net.TCPAddr{IP: net.ParseIP("192.0.2.10"), Port: 51000}, readErr: &net.OpError{Op: "read", Net: "tcp", Err: syscall.ECONNRESET}}
	conn.onRead = func() {
		if c.saves != 1 {
			t.Fatal("RST was checked outside the single reviewed reload")
		}
	}
	cfg := api.Config{Version: api.ConfigVersion, Selected: "test", Scenes: []api.Scene{{ID: "test", Name: "Reload acceptance"}}, Settings: api.DefaultSettings()}
	return s, c, conn, cfg
}

func TestTCPReloadFreezesExactConnectionBeforeSaveAndRequiresReset(t *testing.T) {
	for _, variant := range []string{"reset", "eof", "timeout", "payload", "save-error", "missing-reason", "wrong-reason", "problem", "still-open", "missing-record", "changed-route", "changed-outbound", "missing-bytes"} {
		t.Run(variant, func(t *testing.T) {
			s, c, conn, cfg := tcpReloadFixture(t)
			defer conn.Close()
			switch variant {
			case "eof":
				conn.readErr = io.EOF
			case "timeout":
				conn.readErr = os.ErrDeadlineExceeded
			case "payload":
				conn.readByte = true
			case "save-error":
				c.saveErr = errors.New("reload failed")
			case "missing-reason":
				c.after[1].EndReason = ""
			case "wrong-reason":
				c.after[1].EndReason = "other"
			case "problem":
				c.after[1].Problem = "upstream reset"
			case "still-open":
				c.after[1].Ended = time.Time{}
			case "missing-record":
				c.after = c.after[:1]
			case "changed-route":
				c.after[1].Interface = "example1"
			case "changed-outbound":
				c.after[1].OutboundLocal = "198.51.100.10:50002"
			case "missing-bytes":
				c.after[1].Sent = 0
			}
			ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
			defer cancel()
			var report checkReport
			err := runTCPReload(ctx, s, c, cfg, c.status, conn, "example0", "route-a", &report)
			if (err == nil) != (variant == "reset") || c.saves != 1 || len(report.Cases) != 1 || report.Cases[0].Connection.ID != 1 {
				t.Fatalf("reload result: saves=%d report=%+v err=%v", c.saves, report.Cases, err)
			}
			if variant == "reset" && (report.Cases[0].Connection.EndReason != api.EndRulesChanged || report.Cases[0].Connection.Problem != "" || report.Cases[0].Connection.Ended.IsZero() || report.Cases[0].EvidenceNote == "") {
				t.Fatal("successful reload did not retain reset and original closure evidence")
			}
		})
	}
}

func TestTCPReloadRejectsUnattributedOrClosedConnectionBeforeSave(t *testing.T) {
	for _, variant := range []string{"local", "path", "closed", "end-reason", "interface", "rule", "outbound", "bytes", "duplicate"} {
		t.Run(variant, func(t *testing.T) {
			s, c, conn, cfg := tcpReloadFixture(t)
			defer conn.Close()
			row := &c.before[1]
			switch variant {
			case "local":
				row.Local = "192.0.2.10:51002"
			case "path":
				row.Process.Path = "/example/unrelated"
			case "closed":
				row.Ended = time.Now()
			case "end-reason":
				row.EndReason = api.EndRulesChanged
			case "interface":
				row.Interface = "example1"
			case "rule":
				row.RuleID = "route-b"
			case "outbound":
				row.OutboundLocal = ""
			case "bytes":
				row.Received = 0
			case "duplicate":
				duplicate := *row
				duplicate.ID = 2
				c.before = append(c.before, duplicate)
			}
			ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
			defer cancel()
			var report checkReport
			if err := runTCPReload(ctx, s, c, cfg, c.status, conn, "example0", "route-a", &report); err == nil || c.saves != 0 {
				t.Fatalf("unverified original connection authorized reload: saves=%d err=%v", c.saves, err)
			}
		})
	}
}
