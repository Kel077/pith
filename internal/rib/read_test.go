package rib

import (
	"context"
	"errors"
	"os"
	"syscall"
	"testing"
	"time"
)

func TestReadGrowthRetriesWholeSnapshot(t *testing.T) {
	reads, sleeps := 0, 0
	previous := 0
	data, err := read(context.Background(), func(buffer []byte) (uint64, error) {
		if buffer == nil {
			return 100, nil
		}
		reads++
		if len(buffer) <= previous || len(buffer) < 100+(16<<10) {
			t.Fatal("no growth headroom", len(buffer), previous)
		}
		previous = len(buffer)
		if reads <= 4 {
			buffer[0] = 99
			return uint64(len(buffer)), os.NewSyscallError("sysctl", syscall.ENOMEM)
		}
		copy(buffer, []byte{1, 2, 3})
		return 3, nil
	}, func(_ context.Context, d time.Duration) error {
		sleeps++
		if d <= 0 || d > 25*time.Millisecond {
			t.Fatal(d)
		}
		return nil
	})
	if err != nil || string(data) != "\x01\x02\x03" || reads != 5 || sleeps != 4 {
		t.Fatal(data, err, reads, sleeps)
	}
}

func TestReadIsBoundedAndRejectsPartialOrMalformedOutput(t *testing.T) {
	for _, mode := range []string{"forever", "size-error", "permission", "oversize", "bad-length", "empty"} {
		t.Run(mode, func(t *testing.T) {
			calls, waits := 0, 0
			data, err := read(context.Background(), func(buffer []byte) (uint64, error) {
				calls++
				if mode == "size-error" {
					return 0, syscall.ENOMEM
				}
				if mode == "permission" {
					return 0, syscall.EPERM
				}
				if buffer == nil {
					if mode == "oversize" {
						return maxBytes + 1, nil
					}
					return 10, nil
				}
				if mode == "bad-length" {
					return uint64(len(buffer) + 1), nil
				}
				if mode == "empty" {
					return 0, nil
				}
				buffer[0] = 99
				return uint64(len(buffer)), syscall.ENOMEM
			}, func(context.Context, time.Duration) error { waits++; return nil })
			if mode == "empty" {
				if err != nil || len(data) != 0 {
					t.Fatal(data, err)
				}
				return
			}
			if err == nil || data != nil {
				t.Fatal("partial or invalid buffer accepted", data, err)
			}
			if mode == "forever" && (calls != 20 || waits != 9 || !errors.Is(err, syscall.ENOMEM)) {
				t.Fatal(calls, waits, err)
			}
			if mode == "size-error" && (calls != 10 || waits != 9) {
				t.Fatal(calls, waits)
			}
			if mode == "permission" && calls != 1 {
				t.Fatal("retried permanent error")
			}
		})
	}
}

func TestReadCancellationStopsBackoff(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	calls := 0
	_, err := read(ctx, func(buffer []byte) (uint64, error) {
		calls++
		if buffer == nil {
			return 10, nil
		}
		cancel()
		return 0, syscall.ENOMEM
	}, pause)
	if !errors.Is(err, context.Canceled) || calls != 2 {
		t.Fatal(calls, err)
	}
}
