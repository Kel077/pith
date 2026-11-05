package keychain

import (
	"bytes"
	"context"
	"errors"
	"slices"
	"strings"
	"testing"
)

// fake replaces security: it keeps items in memory and records the calls.
func fake(t *testing.T) *[][]string {
	t.Helper()
	items := map[string]string{}
	var calls [][]string
	old := run
	run = func(_ context.Context, input []byte, args ...string) ([]byte, int, error) {
		calls = append(calls, args)
		flag := func(name string) string {
			i := slices.Index(args, name)
			if i < 0 || i+1 >= len(args) {
				return ""
			}
			return args[i+1]
		}
		key := flag("-s") + "/" + flag("-a")
		switch args[0] {
		case "find-generic-password":
			secret, ok := items[key]
			if !ok {
				return nil, notFound, nil
			}
			return []byte(secret + "\n"), 0, nil
		case "-i":
			want := `"add-generic-password" "-s" "example.service" "-a" "example" "-T" "/usr/bin/security" "-U" "-w" "s3cret value"` + "\n"
			if string(input) != want {
				t.Fatal("incorrect secret input")
			}
			items["example.service/example"] = "s3cret value"
			return nil, 0, nil
		}
		return nil, 1, nil
	}
	t.Cleanup(func() { run = old })
	return &calls
}

func TestSetAndGet(t *testing.T) {
	calls := fake(t)
	ctx := context.Background()
	if _, err := Get(ctx, "example.service", "example"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("Get before Set: %v", err)
	}
	if err := Set(ctx, "example.service", "example", "s3cret value"); err != nil {
		t.Fatal(err)
	}
	if got, err := Get(ctx, "example.service", "example"); err != nil || got != "s3cret value" {
		t.Fatalf("Get = %q, %v", got, err)
	}
	set := (*calls)[1]
	if !slices.Equal(set, []string{"-i"}) {
		t.Fatalf("Set must keep secret values out of process arguments: %v", set)
	}
}

func TestSetInputQuotesWithoutAddingCommands(t *testing.T) {
	data, err := setInput("example.service", "example", `quote" slash\ $() '`)
	if err != nil {
		t.Fatal(err)
	}
	want := `"add-generic-password" "-s" "example.service" "-a" "example" "-T" "/usr/bin/security" "-U" "-w" "quote\" slash\\ $() '"` + "\n"
	if string(data) != want {
		t.Fatalf("unexpected input %q", data)
	}
	for _, value := range []string{"line\nbreak", "line\rbreak", "nul\x00", strings.Repeat("x", 4096)} {
		if _, err := setInput("service", "account", value); err == nil {
			t.Fatal("unsafe command input accepted")
		}
	}
}

func TestSetClearsSecretInputAndDiscardsSubprocessErrors(t *testing.T) {
	old := run
	defer func() { run = old }()
	var input []byte
	run = func(_ context.Context, data []byte, args ...string) ([]byte, int, error) {
		input = data
		return nil, 0, errors.New("synthetic-private-value")
	}
	err := Set(context.Background(), "example.service", "example", "synthetic-private-value")
	if err == nil || strings.Contains(err.Error(), "synthetic-private-value") {
		t.Fatal("unredacted error")
	}
	if len(input) == 0 || bytes.Count(input, []byte{0}) != len(input) {
		t.Fatal("stdin buffer was not cleared")
	}
	run = func(context.Context, []byte, ...string) ([]byte, int, error) {
		t.Fatal("invalid input executed security")
		return nil, 0, nil
	}
	if Set(context.Background(), "example.service", "example", "line\nbreak") == nil {
		t.Fatal("newline accepted")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if !errors.Is(Set(ctx, "example.service", "example", "value"), context.Canceled) {
		t.Fatal("cancellation lost")
	}
}
