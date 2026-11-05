package main

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"errors"
	"fmt"
	"math/big"
	"net"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	quic "github.com/quic-go/quic-go"
	"github.com/quic-go/quic-go/http3"
)

func quicLoopback(t *testing.T, handler http.Handler) (string, *x509.CertPool) {
	t.Helper()
	public, private, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	template := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "example.com"}, DNSNames: []string{"example.com"}, NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour), KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}, BasicConstraintsValid: true}
	der, err := x509.CreateCertificate(rand.Reader, template, template, public, private)
	if err != nil {
		t.Fatal(err)
	}
	certificate, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatal(err)
	}
	pool := x509.NewCertPool()
	pool.AddCert(certificate)
	conn, err := net.ListenPacket("udp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	server := &http3.Server{TLSConfig: &tls.Config{Certificates: []tls.Certificate{{Certificate: [][]byte{der}, PrivateKey: private}}}, QUICConfig: &quic.Config{Versions: []quic.Version{quic.Version1}, MaxIdleTimeout: 2 * time.Second}, Handler: handler}
	done := make(chan error, 1)
	go func() { done <- server.Serve(conn) }()
	t.Cleanup(func() {
		server.Close()
		conn.Close()
		if err := <-done; err != nil && !errors.Is(err, http.ErrServerClosed) && !errors.Is(err, net.ErrClosed) {
			t.Error(err)
		}
	})
	return conn.LocalAddr().String(), pool
}

func TestQUICVerifiedHTTP3WithObservableAndFragmentedHello(t *testing.T) {
	for _, fragmented := range []bool{false, true} {
		t.Run(fmt.Sprint(fragmented), func(t *testing.T) {
			var requests atomic.Int32
			seen := make(chan string, 1)
			target, pool := quicLoopback(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				requests.Add(1)
				select {
				case seen <- r.Header.Get("X-Pith-Acceptance"):
				default:
					t.Error("unexpected second HTTP request")
				}
				if r.ProtoMajor != 3 || r.TLS.ServerName != "example.com" || r.URL.Path != "/" {
					t.Error("unexpected HTTP/3 request", r.Proto, r.TLS.ServerName, r.URL.Path)
				}
				fmt.Fprint(w, "bounded HTTP/3 response")
			}))
			evidence, err := exchangeQUIC(context.Background(), target, "example.com", fragmented, pool)
			if err != nil {
				t.Fatal(err, evidence)
			}
			if !evidence.Verified || evidence.Protocol != "HTTP/3.0" || evidence.ALPN != "h3" || evidence.StatusCode != 200 || evidence.BodyBytes == 0 || evidence.Sent == 0 || evidence.Received == 0 {
				t.Fatal(evidence)
			}
			if evidence.Sent > quicSendLimit || evidence.Received > quicReceiveLimit || requests.Load() != 1 || <-seen != evidence.Nonce {
				t.Fatal("request/counter mismatch", evidence)
			}
			if fragmented && evidence.FirstDatagramSNI != "" || !fragmented && evidence.FirstDatagramSNI != "example.com" {
				t.Fatal("fixture did not produce its intended Initial", evidence)
			}
		})
	}
}

func TestQUICNoRedirectRetryOrUnboundedBody(t *testing.T) {
	for _, status := range []int{http.StatusFound, http.StatusOK, http.StatusServiceUnavailable} {
		t.Run(fmt.Sprint(status), func(t *testing.T) {
			var requests atomic.Int32
			target, pool := quicLoopback(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				requests.Add(1)
				w.Header().Set("Location", "https://other.example.com/")
				w.WriteHeader(status)
				if status == http.StatusOK {
					fmt.Fprint(w, strings.Repeat("x", quicBodyLimit+1))
				}
			}))
			evidence, err := exchangeQUIC(context.Background(), target, "example.com", false, pool)
			if (err != nil) != (status == http.StatusServiceUnavailable) {
				t.Fatal(evidence, err)
			}
			if requests.Load() != 1 {
				t.Fatal("followed redirect or retried request")
			}
			if status == http.StatusOK && (evidence.BodyBytes != quicBodyLimit || !evidence.BodyLimited) {
				t.Fatal("unbounded body", evidence)
			}
		})
	}
}

func TestQUICRejectsWrongCertificateAndCancellation(t *testing.T) {
	var requests atomic.Int32
	target, pool := quicLoopback(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { requests.Add(1) }))
	if _, err := exchangeQUIC(context.Background(), target, "other.example.com", false, pool); err == nil {
		t.Fatal("accepted wrong certificate name")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := exchangeQUIC(ctx, target, "example.com", false, pool); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	if requests.Load() != 0 {
		t.Fatal("sent request before certificate/context validation")
	}
}

func TestQUICPacketLimitsAndDestinationPin(t *testing.T) {
	_, c, _, _ := dnsFixture(t)
	conn := &dnsConn{s: c, local: &net.UDPAddr{IP: net.ParseIP("192.0.2.10"), Port: 50000}, remote: &net.UDPAddr{IP: net.ParseIP("192.0.2.20"), Port: 443}, row: -1}
	packets := &quicPackets{Conn: conn, sent: quicSendLimit - 1}
	if _, err := packets.WriteTo([]byte{1}, &net.UDPAddr{IP: net.ParseIP("198.51.100.20"), Port: 443}); err == nil {
		t.Fatal("allowed migration outside frozen target")
	}
	if _, err := packets.WriteTo([]byte{1, 2}, conn.RemoteAddr()); err == nil || !conn.closed {
		t.Fatal("send cap did not close socket")
	}
	conn.closed = false
	packets.received = quicReceiveLimit
	if _, _, err := packets.ReadFrom(make([]byte, 1200)); err == nil || !conn.closed {
		t.Fatal("receive cap did not close socket")
	}
}

type quicCountConn struct {
	*dnsConn
	count int
}

func (c *quicCountConn) Write(data []byte) (int, error) { c.count += len(data); return len(data), nil }

func TestQUICConcurrentWritersCannotExceedBudget(t *testing.T) {
	conn := &quicCountConn{dnsConn: &dnsConn{remote: &net.UDPAddr{IP: net.ParseIP("192.0.2.20"), Port: 443}}}
	packets := &quicPackets{Conn: conn}
	var workers sync.WaitGroup
	for range 8 {
		workers.Go(func() {
			for range 8 {
				_, _ = packets.WriteTo(make([]byte, 4096), conn.RemoteAddr())
			}
		})
	}
	workers.Wait()
	if conn.count != quicSendLimit || packets.sent != quicSendLimit || packets.budgetErr == nil {
		t.Fatal("concurrent writes escaped the send cap", conn.count, packets.sent, packets.budgetErr)
	}
}
