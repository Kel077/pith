//go:build !darwin || !arm64

package helper

import (
	"errors"
	"net"
)

func VerifyPeer(*net.UnixConn, uint32, string) error {
	return errors.New("helper executable verification requires macOS on Apple Silicon")
}
