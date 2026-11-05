//go:build darwin && arm64

package main

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"os"
	"runtime"
	"syscall"
	"time"
	"unsafe"

	"golang.org/x/sys/unix"
)

// ABI references: XNU bsd/sys/proc_info_private.h and bsd/kern/proc_info.c.
// Signal-by-audit-token checks PID plus exec generation atomically in the kernel;
// there is deliberately no fallback to kill(pid) or process-group signals.
type processCall func(call, pid, flavor int, buffer []byte) (int, error)

func callProcessInfo(call, pid, flavor int, buffer []byte) (int, error) {
	if len(buffer) == 0 {
		return 0, errors.New("empty process information buffer")
	}
	n, _, errno := syscall.Syscall6(syscall.SYS_PROC_INFO, uintptr(call), uintptr(pid), uintptr(flavor), 0, uintptr(unsafe.Pointer(&buffer[0])), uintptr(len(buffer)))
	runtime.KeepAlive(buffer)
	if errno != 0 {
		return 0, errno
	}
	return int(n), nil
}

func processDetails(p processRecord, call processCall) (processRecord, error) {
	var before, after [56]byte
	var coalition [40]byte
	var path [4096]byte
	n, err := call(2, p.PID, 17, before[:]) // PROC_PIDUNIQIDENTIFIERINFO
	if err != nil {
		return p, err
	}
	if n != len(before) {
		return p, errors.New("short unique process identity")
	}
	if _, err = call(2, p.PID, 11, path[:]); err != nil {
		return p, err
	}
	end := bytes.IndexByte(path[:], 0)
	if end <= 0 {
		return p, errors.New("missing process executable path")
	}
	n, err = call(2, p.PID, 20, coalition[:]) // PROC_PIDCOALITIONINFO
	if err != nil {
		return p, err
	}
	if n != len(coalition) {
		return p, errors.New("short process coalition identity")
	}
	n, err = call(2, p.PID, 17, after[:])
	if err != nil {
		return p, err
	}
	if n != len(after) || before != after {
		return p, errProcessChanged
	}
	p.Path = string(path[:end])
	p.UniqueID = binary.LittleEndian.Uint64(before[16:24])
	p.Version = binary.LittleEndian.Uint32(before[32:36])
	p.Resource = binary.LittleEndian.Uint64(coalition[:8])
	p.Jetsam = binary.LittleEndian.Uint64(coalition[8:16])
	if p.UniqueID == 0 || p.Version == 0 {
		return p, errors.New("missing unique process identity")
	}
	return p, nil
}

// Use x/sys's generated Darwin structure offsets, without interpreting kernel
// pointers as Go pointers. kern.proc.all supplies start times even for other UIDs.
func decodeProcessTable(data []byte) ([]processRecord, error) {
	if len(data)%unix.SizeofKinfoProc != 0 || len(data) > 32<<20 {
		return nil, errors.New("invalid process table size")
	}
	var k unix.KinfoProc
	const base = unsafe.Offsetof(k.Proc)
	const eb = unsafe.Offsetof(k.Eproc)
	u32 := func(b []byte, offset uintptr) uint32 { return binary.LittleEndian.Uint32(b[offset : offset+4]) }
	u64 := func(b []byte, offset uintptr) uint64 { return binary.LittleEndian.Uint64(b[offset : offset+8]) }
	var result []processRecord
	for len(data) != 0 {
		b := data[:unix.SizeofKinfoProc]
		data = data[unix.SizeofKinfoProc:]
		pid := int(u32(b, base+unsafe.Offsetof(k.Proc.P_pid)))
		if pid <= 1 {
			continue
		}
		start := base + unsafe.Offsetof(k.Proc.P_starttime)
		sec := int64(u64(b, start+unsafe.Offsetof(k.Proc.P_starttime.Sec)))
		usec := int64(u32(b, start+unsafe.Offsetof(k.Proc.P_starttime.Usec)))
		if sec <= 0 || usec < 0 || usec >= 1000000 {
			return nil, errors.New("invalid process start time")
		}
		comm := base + unsafe.Offsetof(k.Proc.P_comm)
		name := b[comm : comm+uintptr(len(k.Proc.P_comm))]
		if end := bytes.IndexByte(name, 0); end >= 0 {
			name = name[:end]
		}
		result = append(result, processRecord{PID: pid, Parent: int(u32(b, eb+unsafe.Offsetof(k.Eproc.Ppid))), UID: int(u32(b, eb+unsafe.Offsetof(k.Eproc.Ucred)+unsafe.Offsetof(k.Eproc.Ucred.Uid))), Started: time.Unix(sec, usec*1000), Name: string(name)})
	}
	return result, nil
}

