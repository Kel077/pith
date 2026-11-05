package main

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/netip"
	"sync"
	"time"

	"github.com/Kel077/pith/internal/engine/domains"
	quic "github.com/quic-go/quic-go"
	"github.com/quic-go/quic-go/http3"
)

const (
	quicRequestTimeout = 10 * time.Second
	quicSendLimit      = 64 << 10
	quicReceiveLimit   = 256 << 10
	quicBodyLimit      = 16 << 10
)

type quicEvidence struct {
	Local             string        `json:"local"`
	Remote            string        `json:"remote"`
	Nonce             string        `json:"nonce"`
	StatusCode        int           `json:"statusCode"`
	Protocol          string        `json:"protocol"`
	ALPN              string        `json:"alpn"`
	Verified          bool          `json:"verified"`
	BodyBytes         int           `json:"bodyBytes"`
	BodyPrefixSHA256  string        `json:"bodyPrefixSHA256,omitempty"`
	BodyLimited       bool          `json:"bodyLimited,omitempty"`
	FirstDatagramSNI  string        `json:"firstDatagramSNI,omitempty"`
	FragmentedHello   bool          `json:"fragmentedHello"`
	Sent              uint64        `json:"sent"`
	Received          uint64        `json:"received"`
	SentDatagrams     int           `json:"sentDatagrams"`
	ReceivedDatagrams int           `json:"receivedDatagrams"`
	Elapsed           time.Duration `json:"elapsed"`
	Error             string        `json:"error,omitempty"`
}

// A connected UDP socket pins both address family and destination. The wrapper
// exposes only PacketConn, preventing UDP offload/batching from bypassing its
// counters and budgets. It observes no plaintext beyond the Initial's SNI.
type quicPackets struct {
	net.Conn
	mu                           sync.Mutex
	writeMu, readMu              sync.Mutex
	sent, received               uint64
	sentPackets, receivedPackets int
	firstSNI                     string
	budgetErr                    error
}

func (p *quicPackets) SetReadBuffer(size int) error {
	if conn, ok := p.Conn.(interface{ SetReadBuffer(int) error }); ok {
		return conn.SetReadBuffer(min(size, quicReceiveLimit))
	}
	return errors.New("QUIC socket has no receive buffer control")
}

func (p *quicPackets) SetWriteBuffer(size int) error {
	if conn, ok := p.Conn.(interface{ SetWriteBuffer(int) error }); ok {
		return conn.SetWriteBuffer(min(size, quicSendLimit))
	}
	return errors.New("QUIC socket has no send buffer control")
}

func (p *quicPackets) WriteTo(data []byte, addr net.Addr) (int, error) {
	p.writeMu.Lock()
	defer p.writeMu.Unlock()
	if addr.String() != p.RemoteAddr().String() {
		return 0, errors.New("QUIC attempted an unreviewed destination")
	}
	p.mu.Lock()
	if p.sent+uint64(len(data)) > quicSendLimit || p.sentPackets >= 128 {
		p.budgetErr = errors.New("QUIC send budget exhausted")
		err := p.budgetErr
		p.mu.Unlock()
		p.Conn.Close()
		return 0, err
	}
	if p.sentPackets == 0 {
		p.firstSNI = domains.QUIC(data)
	}
	p.mu.Unlock()
	n, err := p.Conn.Write(data)
	p.mu.Lock()
	p.sent += uint64(n)
	p.sentPackets++
	p.mu.Unlock()
	return n, err
}

func (p *quicPackets) ReadFrom(data []byte) (int, net.Addr, error) {
	p.readMu.Lock()
	defer p.readMu.Unlock()
	p.mu.Lock()
	remaining := quicReceiveLimit - p.received
	if remaining == 0 || p.receivedPackets >= 256 {
		p.budgetErr = errors.New("QUIC receive budget exhausted")
		err := p.budgetErr
		p.mu.Unlock()
		p.Conn.Close()
		return 0, nil, err
	}
	p.mu.Unlock()
	if uint64(len(data)) > remaining {
		data = data[:remaining]
	}
	n, err := p.Conn.Read(data)
	p.mu.Lock()
	p.received += uint64(n)
	if n > 0 || err == nil {
		p.receivedPackets++
	}
	p.mu.Unlock()
	return n, p.RemoteAddr(), err
}

