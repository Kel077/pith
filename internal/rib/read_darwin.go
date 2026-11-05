//go:build darwin

package rib

import (
	"context"
	"os"
	"runtime"
	"syscall"
	"time"
	"unsafe"

	"golang.org/x/sys/unix"
)

// Read returns one complete AF_UNSPEC/NET_RT_DUMP, without any sysctl writer or
// routing socket. Ten ENOMEM attempts and a 250 ms deadline bound each read.
func Read(parent context.Context) ([]byte, error) {
	ctx, cancel := context.WithTimeout(parent, 250*time.Millisecond)
	defer cancel()
	return read(ctx, routeQuery, pause)
}

func routeQuery(buffer []byte) (uint64, error) {
	// XNU bsd/kern/syscalls.master syscall 202: int[6] MIB, count, old buffer,
	// size_t*, new pointer, new length. The last two are always zero (read-only).
	// kern_newsysctl.c updates oldlen even on ENOMEM; it may be only bytes copied.
	mib := [6]int32{unix.CTL_NET, unix.AF_ROUTE, 0, unix.AF_UNSPEC, unix.NET_RT_DUMP, 0}
	n := uintptr(len(buffer))
	var pointer unsafe.Pointer
	if len(buffer) != 0 {
		pointer = unsafe.Pointer(&buffer[0])
	}
	_, _, errno := syscall.Syscall6(syscall.SYS___SYSCTL, uintptr(unsafe.Pointer(&mib[0])), uintptr(len(mib)), uintptr(pointer), uintptr(unsafe.Pointer(&n)), 0, 0)
	runtime.KeepAlive(mib)
	runtime.KeepAlive(buffer)
	if errno != 0 {
		return uint64(n), os.NewSyscallError("sysctl", errno)
	}
	return uint64(n), nil
}
