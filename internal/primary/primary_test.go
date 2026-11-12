package primary

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"
)

// fake is a scutil session over a made-up store: a service of en0 and one of
// en9, both with a router.
type fake struct {
	store   dict
	primary []string // what State:/Network/Global/IPv4 says, in turn
	addErr  string   // what add prints, if it fails
	sent    []string
	added   bool
	closed  bool
	ended   chan struct{}
}

func newFake() *fake {
	return &fake{
		store: dict{
			"State:/Network/Global/IPv4":                                            dict{"PrimaryInterface": "en0"},
			"State:/Network/Service/11111111-1111-1111-1111-111111111111/IPv4":      dict{"InterfaceName": "en0", "Router": "192.0.2.1"},
			"State:/Network/Service/22222222-2222-2222-2222-222222222222/IPv4":      dict{"InterfaceName": "en9", "Router": "198.51.100.1"},
			"Setup:/Network/Service/11111111-1111-1111-1111-111111111111/Interface": dict{"DeviceName": "en0"},
		},
		primary: []string{"en0", "en0", "en9"},
		ended:   make(chan struct{}),
	}
}

func (f *fake) show(_ context.Context, key string, pattern bool) (dict, error) {
	switch {
	case pattern:
		return f.store, nil
	case key == "State:/Network/Global/IPv4":
		p := f.primary[0]
		if len(f.primary) > 1 {
			f.primary = f.primary[1:]
		}
		return dict{"PrimaryInterface": p}, nil
	case f.addErr != "":
		return nil, errors.New("primary: scutil: " + f.addErr)
	case f.added:
		return dict{"PrimaryRank": "Never"}, nil
	}
	return nil, nil
}

func (f *fake) send(commands ...string) error {
	f.sent = append(f.sent, commands...)
	for _, c := range commands {
		f.added = f.added || strings.HasPrefix(c, "add ")
	}
	return nil
}

func (f *fake) close() error          { f.closed = true; return nil }
func (f *fake) done() <-chan struct{} { return f.ended }

func use(t *testing.T, f *fake) {
	t.Helper()
	old, oldPoll, oldSwitch := newSession, poll, switchTime
	newSession = func(context.Context) (session, error) { return f, nil }
	poll, switchTime = time.Millisecond, 50*time.Millisecond
	t.Cleanup(func() { newSession, poll, switchTime = old, oldPoll, oldSwitch })
}

func TestYield(t *testing.T) {
	f := newFake()
	use(t, f)
	ctx, cancel := context.WithCancel(context.Background())
	var got string
	err := Yield(ctx, "en0", func(p string) { got = p; cancel() })
	if err != nil || got != "en9" {
		t.Fatalf("Yield = %v, primary %q", err, got)
	}
	want := []string{"d.init", "d.add PrimaryRank Never", "add State:/Network/Service/11111111-1111-1111-1111-111111111111 temporary"}
	if !slices.Equal(f.sent, want) || !f.closed {
		t.Fatalf("sent %q, closed %v", f.sent, f.closed)
	}
}

func TestYieldChangesNothingWithoutAnotherRouter(t *testing.T) {
	f := newFake()
	delete(f.store, "State:/Network/Service/22222222-2222-2222-2222-222222222222/IPv4")
	use(t, f)
	if err := Yield(context.Background(), "en0", func(string) { t.Fatal("ready") }); err == nil || len(f.sent) != 0 || !f.closed {
		t.Fatalf("Yield = %v, sent %q", err, f.sent)
	}
}

func TestYieldLeavesAnotherStateAlone(t *testing.T) {
	f := newFake()
	f.store["State:/Network/Service/11111111-1111-1111-1111-111111111111"] = dict{"PrimaryRank": "First"}
	use(t, f)
	if err := Yield(context.Background(), "en0", func(string) { t.Fatal("ready") }); err == nil || len(f.sent) != 0 {
		t.Fatalf("Yield = %v, sent %q", err, f.sent)
	}
}

func TestYieldRefusesUnknownInterfaces(t *testing.T) {
	for _, name := range []string{"", "en0 temporary", "en0\nremove", "utun9", "EN0"} {
		f := newFake()
		use(t, f)
		if err := Yield(context.Background(), name, func(string) { t.Fatal("ready") }); err == nil || len(f.sent) != 0 {
			t.Fatalf("Yield(%q) = %v, sent %q", name, err, f.sent)
		}
	}
}

func TestYieldFailsWhenAddFails(t *testing.T) {
	f := newFake()
	f.addErr = "Permission denied"
	use(t, f)
	if err := Yield(context.Background(), "en0", func(string) { t.Fatal("ready") }); err == nil || !strings.Contains(err.Error(), "Permission denied") || !f.closed {
		t.Fatalf("Yield = %v", err)
	}
}

func TestYieldGivesUpWhenMacOSKeepsThePrimary(t *testing.T) {
	f := newFake()
	f.primary = []string{"en0"}
	use(t, f)
	if err := Yield(context.Background(), "en0", func(string) { t.Fatal("ready") }); err == nil || !f.closed {
		t.Fatalf("Yield = %v, closed %v", err, f.closed)
	}
}

func TestYieldReportsAnEarlyEnd(t *testing.T) {
	f := newFake()
	use(t, f)
	err := Yield(context.Background(), "en0", func(string) { close(f.ended) })
	if err == nil || !strings.Contains(err.Error(), "primary again") {
		t.Fatalf("Yield = %v", err)
	}
}

// The real scutil, read only: it shows the global IPv4 state, or no such key
// on a Mac without a primary interface.
func TestScutilShows(t *testing.T) {
	s, err := startScutil(context.Background())
	if err != nil {
		t.Skip("no scutil:", err)
	}
	defer s.close()
	for range 2 {
		if _, err := s.show(context.Background(), "State:/Network/Global/IPv4", false); err != nil {
			t.Fatal(err)
		}
	}
	if d, err := s.show(context.Background(), "State:/Network/Pith/NoSuchKey", false); err != nil || d != nil {
		t.Fatalf("a missing key: %v, %v", d, err)
	}
	if err := s.send("d.init", "d.show"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.show(context.Background(), keys, true); err != nil {
		t.Fatal(err)
	}
	if err := s.close(); err != nil {
		t.Fatal(err)
	}
	select {
	case <-s.done():
	default:
		t.Fatal("scutil still runs")
	}
}
