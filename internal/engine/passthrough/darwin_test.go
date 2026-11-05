//go:build darwin

package passthrough

import (
	"encoding/binary"
	"github.com/Kel077/pith/internal/api"
	"testing"
)

func TestRawIPv4UsesDarwinByteOrderWithoutChangingTCP(t *testing.T) {
	s := legacySocket()
	segment := tcpSegment(s, 16)
	packet := ipv4Packet(Packet{s.Local, s.Remote, api.TCP, segment})
	if binary.NativeEndian.Uint16(packet[2:4]) != uint16(len(packet)) || string(packet[20:]) != string(segment) || packet[9] != 6 {
		t.Fatal("incorrect raw socket format")
	}
}
