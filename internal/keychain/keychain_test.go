package keychain

import (
	"context"
	"errors"
	"slices"
	"testing"
)

// fake replaces security: it keeps items in memory and records the calls.
func fake(t *testing.T) *[][]string {
	t.Helper()
	items := map[string]string{}
	var calls [][]string
	old := run
	run = func(_ context.Context, args ...string) ([]byte, int, error) {
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
		case "add-generic-password":
			items[key] = flag("-w")
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
	if !slices.Contains(set, "-U") || !slices.Contains(set, "/usr/bin/security") {
		t.Fatalf("Set must update in place and trust only security: %v", set)
	}
}
