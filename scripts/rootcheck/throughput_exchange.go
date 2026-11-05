package main

import (
	"bufio"
	"bytes"
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/Kel077/pith/internal/netif"
)

type throughputRequest struct {
	Name   string `json:"name"`
	Upload bool   `json:"upload"`
	Bytes  int64  `json:"bytes"`
}

type throughputSample struct {
	throughputRequest
	Local    string        `json:"local"`
	Remote   string        `json:"remote"`
	Verified bool          `json:"verifiedTLS"`
	Status   int           `json:"status"`
	Payload  int64         `json:"payloadBytes"`
	Sent     int64         `json:"sentTLSBytes"`
	Received int64         `json:"receivedTLSBytes"`
	Elapsed  time.Duration `json:"httpElapsed"`
	Total    time.Duration `json:"totalElapsed"`
	Mbps     float64       `json:"applicationMbps"`
	Error    string        `json:"error,omitempty"`
}

type throughputDial func(context.Context, string, string) (net.Conn, error)

func dialThroughput(ctx context.Context, iface, target string) (net.Conn, error) {
	d, err := netif.Dialer(iface, 3*time.Second, nil)
	if err != nil {
		return nil, err
	}
	return d.DialContext(ctx, "tcp4", target)
}

// The meter sits below TLS. Reservations protect a combined bidirectional limit
// even if the transport performs concurrent I/O; unused reservations are returned.
type throughputConn struct {
	net.Conn
	mu                   sync.Mutex
	left, sent, received int64
}

var errThroughputBudget = errors.New("throughput TLS socket byte limit reached")

func (c *throughputConn) transfer(p []byte, write bool) (int, error) {
	c.mu.Lock()
	if c.left <= 0 || write && int64(len(p)) > c.left {
		c.mu.Unlock()
		return 0, errThroughputBudget
	}
	n := min(int64(len(p)), c.left)
	c.left -= n
	c.mu.Unlock()
	var count int
	var err error
	if write {
		count, err = c.Conn.Write(p[:int(n)])
	} else {
		count, err = c.Conn.Read(p[:int(n)])
	}
	c.mu.Lock()
	c.left += n - int64(count)
	if write {
		c.sent += int64(count)
	} else {
		c.received += int64(count)
	}
	c.mu.Unlock()
	return count, err
}
func (c *throughputConn) Read(p []byte) (int, error)  { return c.transfer(p, false) }
func (c *throughputConn) Write(p []byte) (int, error) { return c.transfer(p, true) }

type zeroSource struct{}

func (zeroSource) Read(p []byte) (int, error) { clear(p); return len(p), nil }

