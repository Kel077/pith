package domains

import (
	"bytes"
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/tls"
	"encoding/binary"
	"encoding/hex"
	"io"
	"net"
	"testing"
	"time"
)

func helloRecord(t *testing.T) []byte {
	t.Helper()
	client, server := net.Pipe()
	done := make(chan struct{})
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	go func() {
		defer close(done)
		defer client.Close()
		_ = tls.Client(client, &tls.Config{ServerName: "Example.COM"}).HandshakeContext(ctx)
	}()
	defer func() { server.Close(); <-done }()
	_ = server.SetReadDeadline(time.Now().Add(time.Second))
	header := make([]byte, 5)
	if _, err := io.ReadFull(server, header); err != nil {
		t.Fatal(err)
	}
	record := append(header, make([]byte, binary.BigEndian.Uint16(header[3:]))...)
	if _, err := io.ReadFull(server, record[5:]); err != nil {
		t.Fatal(err)
	}
	return record
}

func TestTLSAndHTTPNames(t *testing.T) {
	record := helloRecord(t)
	for n := range len(record) {
		if host, more := TCP(record[:n]); host != "" || !more {
			t.Fatalf("incomplete TLS prefix %d: %q, more=%v", n, host, more)
		}
	}
	if host, more := TCP(record); host != "example.com" || more {
		t.Fatalf("TLS: %q, more=%v", host, more)
	}
	// A handshake can span several TLS records and arbitrary stream reads.
	part := len(record[5:]) / 2
	fragmented := []byte{22, 3, 1, byte(part >> 8), byte(part)}
	fragmented = append(fragmented, record[5:5+part]...)
	remaining := record[5+part:]
	fragmented = append(fragmented, 22, 3, 1, byte(len(remaining)>>8), byte(len(remaining)))
	fragmented = append(fragmented, remaining...)
	if host, _ := TCP(fragmented); host != "example.com" {
		t.Fatalf("fragmented TLS: %q", host)
	}
	for _, request := range []string{
		"GET / HTTP/1.1\r\nHost: Example.COM.:80\r\n\r\n",
		"CONNECT example.com:443 HTTP/1.1\r\nHost: example.com:443\r\n\r\n",
		"GET http://example.com/path HTTP/1.1\r\nHost: example.com\r\n\r\n",
	} {
		if host, more := TCP([]byte(request)); host != "example.com" || more {
			t.Fatalf("HTTP: %q, %v", host, more)
		}
	}
	for _, input := range [][]byte{[]byte("SSH-2.0-example\r\n"), []byte("GET / HTTP/1.1\r\nHost: *.example.com\r\n\r\n"), {22, 3, 3, 255, 255}} {
		if host, more := TCP(input); host != "" || more {
			t.Fatalf("invalid input accepted: %q, %v", host, more)
		}
	}
}

func TestSniffPreservesPrefixAndBoundsServerFirstWait(t *testing.T) {
	left, right := net.Pipe()
	done := make(chan struct{})
	payload := []byte("GET / HTTP/1.1\r\nHost: example.com\r\n\r\nbody")
	go func() { defer close(done); _, _ = left.Write(payload); left.Close() }()
	host, prefix, err := Sniff(right, time.Second)
	right.Close()
	<-done
	if err != nil || host != "example.com" || !bytes.Equal(prefix, payload) {
		t.Fatalf("sniff: %q, %q, %v", host, prefix, err)
	}
	left, right = net.Pipe()
	defer left.Close()
	defer right.Close()
	start := time.Now()
	host, prefix, err = Sniff(right, 20*time.Millisecond)
	if err != nil || host != "" || len(prefix) != 0 || time.Since(start) > time.Second {
		t.Fatalf("server-first timeout: %q, %v", host, err)
	}
}

