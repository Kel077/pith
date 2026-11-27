package router

import (
	"context"
	"errors"
	"io"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/Kel077/pith/internal/api"
)

const wanStatus = `{"ok":true,"router":"example-openwrt","device":"wan-demo","wan_up":true,"mac":"02:00:00:00:00:11","ipv4":"192.0.2.8","ipv6_prefix":"2001:db8:1::","mwan3_wan":"online","mwan3_usbwan":"online"}`
const wanChanged = `{"ok":true,"old_mac":"02:00:00:00:00:11","new_mac":"02:00:00:00:00:12","mac_changed":true,"old_ipv4":"192.0.2.8","new_ipv4":"192.0.2.9","old_ipv6_prefix":"2001:db8:1::","new_ipv6_prefix":"2001:db8:2::/56","ipv4_changed":true,"mwan3_wan":"online","mwan3_usbwan":"online","elapsed_sec":12}`

func TestOpenWrtSSHOperations(t *testing.T) {
	d := OpenWrt{Interface: "eth-demo", Config: api.OpenWrtRouter{Via: api.InterfaceRef{Name: "eth-demo"}, Host: "openwrt-demo"}}
	d.Run = func(ctx context.Context, args ...string) ([]byte, error) {
		deadline, ok := ctx.Deadline()
		if !ok || !slices.Contains(args, "BatchMode=yes") || !slices.Contains(args, "StrictHostKeyChecking=yes") ||
			!slices.Contains(args, "ClearAllForwardings=yes") {
			t.Fatalf("SSH session = %v (deadline %v)", args, deadline)
		}
		tail := args[len(args)-5:]
		want := []string{"-B", "eth-demo", "--", "openwrt-demo", "'/usr/sbin/wan-rotate-ip' --status"}
		switch {
		case slices.Equal(tail, want) && time.Until(deadline) <= openWrtStatusTimeout:
			return []byte(wanStatus), nil
		case tail[4] == "'/opt/example'\"'\"'s WAN tool' --rotate" && time.Until(deadline) > openWrtStatusTimeout && time.Until(deadline) <= openWrtRotateTimeout:
			if !slices.Equal(args[len(args)-9:len(args)-2], []string{"-l", "operator", "-o", "IdentitiesOnly=yes", "-i", "/tmp/example key", "--"}) {
				t.Fatalf("SSH overrides = %v", args)
			}
			return []byte(wanChanged), nil
		default:
			t.Fatalf("unexpected SSH args %v", args)
			return nil, nil
		}
	}
	status, err := d.Status(t.Context())
	if err != nil || !status.Connected || status.Interface != "eth-demo" || status.Model != "example-openwrt" ||
		status.PublicIPv4 != "192.0.2.8" || status.OpenWrt.IPv6Prefix != "2001:db8:1::" || status.OpenWrt.BackupState != "online" {
		t.Fatalf("status = %+v, %v", status, err)
	}
	d.Config.User, d.Config.IdentityFile, d.Config.Command = "operator", "/tmp/example key", "/opt/example's WAN tool"
	wan, err := d.Rotate(t.Context())
	want := api.WANRotation{BeforeIPv4: "192.0.2.8", IPv4: "192.0.2.9", BeforeIPv6Prefix: "2001:db8:1::", IPv6Prefix: "2001:db8:2::/56",
		MACChanged: true, WANState: "online", BackupState: "online", Seconds: 12}
	if err != nil || *wan != want {
		t.Fatalf("rotation = %+v, %v", wan, err)
	}
}

func TestOpenWrtSSHFailuresAndSameLease(t *testing.T) {
	exit := errors.New("private diagnostic from SSH")
	for _, test := range []struct {
		name, reply, want string
		runErr            error
		code              api.ErrorCode
	}{
		{"SSH failure", "", "check the host, known_hosts and SSH key", exit, api.ErrInternal},
		{"nonzero exit with success body", wanChanged, "SSH --rotate failed", exit, api.ErrInternal},
		{"malformed", `{}`, "invalid JSON", nil, api.ErrInternal},
		{"trailing data", wanChanged + "\n{}", "invalid JSON", nil, api.ErrInternal},
		{"oversized", wanChanged + strings.Repeat(" ", openWrtOutputLimit), "invalid JSON", nil, api.ErrInternal},
		{"script failure", `{"ok":false,"error":"timed out waiting for new WAN IPv4 lease"}`, "timed out waiting", exit, api.ErrInternal},
		{"busy", `{"ok":false,"error":"another WAN IP rotation is already in progress"}`, "already rotating", exit, api.ErrBusy},
		{"MAC did not change", strings.Replace(wanChanged, "00:00:12", "00:00:11", 1), "confirm a new WAN MAC", nil, api.ErrInternal},
		{"multicast MAC", strings.Replace(wanChanged, "02:00:00:00:00:12", "03:00:00:00:00:12", 1), "confirm a new WAN MAC", nil, api.ErrInternal},
		{"same IPv4", strings.Replace(wanChanged, "192.0.2.9", "192.0.2.8", 1), "", nil, ""},
	} {
		t.Run(test.name, func(t *testing.T) {
			cfg := api.OpenWrtRouter{Via: api.InterfaceRef{Name: "eth-demo"}, Host: "router.example.com"}
			d := OpenWrt{Config: cfg, Interface: cfg.Via.Name, Run: func(context.Context, ...string) ([]byte, error) {
				return []byte(test.reply), test.runErr
			}}
			wan, err := d.Rotate(t.Context())
			if api.Code(err) != test.code || err != nil && (!strings.Contains(err.Error(), test.want) || strings.Contains(err.Error(), exit.Error())) {
				t.Fatalf("rotation error = %v", err)
			}
			if test.want == "" && (wan == nil || !wan.MACChanged || wan.IPv4 != wan.BeforeIPv4) {
				t.Fatalf("same lease = %+v", wan)
			}
		})
	}
}

func TestOpenWrtStatusDownAndCanceled(t *testing.T) {
	d := OpenWrt{Config: api.OpenWrtRouter{Via: api.InterfaceRef{Name: "eth-demo"}, Host: "router.example.com"}, Interface: "eth-demo"}
	d.Run = func(context.Context, ...string) ([]byte, error) {
		return []byte(`{"ok":true,"router":"example-openwrt","wan_up":false,"mwan3_usbwan":"online"}`), nil
	}
	status, err := d.Status(t.Context())
	if err != nil || status.Connected || status.OpenWrt.BackupState != "online" {
		t.Fatalf("status = %+v, %v", status, err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	d.Run = func(ctx context.Context, _ ...string) ([]byte, error) {
		cancel()
		return nil, ctx.Err()
	}
	if _, err := d.Rotate(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled rotation = %v", err)
	}
}

func TestOpenWrtBoundsSSHOutput(t *testing.T) {
	var out boundedSSHOutput
	if _, err := io.Copy(&out, strings.NewReader(strings.Repeat("x", openWrtOutputLimit+1))); err == nil || out.buffer.Len() > openWrtOutputLimit {
		t.Fatalf("oversized SSH output: %v", err)
	}
}