func readProcessTable(ctx context.Context, read func() ([]byte, error)) ([]processRecord, error) {
	for attempt := 0; attempt < 10; attempt++ {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		data, err := read()
		if err == nil {
			return decodeProcessTable(data)
		}
		if !errors.Is(err, syscall.ENOMEM) {
			return nil, err
		}
		if err := processPause(ctx, 10*time.Millisecond); err != nil {
			return nil, err
		}
	}
	return nil, fmt.Errorf("process inventory growth did not settle: %w", syscall.ENOMEM)
}

func liveProcessInventory(parent context.Context) (processInventory, error) {
	ctx, cancel := context.WithTimeout(parent, 2*time.Second)
	defer cancel()
	r := processInventory{At: time.Now()}
	rows, err := readProcessTable(ctx, func() ([]byte, error) { return unix.SysctlRaw("kern.proc.all") })
	if err != nil {
		return r, err
	}
	for _, p := range rows {
		if err := ctx.Err(); err != nil {
			return r, err
		}
		if p.UID == os.Getuid() {
			detailed, err := processDetails(p, callProcessInfo)
			if errors.Is(err, syscall.ESRCH) || errors.Is(err, syscall.ENOENT) {
				continue
			}
			if err == nil {
				// The table predates the detail calls. Reject PID reuse between them.
				fresh, e := unix.SysctlKinfoProc("kern.proc.pid", p.PID)
				if e != nil || fresh.Proc.P_pid != int32(p.PID) || fresh.Eproc.Ucred.Uid != uint32(p.UID) || !time.Unix(fresh.Proc.P_starttime.Sec, int64(fresh.Proc.P_starttime.Usec)*1000).Equal(p.Started) {
					err = errProcessChanged
				} else {
					p = detailed
				}
			}
			if err != nil {
				p.Problem = err.Error()
			}
		}
		r.Processes = append(r.Processes, p)
	}
	return r, nil
}

func signalProcessIdentity(ctx context.Context, p processRecord, signal syscall.Signal, call processCall) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if p.PID <= 1 || p.UID != os.Getuid() || p.Version == 0 || p.UniqueID == 0 || p.Problem != "" || (signal != syscall.SIGTERM && signal != syscall.SIGKILL) {
		return errors.New("invalid process cleanup signal")
	}
	now, err := processDetails(p, call)
	if err != nil {
		return err
	}
	if !sameProcess(p, now) || p.Jetsam != now.Jetsam || p.Resource != now.Resource {
		return errProcessChanged
	}
	var token [32]byte // audit_token_t: PID in word 5, pidversion in word 7.
	binary.LittleEndian.PutUint32(token[20:24], uint32(p.PID))
	binary.LittleEndian.PutUint32(token[28:32], p.Version)
	_, err = call(17, 0, int(signal), token[:]) // PROC_INFO_CALL_SIGNAL_AUDITTOKEN
	return err
}

func liveProcessSignal(ctx context.Context, p processRecord, signal syscall.Signal) error {
	return signalProcessIdentity(ctx, p, signal, callProcessInfo)
}