func TestQUICInitialKeysAgainstRFCVectors(t *testing.T) {
	// RFC 9001 A.1 and RFC 9369 A.1, public protocol test vectors.
	cid, _ := hex.DecodeString("8394c8f03e515708")
	for _, tc := range []struct {
		version     uint32
		key, iv, hp string
	}{
		{1, "1f369613dd76d5467730efcbe3b1a22d", "fa044b2f42a3fd3b46fb255c", "9f50449e04a0e810283a1e9933adedd2"},
		{0x6b3343cf, "8b1a0bc121284290a29e0971b5cd045d", "91f73e2351d8fa91660e909f", "45b95e15235d6f45a6b19cbcb0294ba9"},
	} {
		key, iv, hp := initialKeys(tc.version, cid)
		if hex.EncodeToString(key) != tc.key || hex.EncodeToString(iv) != tc.iv || hex.EncodeToString(hp) != tc.hp {
			t.Fatalf("incorrect Initial keys for %x", tc.version)
		}
	}
}

func testVarint(n int) []byte {
	if n < 64 {
		return []byte{byte(n)}
	}
	if n < 16384 {
		return []byte{0x40 | byte(n>>8), byte(n)}
	}
	return []byte{0x80 | byte(n>>24), byte(n >> 16), byte(n >> 8), byte(n)}
}

func TestQUICVersionsAndAuthenticatedPayload(t *testing.T) {
	hello := helloRecord(t)[5:]
	for _, version := range []uint32{1, 0x6b3343cf} {
		// Reverse the two CRYPTO fragments to exercise offset reassembly.
		split := len(hello) / 2
		plain := append([]byte{6}, testVarint(split)...)
		plain = append(plain, testVarint(len(hello)-split)...)
		plain = append(plain, hello[split:]...)
		plain = append(plain, 6, 0)
		plain = append(plain, testVarint(split)...)
		plain = append(plain, hello[:split]...)
		plain = append(plain, 0, 0, 1) // padding and ping
		cid := []byte{1, 2, 3, 4, 5, 6, 7, 8}
		header := []byte{0xc3, byte(version >> 24), byte(version >> 16), byte(version >> 8), byte(version), 8}
		if version != 1 {
			header[0] = 0xd3
		}
		header = append(header, cid...)
		header = append(header, 0, 0) // empty SCID and token
		header = append(header, testVarint(4+len(plain)+16)...)
		pn := len(header)
		header = append(header, 0, 0, 0, 2)
		key, iv, hp := initialKeys(version, cid)
		iv[len(iv)-1] ^= 2
		block, _ := aes.NewCipher(key)
		aead, _ := cipher.NewGCM(block)
		packet := aead.Seal(append([]byte(nil), header...), iv, plain, header)
		protect, _ := aes.NewCipher(hp)
		var mask [16]byte
		protect.Encrypt(mask[:], packet[pn+4:pn+20])
		packet[0] ^= mask[0] & 15
		for i := 0; i < 4; i++ {
			packet[pn+i] ^= mask[1+i]
		}
		original := append([]byte(nil), packet...)
		if host := QUIC(packet); host != "example.com" {
			t.Fatalf("QUIC %x: %q", version, host)
		}
		if !bytes.Equal(original, packet) {
			t.Fatal("sniff changed a datagram")
		}
		packet[len(packet)-1] ^= 1
		if host := QUIC(packet); host != "" {
			t.Fatal("accepted unauthenticated Initial")
		}
	}
}

func FuzzNames(f *testing.F) {
	f.Add([]byte("GET / HTTP/1.1\r\nHost: example.com\r\n\r\n"))
	f.Add([]byte{22, 3, 3, 0, 1, 1})
	f.Add([]byte{0xc0, 0, 0, 0, 1, 0, 0})
	f.Fuzz(func(t *testing.T, data []byte) {
		if len(data) > MaxHello+1 {
			t.Skip()
		}
		TCP(data)
		QUIC(data)
		clientHello(data)
		cryptoFrames(data)
	})
}
