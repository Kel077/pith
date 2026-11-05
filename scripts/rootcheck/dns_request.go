package main

import (
	"context"
	"crypto/rand"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"net/netip"
	"strings"
	"time"

	"golang.org/x/net/dns/dnsmessage"
)

const dnsResponseTimeout = 2 * time.Second

// This stage deliberately uses classic DNS without EDNS, retransmission, or a
// TCP fallback. Every exchange sends one small query to the frozen resolver.
type dnsEvidence struct {
	ID            uint16        `json:"id"`
	Name          string        `json:"name"`
	Type          string        `json:"type"`
	Local         string        `json:"local"`
	Remote        string        `json:"remote"`
	QueryBytes    int           `json:"queryBytes"`
	ResponseBytes int           `json:"responseBytes"`
	Elapsed       time.Duration `json:"elapsed"`
	Answers       []dnsAnswer   `json:"answers,omitempty"`
	Error         string        `json:"error,omitempty"`
}

type dnsAnswer struct {
	Name  string `json:"name"`
	Type  string `json:"type"`
	Value string `json:"value"`
	TTL   uint32 `json:"ttl"`
}

// A random starting ID followed by a bounded sequence makes every query in the
// command distinct, including two questions on the same source port.
type dnsSequence struct{ next uint16 }

func newDNSSequence() (*dnsSequence, error) {
	var seed [2]byte
	if _, err := rand.Read(seed[:]); err != nil {
		return nil, err
	}
	return &dnsSequence{next: binary.BigEndian.Uint16(seed[:])}, nil
}

func (s *dnsSequence) exchange(ctx context.Context, conn net.Conn, name string, kind dnsmessage.Type) (evidence dnsEvidence, resultErr error) {
	evidence = dnsEvidence{ID: s.next, Name: name, Type: kind.String(), Local: conn.LocalAddr().String(), Remote: conn.RemoteAddr().String()}
	s.next++
	started := time.Now()
	defer func() {
		evidence.Elapsed = time.Since(started)
		if resultErr != nil {
			evidence.Error = resultErr.Error()
		}
	}()
	if kind != dnsmessage.TypeA && kind != dnsmessage.TypeAAAA {
		return evidence, errors.New("DNS acceptance only queries A and AAAA")
	}
	dnsName, err := dnsmessage.NewName(name + ".")
	if err != nil {
		return evidence, err
	}
	question := dnsmessage.Question{Name: dnsName, Type: kind, Class: dnsmessage.ClassINET}
	query, err := (&dnsmessage.Message{Header: dnsmessage.Header{ID: evidence.ID, RecursionDesired: true}, Questions: []dnsmessage.Question{question}}).Pack()
	if err != nil || len(query) > 512 {
		return evidence, errors.New("cannot encode a bounded DNS question")
	}
	if err := ctx.Err(); err != nil {
		return evidence, err
	}
	deadline := time.Now().Add(dnsResponseTimeout)
	if end, ok := ctx.Deadline(); ok && end.Before(deadline) {
		deadline = end
	}
	if err := conn.SetDeadline(deadline); err != nil {
		return evidence, err
	}
	// Closing on cancellation interrupts both a pending UDP read and write. No
	// goroutine survives the exchange or holds an open fixture socket afterward.
	stop := context.AfterFunc(ctx, func() { conn.Close() })
	defer stop()
	evidence.QueryBytes, err = conn.Write(query)
	if err != nil {
		return evidence, err
	}
	if evidence.QueryBytes != len(query) {
		return evidence, io.ErrShortWrite
	}
	response := make([]byte, 513)
	evidence.ResponseBytes, err = conn.Read(response)
	if err != nil {
		return evidence, err
	}
	evidence.Answers, err = parseDNSResponse(response[:evidence.ResponseBytes], evidence.ID, question)
	return evidence, err
}

func parseDNSResponse(wire []byte, id uint16, question dnsmessage.Question) ([]dnsAnswer, error) {
	if len(wire) > 512 {
		return nil, errors.New("DNS response exceeds the reviewed classic DNS budget")
	}
	var message dnsmessage.Message
	if err := message.Unpack(wire); err != nil {
		return nil, fmt.Errorf("invalid DNS response: %w", err)
	}
	if message.ID != id || !message.Response || message.OpCode != 0 || message.Truncated || message.RCode != dnsmessage.RCodeSuccess {
		return nil, errors.New("DNS response ID, flags, truncation or result does not match the query")
	}
	if len(message.Questions) != 1 || message.Questions[0].Class != question.Class || message.Questions[0].Type != question.Type || !strings.EqualFold(message.Questions[0].Name.String(), question.Name.String()) {
		return nil, errors.New("DNS response question does not match the query")
	}
	// Accept only addresses for this question or its bounded CNAME chain. An
	// unrelated answer or additional glue cannot turn an empty response into a pass.
	name := strings.ToLower(question.Name.String())
	seen := map[string]bool{}
	var answers []dnsAnswer
	for range 9 {
		if seen[name] {
			return nil, errors.New("DNS answer has a CNAME loop")
		}
		seen[name] = true
		var next string
		addresses := 0
		for _, answer := range message.Answers {
			if answer.Header.Class != dnsmessage.ClassINET || strings.ToLower(answer.Header.Name.String()) != name {
				continue
			}
			entry := dnsAnswer{Name: name, Type: answer.Header.Type.String(), TTL: answer.Header.TTL}
			switch body := answer.Body.(type) {
			case *dnsmessage.CNAMEResource:
				candidate := strings.ToLower(body.CNAME.String())
				if next != "" && next != candidate {
					return nil, errors.New("DNS answer has ambiguous CNAME targets")
				}
				next, entry.Value = candidate, candidate
			case *dnsmessage.AResource:
				if question.Type != dnsmessage.TypeA {
					continue
				}
				entry.Value = netip.AddrFrom4(body.A).String()
				addresses++
			case *dnsmessage.AAAAResource:
				if question.Type != dnsmessage.TypeAAAA {
					continue
				}
				entry.Value = netip.AddrFrom16(body.AAAA).String()
				addresses++
			default:
				continue
			}
			answers = append(answers, entry)
		}
		if addresses > 0 {
			if next != "" {
				return nil, errors.New("DNS answer mixes CNAME and address records for one owner")
			}
			return answers, nil
		}
		if next == "" {
			return nil, errors.New("DNS response has no address answer for the requested name and type")
		}
		name = next
	}
	return nil, errors.New("DNS CNAME chain exceeds the bounded acceptance limit")
}
