package domains

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/hkdf"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
)

// QUIC observes a client Initial using the public Initial keys defined by
// RFC 9001 sections 5.2-5.4 and RFC 9369 section 3. It never modifies the packet,
// decrypts application data, or retains key material. Fragmentation across UDP
// datagrams, retries and unknown versions are intentionally best effort.
func QUIC(packet []byte) string {
	if len(packet) < 7 || len(packet) > MaxHello || packet[0]&0xc0 != 0xc0 {
		return ""
	}
	version := binary.BigEndian.Uint32(packet[1:5])
	if version == 1 && packet[0]&0x30 != 0 || version == 0x6b3343cf && packet[0]&0x30 != 0x10 {
		return ""
	}
	c := cursor{data: packet[5:]}
	dcid, scid := c.vector(1), c.vector(1)
	if dcid == nil || scid == nil || len(dcid) > 20 || len(scid) > 20 {
		return ""
	}
	token, ok := c.variable()
	if !ok || token > uint64(len(c.data)) || c.take(int(token)) == nil {
		return ""
	}
	length, ok := c.variable()
	if !ok || length < 20 || length > uint64(len(c.data)) {
		return ""
	}
	pnOffset := len(packet) - len(c.data)
	packet = packet[:pnOffset+int(length)]
	key, iv, hp := initialKeys(version, dcid)
	if key == nil {
		return ""
	}
	protect, _ := aes.NewCipher(hp)
	var mask [aes.BlockSize]byte
	protect.Encrypt(mask[:], packet[pnOffset+4:pnOffset+20])
	header := append([]byte(nil), packet[:pnOffset+4]...)
	header[0] ^= mask[0] & 0x0f
	if header[0]&0x0c != 0 {
		return ""
	}
	pnBytes := int(header[0]&3) + 1
	header = header[:pnOffset+pnBytes]
	for i := 0; i < pnBytes; i++ {
		header[pnOffset+i] ^= mask[1+i]
		iv[len(iv)-pnBytes+i] ^= header[pnOffset+i]
	}
	block, _ := aes.NewCipher(key)
	aead, _ := cipher.NewGCM(block)
	plain, err := aead.Open(nil, iv, packet[len(header):], header)
	if err != nil {
		return ""
	}
	return cryptoFrames(plain)
}

func initialKeys(version uint32, dcid []byte) (key, iv, hp []byte) {
	var saltHex, label string
	switch version {
	case 1:
		saltHex, label = "38762cf7f55934b34d179ae6a4c80cadccbb7f0a", "quic"
	case 0x6b3343cf:
		saltHex, label = "0dede3def700a6db819381be6e269dcbf9bd2ed9", "quicv2"
	default:
		return nil, nil, nil
	}
	salt, _ := hex.DecodeString(saltHex)
	secret, _ := hkdf.Extract(sha256.New, dcid, salt)
	client := expandLabel(secret, "client in", 32)
	return expandLabel(client, label+" key", 16), expandLabel(client, label+" iv", 12), expandLabel(client, label+" hp", 16)
}

func expandLabel(secret []byte, label string, size int) []byte {
	label = "tls13 " + label
	info := []byte{byte(size >> 8), byte(size), byte(len(label))}
	info = append(info, label...)
	info = append(info, 0) // empty TLS context
	out, _ := hkdf.Expand(sha256.New, secret, string(info), size)
	return out
}

func (c *cursor) variable() (uint64, bool) {
	if len(c.data) == 0 {
		return 0, false
	}
	size := 1 << (c.data[0] >> 6)
	p := c.take(size)
	if p == nil {
		return 0, false
	}
	v := uint64(p[0] & 0x3f)
	for _, b := range p[1:] {
		v = v<<8 | uint64(b)
	}
	return v, true
}

func cryptoFrames(plain []byte) string {
	c := cursor{data: plain}
	var hello, present []byte
	for len(c.data) > 0 {
		typeID, ok := c.variable()
		if !ok {
			return ""
		}
		switch typeID {
		case 0, 1: // PADDING, PING
		case 2, 3: // ACK, ACK_ECN
			_, a := c.variable() // largest acknowledged
			_, b := c.variable() // delay
			ranges, d := c.variable()
			_, e := c.variable() // first range
			if !a || !b || !d || !e || ranges > uint64(len(c.data)/2) {
				return ""
			}
			for range ranges {
				if _, ok := c.variable(); !ok {
					return ""
				}
				if _, ok := c.variable(); !ok {
					return ""
				}
			}
			if typeID == 3 {
				for range 3 {
					if _, ok := c.variable(); !ok {
						return ""
					}
				}
			}
		case 6: // CRYPTO
			offset, a := c.variable()
			length, b := c.variable()
			if !a || !b || offset > MaxHello || length > MaxHello-offset || length > uint64(len(c.data)) {
				return ""
			}
			end := int(offset + length)
			if len(hello) < end {
				hello = append(hello, make([]byte, end-len(hello))...)
				present = append(present, make([]byte, end-len(present))...)
			}
			for i, v := range c.take(int(length)) {
				at := int(offset) + i
				if present[at] != 0 && hello[at] != v {
					return ""
				}
				hello[at], present[at] = v, 1
			}
		default:
			return ""
		}
	}
	n := 0
	for n < len(present) && present[n] != 0 {
		n++
	}
	host, _ := clientHello(hello[:n])
	return host
}
