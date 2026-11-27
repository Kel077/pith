package router

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
	"unicode"

	"github.com/Kel077/pith/internal/api"
)

// OpenWrt runs the configured WAN service over a non-interactive SSH session.
// Run is optional and lets tests replace the SSH process.
type OpenWrt struct {
	Config    api.OpenWrtRouter
	Interface string
	Run       func(context.Context, ...string) ([]byte, error)
}

const (
	openWrtStatusTimeout = 5 * time.Second
	openWrtRotateTimeout = 90 * time.Second
	openWrtOutputLimit   = 16 << 10
)

type wanReply struct {
	OK          *bool   `json:"ok"`
	Error       string  `json:"error"`
	Router      string  `json:"router"`
	WANUp       *bool   `json:"wan_up"`
	IPv4        string  `json:"ipv4"`
	IPv6Prefix  string  `json:"ipv6_prefix"`
	OldMAC      string  `json:"old_mac"`
	NewMAC      string  `json:"new_mac"`
	MACChanged  bool    `json:"mac_changed"`
	OldIPv4     string  `json:"old_ipv4"`
	NewIPv4     string  `json:"new_ipv4"`
	OldPrefix   string  `json:"old_ipv6_prefix"`
	NewPrefix   string  `json:"new_ipv6_prefix"`
	WANState    string  `json:"mwan3_wan"`
	BackupState string  `json:"mwan3_usbwan"`
	Elapsed     float64 `json:"elapsed_sec"`
}

func (d OpenWrt) Status(ctx context.Context) (api.Router, error) {
	r := api.Router{Interface: d.Interface, At: time.Now()}
	reply, err := d.call(ctx, "--status", openWrtStatusTimeout)
	if err != nil {
		return r, err
	}
	r.Model = openWrtText(reply.Router)
	ipv4, ipv6Prefix := openWrtIP(reply.IPv4, true), openWrtIPv6Prefix(reply.IPv6Prefix)
	if r.Model == "" || reply.WANUp == nil || reply.IPv4 != "" && ipv4 == "" {
		return r, errors.New("OpenWrt: incomplete WAN status")
	}
	r.Connected, r.PublicIPv4 = *reply.WANUp && ipv4 != "", ipv4
	r.OpenWrt = &api.OpenWrtStatus{IPv6Prefix: ipv6Prefix, WANState: openWrtLinkState(reply.WANState), BackupState: openWrtLinkState(reply.BackupState)}
	return r, nil
}

func (d OpenWrt) Rotate(ctx context.Context) (*api.WANRotation, error) {
	reply, err := d.call(ctx, "--rotate", openWrtRotateTimeout)
	if err != nil {
		return nil, err
	}
	oldMAC, oldErr := net.ParseMAC(reply.OldMAC)
	newMAC, newErr := net.ParseMAC(reply.NewMAC)
	changed := oldErr == nil && newErr == nil && len(oldMAC) == 6 && len(newMAC) == 6 &&
		newMAC[0]&3 == 2 && !bytes.Equal(oldMAC, newMAC) && reply.MACChanged
	wan := &api.WANRotation{
		BeforeIPv4: openWrtIP(reply.OldIPv4, true), IPv4: openWrtIP(reply.NewIPv4, true),
		BeforeIPv6Prefix: openWrtIPv6Prefix(reply.OldPrefix), IPv6Prefix: openWrtIPv6Prefix(reply.NewPrefix),
		MACChanged: changed, WANState: openWrtLinkState(reply.WANState), BackupState: openWrtLinkState(reply.BackupState),
		Seconds: reply.Elapsed,
	}
	if !wan.MACChanged || wan.IPv4 == "" || wan.Seconds < 0 {
		return nil, errors.New("OpenWrt: rotation did not confirm a new WAN MAC and an IPv4 lease")
	}
	return wan, nil
}

