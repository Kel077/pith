//go:build darwin && arm64

package helper

import (
	"encoding/hex"
	"errors"
	"net"
	"runtime"
	"syscall"
	"unsafe"
)

type auditToken [8]uint32

// XNU 8792 (macOS 13): LOCAL_PEERTOKEN is SOL_LOCAL option 6, and
// csops_audittoken is syscall 170. CS_OPS_CDHASH (5) is a read-only query
// available without root and checks the supplied PID generation in the kernel.
func peerToken(conn *net.UnixConn) (auditToken, error) {
	raw, err := conn.SyscallConn()
	if err != nil {
		return auditToken{}, err
	}
	var token auditToken
	size := uint32(unsafe.Sizeof(token))
	var queryErr syscall.Errno
	err = raw.Control(func(fd uintptr) {
		_, _, queryErr = syscall.Syscall6(syscall.SYS_GETSOCKOPT, fd, 0, 6, uintptr(unsafe.Pointer(&token)), uintptr(unsafe.Pointer(&size)), 0)
	})
	runtime.KeepAlive(&token)
	runtime.KeepAlive(&size)
	if err != nil {
		return auditToken{}, err
	}
	if queryErr != 0 {
		return auditToken{}, queryErr
	}
	if size != uint32(unsafe.Sizeof(token)) || token[5] == 0 || token[5] > 0x7fffffff || token[7] == 0 {
		return auditToken{}, errors.New("invalid peer audit token")
	}
	return token, nil
}

func processCodeHash(token auditToken) (string, error) {
	var hash [20]byte
	_, _, errno := syscall.Syscall6(170, uintptr(token[5]), 5, uintptr(unsafe.Pointer(&hash)), uintptr(len(hash)), uintptr(unsafe.Pointer(&token)), 0)
	runtime.KeepAlive(&token)
	runtime.KeepAlive(&hash)
	if errno != 0 {
		return "", errno
	}
	return hex.EncodeToString(hash[:]), nil
}

// VerifyPeer authenticates the executing peer on this connected socket. It
// never trusts a PID, version or hash supplied in an IPC message.
func VerifyPeer(conn *net.UnixConn, wantUID uint32, wantCDHash string) error {
	uid, err := PeerUID(conn)
	if err != nil || uid != wantUID {
		return errors.New("helper peer credentials could not be verified")
	}
	before, err := peerToken(conn)
	if err != nil || before[1] != wantUID {
		return errors.New("helper peer audit identity could not be verified")
	}
	hash, err := processCodeHash(before)
	if err != nil || hash != wantCDHash {
		return errors.New("helper executing artifact could not be verified")
	}
	after, err := peerToken(conn)
	if err != nil || before != after {
		return errors.New("helper peer identity changed during verification")
	}
	return nil
}
