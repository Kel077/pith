package forward

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"net/netip"
	"runtime"
	"sync"
	"sync/atomic"
	"testing"

	"golang.zx2c4.com/wireguard/tun"
)

func bufferTestPacket(size, version, protocol int, id uint32) []byte {
	packet := bytes.Repeat([]byte{byte(id)}, size)
	header := 20
	if version == 6 {
		header = 40
		clear(packet[:header])
		packet[0], packet[6], packet[7] = 0x60, byte(protocol), 64
		binary.BigEndian.PutUint16(packet[4:6], uint16(size-header))
		source, destination := netip.MustParseAddr("2001:db8::1").As16(), netip.MustParseAddr("2001:db8::2").As16()
		copy(packet[8:24], source[:])
		copy(packet[24:40], destination[:])
	} else {
		clear(packet[:header])
		packet[0], packet[8], packet[9] = 0x45, 64, byte(protocol)
		binary.BigEndian.PutUint16(packet[2:4], uint16(size))
		copy(packet[12:16], []byte{192, 0, 2, 1})
		copy(packet[16:20], []byte{198, 51, 100, 1})
	}
	binary.BigEndian.PutUint32(packet[header:header+4], id)
	return packet
}

type observingPacketDevice struct {
	tun.Device // Only Write is used; no OS device, interface or route is opened.
	t          *testing.T
	active     atomic.Int32
	mu         sync.Mutex
	packets    [][]byte
	bases      []*byte
	nextError  error
}

func (d *observingPacketDevice) Write(bufs [][]byte, offset int) (int, error) {
	if d.active.Add(1) != 1 {
		d.t.Error("device writes overlap")
	}
	defer d.active.Add(-1)
	if len(bufs) != 1 || offset != 4 || len(bufs[0]) < offset {
		return 0, errors.New("invalid packet framing")
	}
	data := bufs[0]
	if !bytes.Equal(data[:offset], []byte{0, 0, 0, 0}) {
		d.t.Error("a previous mutable native header escaped into the next write")
	}
	copyBefore := bytes.Clone(data[offset:])
	// The native Darwin writer mutates the reserved header and consumes the
	// buffer synchronously. Yield while retaining it to expose early reuse.
	copy(data[:offset], []byte{1, 2, 3, 4})
	runtime.Gosched()
	if !bytes.Equal(data[offset:], copyBefore) {
		d.t.Error("packet storage changed before the device write returned")
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	d.packets = append(d.packets, bytes.Clone(data[offset:]))
	d.bases = append(d.bases, &data[0])
	err := d.nextError
	d.nextError = nil
	if err != nil {
		return 0, err
	}
	return 1, nil
}

func TestWritePacketPreservesLengthsHeadersAndErrors(t *testing.T) {
	d := &observingPacketDevice{t: t}
	f := &Forwarder{opts: Options{Device: d}}
	packets := [][]byte{
		bufferTestPacket(1500, 4, 6, 1),
		bufferTestPacket(4096, 6, 17, 2),
		bufferTestPacket(64, 4, 1, 3),
		bufferTestPacket(1280, 6, 58, 4),
	}
	wantError := errors.New("synthetic device error")
	for i, packet := range packets {
		before := bytes.Clone(packet)
		if i == 2 {
			d.nextError = wantError
		}
		err := f.WritePacket(packet)
		if i == 2 && !errors.Is(err, wantError) || i != 2 && err != nil {
			t.Fatal("device error behavior changed", err)
		}
		if !bytes.Equal(packet, before) || !bytes.Equal(d.packets[i], packet) {
			t.Fatal("input was modified, output corrupted, or a longer packet tail leaked")
		}
	}
	if d.bases[1] != d.bases[2] || d.bases[2] != d.bases[3] || cap(f.writeBuffers[0]) != len(packets[1])+4 {
		t.Fatal("writes did not reuse a single buffer after growth")
	}
}

func TestWritePacketConcurrentProtocolsKeepIndependentPayloads(t *testing.T) {
	d := &observingPacketDevice{t: t}
	f := &Forwarder{opts: Options{Device: d}}
	var packets [][]byte
	want := make(map[string]int)
	for i := range 96 {
		size, version, protocol := 64, 4, 1
		if i%3 == 1 {
			size, protocol = 1500, 6
		} else if i%3 == 2 {
			size, version, protocol = 1280, 6, 17
		}
		p := bufferTestPacket(size, version, protocol, uint32(i+1))
		packets = append(packets, p)
		want[string(p)]++
	}
	var wg sync.WaitGroup
	start := make(chan struct{})
	for _, packet := range packets {
		wg.Go(func() {
			<-start
			if err := f.WritePacket(packet); err != nil {
				t.Error(err)
			}
		})
	}
	close(start)
	wg.Wait()
	for _, p := range d.packets {
		want[string(p)]--
	}
	for _, n := range want {
		if n != 0 {
			t.Fatal("concurrent packet was lost, duplicated or corrupted")
		}
	}
	if len(d.packets) != len(packets) {
		t.Fatal("packet count changed")
	}
}

type packetSink struct {
	tun.Device
	value byte
}

func (d *packetSink) Write(bufs [][]byte, offset int) (int, error) {
	for _, data := range bufs {
		d.value ^= data[offset] ^ data[len(data)-1]
	}
	return len(bufs), nil
}

func BenchmarkWritePacket(b *testing.B) {
	for _, size := range []int{64, 1500, 4096} {
		b.Run(fmt.Sprint(size), func(b *testing.B) {
			f := &Forwarder{opts: Options{Device: &packetSink{}}}
			packet := bufferTestPacket(size, 4, 17, 1)
			if err := f.WritePacket(packet); err != nil {
				b.Fatal(err)
			}
			b.SetBytes(int64(size))
			b.ReportAllocs()
			for b.Loop() {
				if err := f.WritePacket(packet); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}
