package netif

import (
	"context"
	"errors"
	"testing"
)

func TestBindingRequiresTheObservedIndexAndGeneration(t *testing.T) {
	var generationErr error
	ctx := WithBinding(context.Background(), 50, func() error { return generationErr })
	if err := ValidateBinding(ctx, 50); err != nil {
		t.Fatal(err)
	}
	if ValidateBinding(ctx, 51) == nil {
		t.Fatal("bound a replacement index")
	}
	generationErr = errors.New("same name and index, different lifetime")
	if !errors.Is(ValidateBinding(ctx, 50), generationErr) {
		t.Fatal("accepted a recycled identity")
	}
	canceled, cancel := context.WithCancel(context.Background())
	cancel()
	if !errors.Is(ValidateBinding(canceled, 50), context.Canceled) {
		t.Fatal("ignored cancellation")
	}
}
