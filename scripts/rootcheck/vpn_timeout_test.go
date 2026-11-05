package main

import (
	"context"
	"errors"
	"net"
	"strings"
	"testing"
	"time"

	"golang.org/x/net/dns/dnsmessage"
)

// Delay only the kernel deadline to deterministically exercise the other side
// of the deadline/Close race on a real UDP socket, without a fake Read result.
type closeFirstDNSConn struct{ net.Conn }

func (c closeFirstDNSConn) SetDeadline(at time.Time) error {
	return c.Conn.SetDeadline(at.Add(time.Second))
}

func TestVPNWithdrawalTimeoutOnUnansweredLoopbackUDP(t *testing.T) {
	for _, closeFirst := range []bool{false, true} {
		name := "socket-deadline"
		if closeFirst {
			name = "context-close"
		}
		t.Run(name, func(t *testing.T) {
			listener, err := net.ListenPacket("udp4", "127.0.0.1:0")
			if err != nil {
				t.Fatal(err)
			}
			defer listener.Close()
			s := vpnFixture(t)
			s.VPNRestart.DNS = listener.LocalAddr().String()
			c := newVPNService(t, s)
			deps := liveVPNChecks(nil)
			if closeFirst {
				deps.dns = func(parent context.Context, p *vpnRestartPlan) (dnsEvidence, error) {
					query, cancel := context.WithTimeoutCause(parent, dnsResponseTimeout, errVPNDNSDeadline)
					defer cancel()
					conn, err := (&net.Dialer{}).DialContext(query, "udp4", p.DNS)
					if err != nil {
						return dnsEvidence{}, err
					}
					defer conn.Close()
					return exchangeVPNDNS(parent, query, closeFirstDNSConn{conn}, p)
				}
			}
			ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
			defer cancel()
			evidence := &vpnRestartEvidence{}
			if err := queryVPN(ctx, s, c, vpnTunnel{}, evidence, deps, "withdrawn", true); err != nil {
				t.Fatal(err)
			}
			exchange := evidence.Queries[0].Exchange
			if exchange.ResponseBytes != 0 || exchange.QueryBytes <= 0 || exchange.Error == "" || exchange.Elapsed < dnsResponseTimeout/2 || exchange.Elapsed > 5*time.Second {
				t.Fatal(exchange)
			}
			if closeFirst && !strings.Contains(exchange.Error, net.ErrClosed.Error()) {
				t.Fatalf("original socket close diagnostic lost: %s", exchange.Error)
			}
			// The receiver deliberately never answered. Check the actual one-query
			// datagram afterward; there is no server goroutine to leave running.
			if err := listener.SetReadDeadline(time.Now().Add(time.Second)); err != nil {
				t.Fatal(err)
			}
			wire := make([]byte, 513)
			n, from, err := listener.ReadFrom(wire)
			if err != nil || n != exchange.QueryBytes || from.String() != exchange.Local {
				t.Fatal(n, from, err, exchange)
			}
			var question dnsmessage.Message
			if err := question.Unpack(wire[:n]); err != nil || question.ID != exchange.ID || len(question.Questions) != 1 || question.Questions[0].Name.String() != s.VPNRestart.OwnName+"." {
				t.Fatal(question, err)
			}
			if err := listener.SetReadDeadline(time.Now().Add(10 * time.Millisecond)); err != nil {
				t.Fatal(err)
			}
			if _, _, err := listener.ReadFrom(wire); err == nil {
				t.Fatal("withdrawal query was retransmitted")
			}
		})
	}
}

func TestVPNClosedSocketRequiresOwnDeadline(t *testing.T) {
	parent := context.Background()
	query, cancel := context.WithTimeoutCause(parent, -time.Second, errVPNDNSDeadline)
	defer cancel()
	if err := vpnDNSError(parent, query, net.ErrClosed); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatal(err)
	}
	if err := vpnDNSError(parent, parent, net.ErrClosed); !errors.Is(err, net.ErrClosed) {
		t.Fatal("unrelated close became a timeout", err)
	}
	otherDeadline, stop := context.WithTimeout(parent, -time.Second)
	defer stop()
	if err := vpnDNSError(parent, otherDeadline, net.ErrClosed); !errors.Is(err, net.ErrClosed) {
		t.Fatal("another context's deadline became query timeout", err)
	}
	canceled, stop := context.WithCancel(parent)
	stop()
	if err := vpnDNSError(canceled, query, net.ErrClosed); !errors.Is(err, context.Canceled) {
		t.Fatal("parent cancellation became query timeout", err)
	}
	if err := vpnDNSError(otherDeadline, query, net.ErrClosed); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatal("parent error not retained", err)
	}
	if err := vpnDNSError(parent, query, errors.New("another I/O failure")); err == nil || err.Error() != "another I/O failure" {
		t.Fatal("non-close I/O error hidden", err)
	}
	shortParent, stop := context.WithTimeout(parent, time.Second)
	defer stop()
	shortQuery, stop := context.WithTimeoutCause(shortParent, dnsResponseTimeout, errVPNDNSDeadline)
	defer stop()
	if err := vpnDNSError(shortParent, shortQuery, context.DeadlineExceeded); !errors.Is(err, errVPNInconclusive) {
		t.Fatal("kernel timeout before the parent timer fired accepted", err)
	}
}

func TestVPNWithdrawalRejectsParentDeadline(t *testing.T) {
	s := vpnFixture(t)
	c := newVPNService(t, s)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	deps := liveVPNChecks(nil)
	deps.dns = func(context.Context, *vpnRestartPlan) (dnsEvidence, error) {
		answer := vpnAnswer(s)
		answer.ResponseBytes = 0
		cancel()
		return answer, context.DeadlineExceeded
	}
	if err := queryVPN(ctx, s, c, vpnTunnel{}, &vpnRestartEvidence{}, deps, "withdrawn", true); !errors.Is(err, context.Canceled) {
		t.Fatal("canceled stage accepted negative evidence", err)
	}
}

func TestVPNEndReminderIsOnceAndOnlyAfterReady(t *testing.T) {
	var prompts []string
	deps := vpnRestartDeps{prompt: func(line string) { prompts = append(prompts, line) }}
	evidence := &vpnRestartEvidence{}
	remindVPNReconnect(nil, deps)
	remindVPNReconnect(evidence, deps)
	if len(prompts) != 0 {
		t.Fatal(prompts)
	}
	evidence.ReadyAt = time.Now()
	remindVPNReconnect(evidence, deps)
	remindVPNReconnect(evidence, deps)
	if len(prompts) != 1 || prompts[0] != "RECONNECT: test ended; turn the VPN back on now" || evidence.RestoreReminderAt.IsZero() {
		t.Fatal(prompts, evidence)
	}
}
