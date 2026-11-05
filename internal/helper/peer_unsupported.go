//go:build !darwin

package helper

import (
	"errors"
	"net"
)

func PeerUID(*net.UnixConn) (uint32, error) {
	return 0, errors.New("helper peer authentication requires macOS")
}
