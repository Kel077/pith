// Package rib reads the Darwin route table with bounded growth tolerance.
package rib

import (
	"context"
	"errors"
	"fmt"
	"syscall"
	"time"
)

const maxBytes = 64 << 20
const maxAttempts = 10

type queryFunc func([]byte) (uint64, error)

func pause(ctx context.Context, delay time.Duration) error {
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

// read never returns a partial ENOMEM buffer. Size is only a hint: reserve at
// least 16 KiB/25% extra and grow again after a collision, even when the kernel
// reports only the bytes copied rather than the full new size.
func read(ctx context.Context, query queryFunc, wait func(context.Context, time.Duration) error) ([]byte, error) {
	var previous uint64
	var last error
	for attempt := 0; attempt < maxAttempts; attempt++ {
		if err := ctx.Err(); err != nil {
			return nil, errors.Join(err, last)
		}
		if attempt != 0 {
			if err := wait(ctx, min(time.Duration(attempt)*5*time.Millisecond, 25*time.Millisecond)); err != nil {
				return nil, errors.Join(err, last)
			}
		}
		size, err := query(nil)
		if err != nil {
			last = err
			if errors.Is(err, syscall.ENOMEM) {
				continue
			}
			return nil, err
		}
		base := max(size, previous)
		if base > maxBytes {
			return nil, errors.New("route snapshot exceeds the 64 MiB allocation limit")
		}
		capacity := min(uint64(maxBytes), base+max(uint64(16<<10), base/4))
		buffer := make([]byte, int(capacity))
		n, err := query(buffer)
		if err == nil {
			if n > capacity {
				return nil, errors.New("route snapshot returned an invalid length")
			}
			if ctx.Err() != nil {
				return nil, ctx.Err()
			}
			return buffer[:int(n)], nil
		}
		last = err
		if !errors.Is(err, syscall.ENOMEM) {
			return nil, err
		}
		previous = max(capacity, n)
	}
	return nil, fmt.Errorf("route snapshot unavailable after %d attempts: %w", maxAttempts, last)
}
