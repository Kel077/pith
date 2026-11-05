//go:build !darwin

package netif

import (
	"context"
	"errors"
	"syscall"

	"github.com/Kel077/pith/internal/api"
)

func platformMetadata(context.Context) (map[string]api.Interface, error) {
	return nil, errors.New("network discovery requires macOS")
}
func bindControl(int) func(context.Context, string, string, syscall.RawConn) error {
	return func(context.Context, string, string, syscall.RawConn) error {
		return errors.New("interface binding requires macOS")
	}
}
func Watch(context.Context) (<-chan []api.Interface, error) {
	return nil, errors.New("network notifications require macOS")
}
