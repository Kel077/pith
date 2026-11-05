package passthrough

import (
	"encoding/binary"
	"errors"
	"net/netip"
	"testing"

	"github.com/Kel077/pith/internal/api"
	"github.com/Kel077/pith/internal/engine/attribution"
	"github.com/Kel077/pith/internal/engine/capture"
)

type fakeSender struct {
	sent   []Packet
	names  []string
	err    error
	closed bool
}

func (s *fakeSender) Send(name string, p Packet) error {
	p.Data = append([]byte(nil), p.Data...)
	s.sent = append(s.sent, p)
	s.names = append(s.names, name)
	return s.err
}
func (s *fakeSender) Close() error { s.closed = true; return nil }

func legacySocket() attribution.Socket {
	return attribution.Socket{Local: netip.MustParseAddrPort("192.0.2.20:42000"), Remote: netip.MustParseAddrPort("198.51.100.1:443"), Generation: 17}
}
func tcpSegment(s attribution.Socket, flags byte) []byte {
	data := make([]byte, 24)
	binary.BigEndian.PutUint16(data, s.Local.Port())
	binary.BigEndian.PutUint16(data[2:], s.Remote.Port())
	data[12] = 5 << 4
	data[13] = flags
	copy(data[20:], "data")
	return data
}

func TestPreserveNativeSocketsAndRejectTupleReuse(t *testing.T) {
	socket := legacySocket()
	live := []attribution.Socket{socket}
	sender := new(fakeSender)
	table, err := New(Options{Routes: []capture.Route{{Prefix: netip.MustParsePrefix("0.0.0.0/0"), Interface: "utun3"}}, Sender: sender, Snapshot: func(proto api.Protocol) ([]attribution.Socket, error) {
		if proto == api.TCP {
			return live, nil
		}
		return nil, nil
	}})
	if err != nil {
		t.Fatal(err)
	}
	defer table.Close()
	segment := tcpSegment(socket, 16)
	if !table.Unknown(socket.Local, socket.Remote, api.TCP, segment) || len(sender.sent) != 1 || sender.names[0] != "utun3" {
		t.Fatal("lost original system/VPN route")
	}
	// A new connection reusing the tuple must be handled by current rules.
	live[0].Generation++
	if table.Unknown(socket.Local, socket.Remote, api.TCP, tcpSegment(socket, 2)) {
		t.Fatal("new SYN bypassed current rules")
	}
	if len(sender.sent) != 1 {
		t.Fatal("transmitted reused tuple")
	}
	// A native connection established in the snapshot/activation gap is safe to
	// adopt only after netstack has reported no endpoint for its ACK/data.
	if !table.Unknown(socket.Local, socket.Remote, api.TCP, segment) || len(sender.sent) != 2 {
		t.Fatal("startup race lost an existing native TCP connection")
	}
	table.Intercepted(socket.Local, socket.Remote)
	if table.Unknown(socket.Local, socket.Remote, api.TCP, segment) {
		t.Fatal("queued data bypassed a proxied connection's block/reset")
	}
	live = nil
	if err := table.Refresh(); err != nil {
		t.Fatal(err)
	}
	if table.Unknown(socket.Local, socket.Remote, api.TCP, segment) {
		t.Fatal("invented a native socket")
	}
}

func TestNewUDPIsNeverAdoptedAndSendFailureIsReported(t *testing.T) {
	socket := legacySocket()
	var live []attribution.Socket
	sender := &fakeSender{err: errors.New("native send failed")}
	table, err := New(Options{Routes: []capture.Route{{Prefix: netip.MustParsePrefix("0.0.0.0/0"), Interface: "en0"}}, Sender: sender, Snapshot: func(api.Protocol) ([]attribution.Socket, error) { return live, nil }})
	if err != nil {
		t.Fatal(err)
	}
	defer table.Close()
	live = []attribution.Socket{socket}
	if table.Unknown(socket.Local, socket.Remote, api.UDP, make([]byte, 8)) {
		t.Fatal("adopted a new UDP flow")
	}
	if !table.Unknown(socket.Local, socket.Remote, api.TCP, tcpSegment(socket, 16)) {
		t.Fatal("did not recognize native TCP")
	}
	select {
	case err := <-table.Faults():
		if err != sender.err {
			t.Fatal(err)
		}
	default:
		t.Fatal("silently dropped native socket data")
	}
}

func TestParseKernelPackets(t *testing.T) {
	socket := legacySocket()
	segment := tcpSegment(socket, 16)
	packet := make([]byte, 20+len(segment))
	packet[0] = 0x45
	packet[9] = 6
	binary.BigEndian.PutUint16(packet[2:], uint16(len(packet)))
	copy(packet[12:], socket.Local.Addr().AsSlice())
	copy(packet[16:], socket.Remote.Addr().AsSlice())
	copy(packet[20:], segment)
	p, ok := parse(packet)
	if !ok || p.Local != socket.Local || p.Remote != socket.Remote || string(p.Data) != string(segment) {
		t.Fatal(p, ok)
	}
	packet[6] = 0x20
	if _, ok := parse(packet); ok {
		t.Fatal("passed a fragment without reassembly")
	}
	packet6 := make([]byte, 40+len(segment))
	packet6[0] = 0x60
	packet6[6] = 6
	binary.BigEndian.PutUint16(packet6[4:], uint16(len(segment)))
	copy(packet6[8:], netip.MustParseAddr("2001:db8::1").AsSlice())
	copy(packet6[24:], netip.MustParseAddr("2001:db8::2").AsSlice())
	copy(packet6[40:], segment)
	p, ok = parse(packet6)
	if !ok || p.Local.Addr().String() != "2001:db8::1" || string(p.Data) != string(segment) {
		t.Fatal(p, ok)
	}
}
