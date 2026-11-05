//go:build darwin

package helper

import (
	"fmt"
	"golang.org/x/sys/unix"
	"net"
)

// PeerUID uses LOCAL_PEERCRED, whose xucred is supplied by the kernel. Never
// accept an owner ID or process ID asserted by an IPC request body.
func PeerUID(conn *net.UnixConn) (uint32, error) {
	raw, err := conn.SyscallConn()
	if err != nil {
		return 0, err
	}
	var credentials *unix.Xucred
	var queryErr error
	err = raw.Control(func(fd uintptr) {
		credentials, queryErr = unix.GetsockoptXucred(int(fd), unix.SOL_LOCAL, unix.LOCAL_PEERCRED)
	})
	if err != nil {
		return 0, err
	}
	if queryErr != nil {
		return 0, queryErr
	}
	if credentials.Version != 0 { // XUCRED_VERSION, SDK sys/ucred.h.
		return 0, fmt.Errorf("unsupported peer credential version %d", credentials.Version)
	}
	return credentials.Uid, nil
}
