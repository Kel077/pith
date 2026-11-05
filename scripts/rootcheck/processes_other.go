//go:build !darwin || !arm64

package main

import (
	"context"
	"errors"
	"syscall"
)

func liveProcessInventory(context.Context) (processInventory, error) {
	return processInventory{}, errors.New("process inventories require Darwin arm64")
}

func liveProcessSignal(context.Context, processRecord, syscall.Signal) error {
	return errors.New("process cleanup requires Darwin arm64")
}
