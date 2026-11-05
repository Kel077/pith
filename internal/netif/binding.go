package netif

import (
	"context"
	"errors"
)

type bindingKey struct{}
type binding struct {
	index int
	check func() error
}

// WithBinding carries an engine observation generation into both application
// and resolver socket controls. Ordinary unprivileged probes need no generation.
func WithBinding(ctx context.Context, index int, check func() error) context.Context {
	return context.WithValue(ctx, bindingKey{}, binding{index, check})
}

func ValidateBinding(ctx context.Context, index int) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if b, ok := ctx.Value(bindingKey{}).(binding); ok {
		if b.index <= 0 || index != b.index {
			return errors.New("socket interface generation changed")
		}
		return b.check()
	}
	return nil
}
