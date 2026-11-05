//go:build !darwin || !arm64

package main

import (
	"context"
	"errors"
)

func liveHelperUsage(context.Context, int) (helperUsage, error) {
	return helperUsage{}, errors.New("helper usage sampling requires Darwin arm64")
}
