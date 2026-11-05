package attribution

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"net/netip"
	"os"
	"path/filepath"
	"testing"

	"github.com/Kel077/pith/internal/api"
	"howett.net/plist"
)

// Explicit packed fields describe the public wire ABI independently of the
// decoder offsets. No fixture contains a capture from the developer's Mac.
func pcbFixture(t *testing.T, ipv6 bool) []byte {
	t.Helper()
	buf := new(bytes.Buffer)
	write := func(value any) {
		t.Helper()
		if err := binary.Write(buf, binary.LittleEndian, value); err != nil {
			t.Fatal(err)
		}
	}
	generation := struct {
		Length, Count                uint32
		Generation, SocketGeneration uint64
	}{24, 1, 50, 60}
	write(generation)
	write(struct {
		Length, Kind                                   uint32
		Handle                                         uint64
		Type, Pad0                                     uint16
		Options                                        uint32
		Linger, State                                  uint16
		PCB                                            uint64
		Protocol, Family                               uint32
		Queue, Incomplete, Limit, Timeout, Error, Pad1 uint16
		Group, Mark, UID, PID, Effective               uint32
		Generation                                     uint64
		Flags, Flags1, UseCount, RetainCount, Filter   uint32
	}{Length: 104, Kind: 1, Handle: 1, PCB: 2, Protocol: 6, PID: 42, Effective: 84, Generation: 55})
	// Include an unknown extension record to verify length-based iteration.
	write(struct {
		Length, Kind uint32
		Extension    [24]byte
	}{Length: 32, Kind: 1024})
	var remote, local [16]byte
	flag := uint8(1)
	if ipv6 {
		flag = 2
		remote = netip.MustParseAddr("2001:db8::2").As16()
		local = netip.MustParseAddr("2001:db8::1").As16()
	} else {
		copy(remote[12:], []byte{192, 0, 2, 2})
		copy(local[12:], []byte{192, 0, 2, 1})
	}
	write(struct {
		Length, Kind               uint32
		PCB                        uint64
		RemotePort, LocalPort      [2]byte
		ProtocolPCB, Generation    uint64
		Flags, Flow                uint32
		VFlag, TTL, Protocol, Pad0 uint8
		Remote, Local              [16]byte
		Tail                       [24]byte
	}{Length: 104, Kind: 16, PCB: 2, RemotePort: [2]byte{1, 187}, LocalPort: [2]byte{0xc3, 0x50}, Generation: 49, VFlag: flag, Remote: remote, Local: local})
	write(generation)
	return buf.Bytes()
}

func TestParsePCB(t *testing.T) {
	for _, v6 := range []bool{false, true} {
		data := pcbFixture(t, v6)
		sockets, err := ParsePCB(data, api.TCP)
		if err != nil || len(sockets) != 1 {
			t.Fatal(len(sockets), err)
		}
		// The current kernel emits INPCB before SOCKET; both orders are valid
		// because these tagged records are grouped by the same PCB identity.
		reordered := append([]byte{}, data[:24]...)
		reordered = append(reordered, data[24+104+32:24+104+32+104]...)
		reordered = append(reordered, data[24:24+104+32]...)
		reordered = append(reordered, data[len(data)-24:]...)
		if reorderedSockets, err := ParsePCB(reordered, api.TCP); err != nil || len(reorderedSockets) != 1 || reorderedSockets[0] != sockets[0] {
			t.Fatal("record ordering changed attribution", err)
		}
		s := sockets[0]
		want := netip.MustParseAddrPort("192.0.2.1:50000")
		if v6 {
			want = netip.MustParseAddrPort("[2001:db8::1]:50000")
		}
		if s.Local != want || s.Remote.Port() != 443 || s.PID != 42 || s.EffectivePID != 84 || s.Generation != 55 {
			t.Fatal(s)
		}
		for _, cut := range []int{0, 3, 23, 25, 40, len(data) - 1} {
			if _, err := ParsePCB(data[:cut], api.TCP); err == nil {
				t.Errorf("accepted truncated PCB at %d", cut)
			}
		}
		bad := bytes.Clone(data)
		binary.LittleEndian.PutUint32(bad[24:28], 0)
		if _, err := ParsePCB(bad, api.TCP); err == nil {
			t.Fatal("accepted zero-length record")
		}
		bad = bytes.Clone(data)
		binary.LittleEndian.PutUint64(bad[24+28:24+36], 999)
		if _, err := ParsePCB(bad, api.TCP); err == nil {
			t.Fatal("accepted mismatched socket identity")
		}
		bad = bytes.Clone(data)
		binary.LittleEndian.PutUint64(bad[24+104+32+28:24+104+32+36], 51)
		if sockets, err := ParsePCB(bad, api.TCP); err != nil || len(sockets) != 0 {
			t.Fatal("retained PCB created during snapshot")
		}
	}
}

