package echo

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"net/netip"
	"testing"
	"time"
)

func TestEchoReturnsOnlyRealReplies(t *testing.T) {
	for _, raw := range []string{"192.0.2.1", "2001:db8::1"} {
		t.Run(raw, func(t *testing.T) {
			source := netip.MustParseAddr(raw)
			dest := source.Next()
			request := []byte{8, 0, 0, 0, 0, 42, 0, 9, 1, 2, 3}
			if source.Is6() {
				request[0] = 128
				binary.BigEndian.PutUint16(request[2:4], checksum(pseudo(source, dest, request)))
			} else {
				binary.BigEndian.PutUint16(request[2:4], checksum(request))
			}
			replies := make(chan []byte, 1)
			r := New(Options{System: func(netip.Addr) string { return "utun3" }, Write: func(p []byte) error { replies <- p; return nil }, Exchange: func(ctx context.Context, name string, addr netip.Addr, msg []byte) ([]byte, error) {
				if name != "utun3" || addr != dest || !bytes.Equal(msg, request) {
					t.Error(name, addr, msg)
				}
				result := append([]byte(nil), msg...)
				result[0] = 0
				if addr.Is6() {
					result[0] = 129
				}
				result[4], result[5] = 99, 99
				return result, nil
			}})
			defer r.Close()
			r.Handle(source, dest, request)
			select {
			case p := <-replies:
				var msg []byte
				if source.Is4() {
					msg = p[20:]
					if checksum(p[:20]) != 0 || checksum(msg) != 0 {
						t.Fatal("bad IPv4 checksum")
					}
				} else {
					msg = p[40:]
					if checksum(pseudo(dest, source, msg)) != 0 {
						t.Fatal("bad IPv6 checksum")
					}
				}
				if !bytes.Equal(msg[4:], request[4:]) {
					t.Fatal("did not restore ping identity/payload")
				}
			case <-time.After(time.Second):
				t.Fatal("no echo reply")
			}
		})
	}
}

func TestCloseCancelsEchoAndNeverFakesSuccess(t *testing.T) {
	started := make(chan struct{})
	r := New(Options{System: func(netip.Addr) string { return "en0" }, Write: func([]byte) error { t.Error("fabricated echo reply"); return nil }, Exchange: func(ctx context.Context, _ string, _ netip.Addr, _ []byte) ([]byte, error) {
		close(started)
		<-ctx.Done()
		return nil, ctx.Err()
	}})
	message := []byte{8, 0, 0, 0, 0, 1, 0, 1}
	binary.BigEndian.PutUint16(message[2:4], checksum(message))
	r.Handle(netip.MustParseAddr("192.0.2.1"), netip.MustParseAddr("192.0.2.2"), message)
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("echo did not start")
	}
	r.Close()
	r.opts.Exchange = func(context.Context, string, netip.Addr, []byte) ([]byte, error) {
		t.Error("echo started after close")
		return nil, errors.New("closed")
	}
	r.Handle(netip.MustParseAddr("192.0.2.1"), netip.MustParseAddr("192.0.2.2"), message)
}