func (d OpenWrt) call(parent context.Context, operation string, limit time.Duration) (wanReply, error) {
	var reply wanReply
	if err := d.Config.Validate(); err != nil {
		return reply, err
	}
	ctx, cancel := context.WithTimeout(parent, limit)
	defer cancel()
	if err := ctx.Err(); err != nil {
		return reply, err
	}
	args, err := d.args(operation)
	if err != nil {
		return reply, err
	}
	run := d.Run
	if run == nil {
		run = runSSH
	}
	data, runErr := run(ctx, args...)
	if err := ctx.Err(); err != nil {
		return reply, fmt.Errorf("OpenWrt SSH %s: %w", operation, err)
	}
	if len(data) <= openWrtOutputLimit && json.Unmarshal(data, &reply) == nil && reply.OK != nil {
		if !*reply.OK {
			if reply.Error == "another WAN IP rotation is already in progress" {
				return reply, &api.Error{Code: api.ErrBusy, Detail: "the OpenWrt router is already rotating its WAN"}
			}
			detail := openWrtText(reply.Error)
			if detail == "" {
				detail = "the command reported a failure"
			}
			return reply, &api.Error{Code: api.ErrInternal, Detail: "OpenWrt SSH " + operation + ": " + detail}
		}
		if runErr == nil {
			return reply, nil
		}
	}
	if runErr != nil {
		return reply, fmt.Errorf("OpenWrt SSH %s failed; check the host, known_hosts and SSH key", operation)
	}
	return reply, fmt.Errorf("OpenWrt SSH %s returned an invalid JSON reply", operation)
}

func (d OpenWrt) args(operation string) ([]string, error) {
	args := []string{"-nT",
		"-o", "BatchMode=yes", "-o", "StrictHostKeyChecking=yes", "-o", "UpdateHostKeys=no", "-o", "CheckHostIP=no",
		"-o", "ConnectTimeout=4", "-o", "ConnectionAttempts=1", "-o", "ServerAliveInterval=5", "-o", "ServerAliveCountMax=2",
		"-o", "ClearAllForwardings=yes", "-o", "ForwardAgent=no", "-o", "ForwardX11=no", "-o", "PermitLocalCommand=no",
		"-o", "ControlPath=none", "-o", "ForkAfterAuthentication=no", "-o", "ProxyCommand=none",
		"-o", "RemoteCommand=none", "-o", "SessionType=default",
		"-B", d.Interface,
	}
	if d.Config.User != "" {
		args = append(args, "-l", d.Config.User)
	}
	if file := d.Config.IdentityFile; file != "" {
		if path, ok := strings.CutPrefix(file, "~/"); ok {
			home, err := os.UserHomeDir()
			if err != nil {
				return nil, errors.New("OpenWrt SSH: the user home directory is unavailable")
			}
			file = filepath.Join(home, path)
		}
		args = append(args, "-o", "IdentitiesOnly=yes", "-i", file)
	}
	path := d.Config.Command
	if path == "" {
		path = api.DefaultOpenWrtCommand
	}
	// SSH sends one command string to the remote shell. Quote the path as a
	// single word, including any literal quotes, and append a fixed operation.
	command := "'" + strings.ReplaceAll(path, "'", "'\"'\"'") + "' " + operation
	return append(args, "--", d.Config.Host, command), nil
}

type boundedSSHOutput struct{ buffer bytes.Buffer }

func (b *boundedSSHOutput) Write(p []byte) (int, error) {
	if len(p) > openWrtOutputLimit-b.buffer.Len() {
		return 0, errors.New("OpenWrt SSH output exceeded its limit")
	}
	return b.buffer.Write(p)
}

func runSSH(ctx context.Context, args ...string) ([]byte, error) {
	var out boundedSSHOutput
	cmd := exec.CommandContext(ctx, "/usr/bin/ssh", args...)
	cmd.Stdout = &out
	cmd.WaitDelay = time.Second
	err := cmd.Run()
	return out.buffer.Bytes(), err
}

func openWrtIP(s string, v4 bool) string {
	a, err := netip.ParseAddr(s)
	a = a.Unmap()
	if err != nil || a.Zone() != "" || !a.IsGlobalUnicast() || a.Is4() != v4 {
		return ""
	}
	return a.String()
}

// openWrtIPv6Prefix accepts a delegated prefix with or without a length.
func openWrtIPv6Prefix(s string) string {
	if p, err := netip.ParsePrefix(s); err == nil && openWrtIP(p.Addr().String(), false) != "" {
		return p.Masked().String()
	}
	return openWrtIP(s, false)
}

// openWrtLinkState accepts the short names printed by mwan3.
func openWrtLinkState(s string) string {
	if len(s) > 32 {
		return ""
	}
	for _, r := range s {
		if !(r >= 'a' && r <= 'z' || r >= '0' && r <= '9' || r == '-' || r == '_') {
			return ""
		}
	}
	return s
}

func openWrtText(s string) string {
	clean := []rune(strings.Map(func(r rune) rune {
		if unicode.IsControl(r) {
			return ' '
		}
		return r
	}, s))
	return strings.TrimSpace(string(clean[:min(len(clean), 160)]))
}
