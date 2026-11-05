package main

import (
	"context"
	"errors"
	"net"
	"slices"
	"testing"
	"time"

	"golang.org/x/net/dns/dnsmessage"
)

func dnsResponseFixture(id uint16, kind dnsmessage.Type) dnsmessage.Message {
	name := dnsmessage.MustNewName("example.com.")
	var body dnsmessage.ResourceBody = &dnsmessage.AResource{A: [4]byte{192, 0, 2, 10}}
	if kind == dnsmessage.TypeAAAA {
		body = &dnsmessage.AAAAResource{AAAA: [16]byte{0x20, 0x01, 0x0d, 0xb8, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 10}}
	}
	return dnsmessage.Message{Header: dnsmessage.Header{ID: id, Response: true, RecursionAvailable: true},
		Questions: []dnsmessage.Question{{Name: name, Type: kind, Class: dnsmessage.ClassINET}},
		Answers:   []dnsmessage.Resource{{Header: dnsmessage.ResourceHeader{Name: name, Type: kind, Class: dnsmessage.ClassINET, TTL: 123}, Body: body}}}
}

func TestDNSResponseRequiresMatchingNonceQuestionAndAnswer(t *testing.T) {
	for _, kind := range []dnsmessage.Type{dnsmessage.TypeA, dnsmessage.TypeAAAA} {
		message := dnsResponseFixture(42, kind)
		wire, err := message.Pack()
		if err != nil {
			t.Fatal(err)
		}
		answers, err := parseDNSResponse(wire, 42, message.Questions[0])
		if err != nil || len(answers) != 1 || answers[0].TTL != 123 || answers[0].Type != kind.String() {
			t.Fatalf("answers=%+v error=%v", answers, err)
		}
	}
	for _, name := range []string{"id", "response", "opcode", "truncated", "rcode", "question-name", "question-type", "question-class", "extra-question", "unrelated-answer", "empty", "garbage", "oversize", "cname-loop"} {
		t.Run(name, func(t *testing.T) {
			m := dnsResponseFixture(42, dnsmessage.TypeA)
			q := m.Questions[0]
			switch name {
			case "id":
				m.ID++
			case "response":
				m.Response = false
			case "opcode":
				m.OpCode = 1
			case "truncated":
				m.Truncated = true
			case "rcode":
				m.RCode = dnsmessage.RCodeNameError
			case "question-name":
				m.Questions[0].Name = dnsmessage.MustNewName("other.example.com.")
			case "question-type":
				m.Questions[0].Type = dnsmessage.TypeAAAA
			case "question-class":
				m.Questions[0].Class = dnsmessage.ClassCHAOS
			case "extra-question":
				m.Questions = append(m.Questions, q)
			case "unrelated-answer":
				m.Answers[0].Header.Name = dnsmessage.MustNewName("other.example.com.")
			case "empty":
				m.Answers = nil
			case "cname-loop":
				m.Answers = []dnsmessage.Resource{{Header: dnsmessage.ResourceHeader{Name: q.Name, Type: dnsmessage.TypeCNAME, Class: dnsmessage.ClassINET}, Body: &dnsmessage.CNAMEResource{CNAME: q.Name}}}
			}
			wire, err := m.Pack()
			if err != nil {
				t.Fatal(err)
			}
			if name == "garbage" {
				wire = wire[:5]
			}
			if name == "oversize" {
				wire = make([]byte, 513)
			}
			if _, err := parseDNSResponse(wire, 42, q); err == nil {
				t.Fatal("accepted invalid DNS evidence")
			}
		})
	}
}

func TestDNSResponseCNAMEChain(t *testing.T) {
	m := dnsResponseFixture(42, dnsmessage.TypeA)
	alias := dnsmessage.MustNewName("alias.example.com.")
	m.Answers[0].Header.Name = alias
	m.Answers = append(m.Answers, dnsmessage.Resource{Header: dnsmessage.ResourceHeader{Name: m.Questions[0].Name, Type: dnsmessage.TypeCNAME, Class: dnsmessage.ClassINET, TTL: 45}, Body: &dnsmessage.CNAMEResource{CNAME: alias}})
	wire, _ := m.Pack()
	answers, err := parseDNSResponse(wire, 42, m.Questions[0])
	if err != nil || len(answers) != 2 || answers[0].TTL != 45 || answers[1].Value != "192.0.2.10" {
		t.Fatalf("answers=%+v err=%v", answers, err)
	}
}

func TestDNSExchangeLoopbackReusesPortAndChangesNonce(t *testing.T) {
	server, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.ParseIP("127.0.0.1")})
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	var serverErr error
	var ports []int
	go func() {
		defer close(done)
		_ = server.SetDeadline(time.Now().Add(3 * time.Second))
		for range 2 {
			buf := make([]byte, 512)
			n, source, err := server.ReadFromUDP(buf)
			if err != nil {
				serverErr = err
				return
			}
			ports = append(ports, source.Port)
			var query dnsmessage.Message
			if err := query.Unpack(buf[:n]); err != nil {
				serverErr = err
				return
			}
			response := dnsResponseFixture(query.ID, query.Questions[0].Type)
			wire, _ := response.Pack()
			if _, err := server.WriteToUDP(wire, source); err != nil {
				serverErr = err
				return
			}
		}
	}()
	t.Cleanup(func() {
		server.Close()
		<-done
		if serverErr != nil && !errors.Is(serverErr, net.ErrClosed) {
			t.Error(serverErr)
		}
	})
	conn, err := net.Dial("udp4", server.LocalAddr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	sequence := dnsSequence{next: 65535}
	first, err := sequence.exchange(context.Background(), conn, "example.com", dnsmessage.TypeA)
	if err != nil {
		t.Fatal(err)
	}
	second, err := sequence.exchange(context.Background(), conn, "example.com", dnsmessage.TypeAAAA)
	if err != nil {
		t.Fatal(err)
	}
	if first.ID == second.ID || first.Local != second.Local || first.QueryBytes == 0 || second.ResponseBytes == 0 || !slices.EqualFunc(first.Answers, second.Answers, func(a, b dnsAnswer) bool { return a.Name == b.Name }) {
		t.Fatal(first, second)
	}
	// The same application's local port is also observed at this controlled
	// loopback server. Public resolver evidence cannot establish a NAT-side port.
	server.Close()
	<-done
	if serverErr != nil {
		t.Fatal(serverErr)
	}
	if len(ports) != 2 || ports[0] != ports[1] {
		t.Fatal(ports)
	}
}

func TestDNSExchangeCancellationHasNoRetry(t *testing.T) {
	a, b := net.Pipe()
	defer a.Close()
	defer b.Close()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	sequence := dnsSequence{}
	evidence, err := sequence.exchange(ctx, a, "example.com", dnsmessage.TypeA)
	if !errors.Is(err, context.Canceled) || evidence.QueryBytes != 0 {
		t.Fatal(evidence, err)
	}
}