func TestFindWildcardsAndAmbiguity(t *testing.T) {
	local, remote := netip.MustParseAddrPort("192.0.2.1:50000"), netip.MustParseAddrPort("192.0.2.2:443")
	wild := Socket{Local: netip.MustParseAddrPort("0.0.0.0:50000"), Remote: netip.MustParseAddrPort("0.0.0.0:0"), PID: 1}
	exact := Socket{Local: local, Remote: remote, PID: 2}
	if s, err := Find([]Socket{wild, exact}, local, remote); err != nil || s.PID != 2 {
		t.Fatal(s, err)
	}
	other := wild
	other.PID = 3
	if _, err := Find([]Socket{wild, other}, local, remote); !errors.Is(err, ErrAmbiguous) {
		t.Fatal(err)
	}
	if s, err := Find([]Socket{wild, other, exact}, local, remote); err != nil || s.PID != 2 {
		t.Fatal(s, err)
	}
	if _, err := Find([]Socket{exact}, local, netip.MustParseAddrPort("192.0.2.3:443")); !errors.Is(err, ErrNotFound) {
		t.Fatal(err)
	}
	local6, remote6 := netip.MustParseAddrPort("[2001:db8::1]:50000"), netip.MustParseAddrPort("[2001:db8::2]:443")
	if _, err := Find([]Socket{wild}, local6, remote6); !errors.Is(err, ErrNotFound) {
		t.Fatal("IPv4 wildcard matched IPv6", err)
	}
	wild.families = 3
	if s, err := Find([]Socket{wild}, local6, remote6); err != nil || s.PID != wild.PID {
		t.Fatal("dual-stack wildcard did not match IPv6", err)
	}
}

func TestBundleAttribution(t *testing.T) {
	bundle := filepath.Join(t.TempDir(), "Example.app")
	if err := os.MkdirAll(filepath.Join(bundle, "Contents"), 0700); err != nil {
		t.Fatal(err)
	}
	for _, format := range []int{plist.XMLFormat, plist.BinaryFormat} {
		data, err := plist.Marshal(map[string]string{"CFBundleIdentifier": "com.example.app", "CFBundleDisplayName": "Example App"}, format)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(bundle, "Contents", "Info.plist"), data, 0600); err != nil {
			t.Fatal(err)
		}
		app, err := AppForPath(filepath.Join(bundle, "Contents", "XPCServices", "Child.app", "Contents", "MacOS", "child"))
		if err != nil || app.Path != bundle || app.BundleID != "com.example.app" || app.Name != "Example App" {
			t.Fatal(app, err)
		}
	}
	app, err := AppForPath("/usr/bin/example-tool")
	if err != nil || app.Path != "/usr/bin/example-tool" || app.Name != "example-tool" {
		t.Fatal(app, err)
	}
}

func TestDelegationCacheAndPIDReuse(t *testing.T) {
	r := New()
	local, remote := netip.MustParseAddrPort("192.0.2.1:50000"), netip.MustParseAddrPort("192.0.2.2:443")
	r.read = func(api.Protocol) ([]Socket, error) {
		return []Socket{{Local: local, Remote: remote, PID: 42, EffectivePID: 84}}, nil
	}
	unique := uint64(1)
	r.identify = func(pid int) (identity, error) { return identity{unique: unique + uint64(pid)}, nil }
	pathCalls := 0
	r.path = func(pid int) (string, error) {
		pathCalls++
		if pid == 42 {
			return "/usr/bin/proxy", nil
		}
		if unique == 1 {
			return "/usr/bin/served-app", nil
		}
		return "/usr/bin/new-app", nil
	}
	for range 2 {
		result, err := r.Lookup(context.Background(), api.TCP, local, remote)
		if err != nil || result.Process.Path != "/usr/bin/proxy" || result.App.Path != "/usr/bin/served-app" || !result.Delegated {
			t.Fatal(result, err)
		}
	}
	if pathCalls != 2 {
		t.Fatal("process cache missed")
	}
	unique++
	result, err := r.Lookup(context.Background(), api.TCP, local, remote)
	if err != nil || result.App.Path != "/usr/bin/new-app" {
		t.Fatal(result, err)
	}
	r.identify = func(pid int) (identity, error) {
		if pid == 84 {
			return identity{}, os.ErrNotExist
		}
		return identity{unique: 44}, nil
	}
	result, err = r.Lookup(context.Background(), api.TCP, local, remote)
	if err == nil || result.App.Path != "" || result.Process.Path != "/usr/bin/proxy" {
		t.Fatal("delegated lookup fell back to proxy", result, err)
	}
}

func FuzzParsePCB(f *testing.F) {
	f.Add([]byte{})
	f.Add([]byte{24, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0})
	f.Fuzz(func(t *testing.T, data []byte) { _, _ = ParsePCB(data, api.TCP) })
}
