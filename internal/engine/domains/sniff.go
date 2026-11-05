// Package domains observes names without terminating TLS or changing payloads.
package domains

import (
	"bufio"
	"bytes"
	"encoding/binary"
	"errors"
	"io"
	"net"
	"net/http"
	"strings"
	"time"

	"github.com/Kel077/pith/internal/api"
)

const MaxHello = 64 << 10

// Sniff reads a bounded prefix with a single, short deadline. The caller must
// forward prefix even when an error accompanies it. A timeout is not a failure:
// server-first protocols must be able to connect without sending a greeting.
func Sniff(conn net.Conn, timeout time.Duration) (host string, prefix []byte, err error) {
	if err = conn.SetReadDeadline(time.Now().Add(timeout)); err != nil {
		return
	}
	defer conn.SetReadDeadline(time.Time{})
	prefix = make([]byte, 0, 4096)
	buf := make([]byte, 4096)
	for len(prefix) < MaxHello {
		n, readErr := conn.Read(buf[:min(len(buf), MaxHello-len(prefix))])
		prefix = append(prefix, buf[:n]...)
		var more bool
		host, more = TCP(prefix)
		if !more || readErr != nil || n == 0 {
			var ne net.Error
			if errors.As(readErr, &ne) && ne.Timeout() || errors.Is(readErr, io.EOF) {
				readErr = nil
			}
			return host, prefix, readErr
		}
	}
	return host, prefix, nil
}

// TCP recognizes a TLS ClientHello (including fragmented records) or an HTTP/1
// request. more means the input is a recognized, incomplete prefix.
func TCP(data []byte) (host string, more bool) {
	if len(data) == 0 {
		return "", true
	}
	if len(data) > MaxHello {
		return "", false
	}
	if data[0] == 22 {
		var handshake []byte
		for len(data) > 0 {
			if len(data) < 5 {
				return "", true
			}
			if data[0] != 22 || data[1] != 3 {
				return "", false
			}
			n := int(binary.BigEndian.Uint16(data[3:5]))
			if n == 0 || n > 16384 {
				return "", false
			}
			if len(data) < 5+n {
				return "", true
			}
			handshake = append(handshake, data[5:5+n]...)
			if host, more := clientHello(handshake); !more {
				return host, false
			}
			data = data[5+n:]
		}
		return "", true
	}
	known := false
	for _, method := range []string{"GET ", "POST ", "HEAD ", "PUT ", "DELETE ", "OPTIONS ", "PATCH ", "CONNECT ", "TRACE "} {
		if bytes.HasPrefix(data, []byte(method)) || bytes.HasPrefix([]byte(method), data) {
			known = true
			break
		}
	}
	if !known {
		return "", false
	}
	if !bytes.Contains(data, []byte("\r\n\r\n")) {
		return "", len(data) < MaxHello
	}
	req, err := http.ReadRequest(bufio.NewReader(bytes.NewReader(data)))
	if err != nil {
		return "", false
	}
	return normalize(req.Host), false
}

func normalize(host string) string {
	// A wire name is a host, not the more permissive URL/target input accepted
	// by the configuration parser. Reject wildcard and URL syntax explicitly.
	if strings.TrimSpace(host) != host || strings.ContainsAny(host, "/*?#@\\\x00\r\n\t ") {
		return ""
	}
	target, err := api.ParseTarget(host)
	if err != nil || target.Subdomains || !target.IsDomain() {
		return ""
	}
	return target.Domain
}

type cursor struct{ data []byte }

func (c *cursor) take(n int) []byte {
	if n < 0 || n > len(c.data) {
		c.data = nil
		return nil
	}
	p := c.data[:n]
	c.data = c.data[n:]
	return p
}
func (c *cursor) vector(width int) []byte {
	p := c.take(width)
	if len(p) != width {
		return nil
	}
	n := 0
	for _, v := range p {
		n = n<<8 | int(v)
	}
	return c.take(n)
}

// clientHello consumes the TLS handshake framing, also used directly by QUIC.
// It does not parse or retain any application data or TLS secrets.
func clientHello(data []byte) (string, bool) {
	if len(data) < 4 {
		return "", true
	}
	if data[0] != 1 {
		return "", false
	}
	n := int(data[1])<<16 | int(data[2])<<8 | int(data[3])
	if n > MaxHello-4 {
		return "", false
	}
	if len(data) < n+4 {
		return "", true
	}
	c := cursor{data: data[4 : 4+n]}
	if len(c.take(34)) != 34 || c.vector(1) == nil || len(c.vector(2)) < 2 || len(c.vector(1)) == 0 {
		return "", false
	}
	extensions := c.vector(2)
	if extensions == nil || len(c.data) != 0 {
		return "", false
	}
	e := cursor{data: extensions}
	for len(e.data) > 0 {
		kind := e.take(2)
		body := e.vector(2)
		if len(kind) != 2 || body == nil {
			return "", false
		}
		if binary.BigEndian.Uint16(kind) != 0 {
			continue
		}
		b := cursor{data: body}
		list := b.vector(2)
		if list == nil || len(b.data) != 0 {
			return "", false
		}
		names := cursor{data: list}
		for len(names.data) > 0 {
			typeID := names.take(1)
			name := names.vector(2)
			if len(typeID) != 1 || len(name) == 0 {
				return "", false
			}
			if typeID[0] == 0 {
				return normalize(string(name)), false
			}
		}
		return "", false
	}
	return "", false
}