// One raw socket and one HTTP request. Avoid http.Transport's connection retries,
// proxy selection, redirect handling, automatic decompression and pooling.
func exchangeThroughput(parent context.Context, plan *throughputPlan, iface string, request throughputRequest, dial throughputDial, tlsConfig *tls.Config) (result throughputSample, resultErr error) {
	result.throughputRequest = request
	if request.Bytes <= 0 || request.Bytes > 256<<20 {
		return result, errors.New("invalid throughput payload limit")
	}
	timeout := throughputTimeout
	if request.Name == "warmup" {
		timeout = 5 * time.Second
	}
	ctx, cancel := context.WithTimeout(parent, timeout)
	defer cancel()
	started := time.Now()
	defer func() {
		result.Total = time.Since(started)
		if resultErr != nil {
			result.Error = resultErr.Error()
		}
	}()
	if dial == nil {
		dial = dialThroughput
	}
	raw, err := dial(ctx, iface, plan.Target)
	if err != nil {
		return result, err
	}
	meter := &throughputConn{Conn: raw, left: throughputSocketLimit(request.Bytes)}
	defer raw.Close()
	closed := make(chan struct{})
	stop := context.AfterFunc(ctx, func() { raw.Close(); close(closed) })
	defer func() {
		if !stop() {
			<-closed
		}
	}()
	deadline, _ := ctx.Deadline()
	if err := raw.SetDeadline(deadline); err != nil {
		return result, err
	}
	result.Local, result.Remote = raw.LocalAddr().String(), raw.RemoteAddr().String()
	config := &tls.Config{}
	if tlsConfig != nil {
		config = tlsConfig.Clone()
	}
	config.ServerName, config.MinVersion, config.NextProtos = plan.ServerName, tls.VersionTLS12, []string{"http/1.1"}
	config.InsecureSkipVerify = false
	conn := tls.Client(meter, config)
	defer func() {
		_ = raw.SetWriteDeadline(time.Now().Add(100 * time.Millisecond))
		_ = conn.Close()
		meter.mu.Lock()
		result.Sent, result.Received = meter.sent, meter.received
		meter.mu.Unlock()
	}()
	if err := conn.HandshakeContext(ctx); err != nil {
		return result, err
	}
	state := conn.ConnectionState()
	if len(state.VerifiedChains) == 0 || state.NegotiatedProtocol != "" && state.NegotiatedProtocol != "http/1.1" {
		return result, errors.New("throughput requires verified HTTP/1.1 TLS")
	}
	result.Verified = true
	method, path := "GET", "/__down"
	if request.Upload {
		method, path = "POST", "/__up"
	}
	uri := fmt.Sprintf("%s?bytes=%d", path, request.Bytes)
	var body io.Reader
	if request.Upload {
		body = io.LimitReader(zeroSource{}, request.Bytes)
	}
	req, err := http.NewRequestWithContext(ctx, method, "https://"+plan.ServerName+uri, body)
	if err != nil {
		return result, err
	}
	if request.Upload {
		req.ContentLength = request.Bytes
		req.Header.Set("Content-Type", "application/octet-stream")
	}
	req.Close = true
	req.Header.Set("User-Agent", "Pith-rootcheck")
	req.Header.Set("Accept-Encoding", "identity")
	req.Header.Set("Cache-Control", "no-store")
	transferStarted := time.Now()
	if err := req.Write(conn); err != nil {
		return result, err
	}
	reader := bufio.NewReaderSize(conn, 16<<10)
	var header bytes.Buffer
	for {
		line, err := reader.ReadSlice('\n')
		if header.Len()+len(line) > 16<<10 {
			return result, errors.New("throughput response header exceeds 16 KiB")
		}
		header.Write(line)
		if err != nil {
			return result, err
		}
		if bytes.Equal(line, []byte("\r\n")) {
			break
		}
	}
	response, err := http.ReadResponse(bufio.NewReader(io.MultiReader(bytes.NewReader(header.Bytes()), reader)), req)
	if err != nil {
		return result, err
	}
	// No Body.Close drain on an untrusted or oversized response; closing the TLS
	// socket below ends it without consuming additional body bytes.
	defer func() { _ = raw.Close(); _ = response.Body.Close() }()
	result.Status = response.StatusCode
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return result, fmt.Errorf("throughput endpoint returned HTTP %d; no redirect or retry", response.StatusCode)
	}
	if encoding := response.Header.Get("Content-Encoding"); encoding != "" && !strings.EqualFold(encoding, "identity") {
		return result, errors.New("compressed throughput response cannot prove the requested payload")
	}
	limit := request.Bytes
	if request.Upload {
		limit = 4096
	}
	n, err := io.Copy(io.Discard, io.LimitReader(response.Body, limit+1))
	result.Elapsed = time.Since(transferStarted)
	if err != nil {
		return result, err
	}
	if n > limit || !request.Upload && n != request.Bytes {
		return result, errors.New("throughput response length differs from the frozen payload limit")
	}
	result.Payload = request.Bytes
	if result.Elapsed > 0 {
		result.Mbps = float64(result.Payload) * 8 / result.Elapsed.Seconds() / 1e6
	}
	return result, nil
}
