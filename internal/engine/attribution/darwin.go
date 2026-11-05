//go:build darwin

package attribution

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"runtime"
	"syscall"
	"unsafe"

	"github.com/Kel077/pith/internal/api"
	"golang.org/x/sys/unix"
)

func Snapshot(protocol api.Protocol) ([]Socket, error) {
	var name string
	switch protocol {
	case api.TCP:
		name = "net.inet.tcp.pcblist_n"
	case api.UDP:
		name = "net.inet.udp.pcblist_n"
	default:
		return nil, fmt.Errorf("unsupported protocol %q", protocol)
	}
	var data []byte
	var err error
	for range 3 {
		data, err = unix.SysctlRaw(name)
		if !errors.Is(err, syscall.ENOMEM) {
			break
		}
	}
	if err != nil {
		return nil, fmt.Errorf("read PCB snapshot: %w", err)
	}
	return ParsePCB(data, protocol)
}

// procInfo is the six-argument proc_info syscall, matching libproc's
// proc_pidinfo/proc_pidpath wrappers. Call 2 is PROC_INFO_CALL_PIDINFO; flavor
// 11 is PROC_PIDPATHINFO and 17 is PROC_PIDUNIQIDENTIFIERINFO. Sources:
// XNU bsd/sys/proc_info{,_private}.h and libsyscall/wrappers/libproc/libproc.c.
func procInfo(pid, flavor int, buffer []byte) (int, error) {
	if pid <= 0 || len(buffer) == 0 {
		return 0, syscall.EINVAL
	}
	n, _, errno := syscall.Syscall6(syscall.SYS_PROC_INFO, 2, uintptr(pid), uintptr(flavor), 0, uintptr(unsafe.Pointer(&buffer[0])), uintptr(len(buffer)))
	runtime.KeepAlive(buffer)
	if errno != 0 {
		return 0, errno
	}
	if n > uintptr(len(buffer)) {
		return 0, errors.New("proc_info returned an oversized result")
	}
	return int(n), nil
}

func ProcessPath(pid int) (string, error) {
	buffer := make([]byte, 4096)
	_, err := procInfo(pid, 11, buffer)
	if err != nil {
		return "", err
	}
	// Unlike other proc_info flavors, PROC_PIDPATHINFO does not set retval.
	// libproc obtains the length from the NUL-terminated output buffer.
	if i := bytes.IndexByte(buffer, 0); i >= 0 {
		buffer = buffer[:i]
	}
	if len(buffer) == 0 {
		return "", ErrNotFound
	}
	return string(buffer), nil
}

func processIdentity(pid int) (identity, error) {
	var buffer [56]byte
	n, err := procInfo(pid, 17, buffer[:])
	if err != nil {
		return identity{}, err
	}
	if n != len(buffer) {
		return identity{}, errors.New("short process identity")
	}
	return identity{uuid: [16]byte(buffer[:16]), unique: binary.LittleEndian.Uint64(buffer[16:24]), version: binary.LittleEndian.Uint32(buffer[32:36])}, nil
}