func exchangeQUIC(parent context.Context, target, serverName string, fragmented bool, roots *x509.CertPool) (evidence quicEvidence, resultErr error) {
	ctx, cancel := context.WithTimeout(parent, quicRequestTimeout)
	defer cancel()
	started := time.Now()
	evidence.Remote, evidence.FragmentedHello = target, fragmented
	defer func() {
		evidence.Elapsed = time.Since(started)
		if resultErr != nil {
			evidence.Error = resultErr.Error()
		}
	}()
	endpoint, err := netip.ParseAddrPort(target)
	if err != nil || !endpoint.Addr().Is4() {
		return evidence, errors.New("QUIC acceptance requires a frozen IPv4 endpoint")
	}
	conn, err := (&net.Dialer{}).DialContext(ctx, "udp4", target)
	if err != nil {
		return evidence, err
	}
	evidence.Local = conn.LocalAddr().String()
	packets := &quicPackets{Conn: conn}
	transport := &quic.Transport{Conn: packets}
	defer func() {
		transport.Close()
		conn.Close()
		packets.mu.Lock()
		evidence.Sent, evidence.Received = packets.sent, packets.received
		evidence.SentDatagrams, evidence.ReceivedDatagrams = packets.sentPackets, packets.receivedPackets
		evidence.FirstDatagramSNI = packets.firstSNI
		resultErr = errors.Join(resultErr, packets.budgetErr)
		packets.mu.Unlock()
	}()
	stop := context.AfterFunc(ctx, func() { conn.Close() })
	defer stop()
	tlsConfig := &tls.Config{MinVersion: tls.VersionTLS13, ServerName: serverName, NextProtos: []string{http3.NextProtoH3}, CurvePreferences: []tls.CurveID{tls.X25519}}
	if roots != nil {
		tlsConfig.RootCAs = roots
	}
	if fragmented {
		// A hybrid key share makes ClientHello span the fixed-size Initial
		// packets without disabling certificate verification or changing SNI.
		tlsConfig.CurvePreferences = []tls.CurveID{tls.X25519MLKEM768, tls.X25519}
	}
	qconn, err := transport.Dial(ctx, net.UDPAddrFromAddrPort(endpoint), tlsConfig, &quic.Config{
		Versions: []quic.Version{quic.Version1}, HandshakeIdleTimeout: 3 * time.Second, MaxIdleTimeout: quicRequestTimeout,
		InitialPacketSize: 1200, DisablePathMTUDiscovery: true, MaxIncomingStreams: -1, MaxIncomingUniStreams: 3,
		InitialStreamReceiveWindow: 32 << 10, MaxStreamReceiveWindow: 32 << 10,
		InitialConnectionReceiveWindow: 64 << 10, MaxConnectionReceiveWindow: 64 << 10,
	})
	if err != nil {
		return evidence, err
	}
	defer qconn.CloseWithError(0, "")
	state := qconn.ConnectionState().TLS
	evidence.ALPN, evidence.Verified = state.NegotiatedProtocol, len(state.VerifiedChains) > 0
	if evidence.ALPN != http3.NextProtoH3 || !evidence.Verified {
		return evidence, errors.New("QUIC did not establish verified HTTP/3 TLS")
	}
	var nonce [16]byte
	if _, err := rand.Read(nonce[:]); err != nil {
		return evidence, err
	}
	evidence.Nonce = hex.EncodeToString(nonce[:])
	host := serverName
	if endpoint.Port() != 443 {
		host = net.JoinHostPort(host, fmt.Sprint(endpoint.Port()))
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "https://"+host+"/", nil)
	if err != nil {
		return evidence, err
	}
	req.Header.Set("User-Agent", "Pith-rootcheck")
	req.Header.Set("X-Pith-Acceptance", evidence.Nonce)
	// A direct ClientConn performs one RoundTrip, without Transport's retry
	// branch or http.Client's redirect handling. Every case owns one connection.
	h3 := (&http3.Transport{MaxResponseHeaderBytes: 16 << 10, DisableCompression: true}).NewClientConn(qconn)
	response, err := h3.RoundTrip(req)
	if err != nil {
		return evidence, err
	}
	defer response.Body.Close()
	evidence.StatusCode, evidence.Protocol = response.StatusCode, response.Proto
	if response.ProtoMajor != 3 || response.StatusCode < 200 || response.StatusCode >= 400 {
		return evidence, fmt.Errorf("unexpected HTTP/3 response: %s %d", response.Proto, response.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(response.Body, quicBodyLimit))
	evidence.BodyBytes, evidence.BodyLimited = len(body), len(body) == quicBodyLimit
	digest := sha256.Sum256(body)
	evidence.BodyPrefixSHA256 = hex.EncodeToString(digest[:])
	return evidence, err
}
