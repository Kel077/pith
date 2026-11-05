//go:build darwin && arm64

package main

import (
	"context"
	"encoding/binary"
	"errors"
	"os"
	"syscall"
	"testing"
	"time"
	"unsafe"

	"golang.org/x/sys/unix"
)

func syntheticProcessTable() []byte {
	b := make([]byte, unix.SizeofKinfoProc)
	var k unix.KinfoProc
	u32 := func(offset uintptr, n uint32) { binary.LittleEndian.PutUint32(b[offset:offset+4], n) }
	binary.LittleEndian.PutUint64(b[:8], 1700000000)
	u32(8, 123456)
	u32(unsafe.Offsetof(k.Proc.P_pid), 701)
	u32(unsafe.Offsetof(k.Eproc)+unsafe.Offsetof(k.Eproc.Ppid), 1)
	u32(unsafe.Offsetof(k.Eproc)+unsafe.Offsetof(k.Eproc.Ucred)+unsafe.Offsetof(k.Eproc.Ucred.Uid), 501)
	copy(b[unsafe.Offsetof(k.Proc.P_comm):], "Safari")
	return b
}

func TestProcessTableWireFormatAndGrowthBound(t *testing.T) {
	data := syntheticProcessTable()
	rows, err := decodeProcessTable(data)
	if err != nil || len(rows) != 1 {
		t.Fatal(rows, err)
	}
	p := rows[0]
	if p.PID != 701 || p.Parent != 1 || p.UID != 501 || p.Name != "Safari" || !p.Started.Equal(time.Unix(1700000000, 123456000)) {
		t.Fatal(p)
	}
	if _, err := decodeProcessTable(data[:len(data)-1]); err == nil {
		t.Fatal("partial record accepted")
	}
	attempts := 0
	rows, err = readProcessTable(context.Background(), func() ([]byte, error) {
		attempts++
		if attempts < 4 {
			return data[:13], syscall.ENOMEM
		}
		return data, nil
	})
	if err != nil || len(rows) != 1 || attempts != 4 {
		t.Fatal(rows, err, attempts)
	}
	attempts = 0
	_, err = readProcessTable(context.Background(), func() ([]byte, error) { attempts++; return nil, syscall.ENOMEM })
	if !errors.Is(err, syscall.ENOMEM) || attempts != 10 {
		t.Fatal(err, attempts)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := readProcessTable(ctx, func() ([]byte, error) { t.Fatal("read after cancellation"); return nil, nil }); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
}

func syntheticProcessCalls(p processRecord, mutate func(int, int, []byte)) processCall {
	return func(call, pid, flavor int, data []byte) (int, error) {
		if call != 2 || pid != p.PID {
			return 0, errors.New("unexpected process operation")
		}
		switch flavor {
		case 17:
			binary.LittleEndian.PutUint64(data[16:24], p.UniqueID)
			binary.LittleEndian.PutUint32(data[32:36], p.Version)
		case 11:
			copy(data, p.Path)
		case 20:
			binary.LittleEndian.PutUint64(data[:8], p.Resource)
			binary.LittleEndian.PutUint64(data[8:16], p.Jetsam)
		default:
			return 0, errors.New("unexpected process flavor")
		}
		if mutate != nil {
			mutate(call, flavor, data)
		}
		if flavor == 11 {
			return 0, nil
		} // Darwin path call can fill the buffer with a zero retval.
		return len(data), nil
	}
}

func TestProcessAuditTokenSignalCannotTargetReusedPID(t *testing.T) {
	p := fakeProcess(701, "Safari", "/Applications/Safari.app/Contents/MacOS/Safari", time.Now())
	for _, mode := range []string{"term", "kill", "reused", "exec", "path", "coalition", "permission", "atomic-reuse", "other-uid", "other-signal"} {
		t.Run(mode, func(t *testing.T) {
			current := p
			sig := syscall.SIGTERM
			switch mode {
			case "kill":
				sig = syscall.SIGKILL
			case "reused":
				current.UniqueID++
			case "exec":
				current.Version++
			case "path":
				current.Path = "/Applications/Example.app/Contents/MacOS/Example"
			case "coalition":
				current.Jetsam++
			case "other-signal":
				sig = syscall.SIGUSR1
			}
			original := p
			if mode == "other-uid" {
				original.UID = os.Getuid() + 1
			}
			details := syntheticProcessCalls(current, nil)
			signals := 0
			call := func(call, pid, flavor int, data []byte) (int, error) {
				if call != 17 {
					return details(call, pid, flavor, data)
				}
				signals++
				if pid != 0 || flavor != int(sig) || len(data) != 32 || binary.LittleEndian.Uint32(data[20:24]) != uint32(p.PID) || binary.LittleEndian.Uint32(data[28:32]) != p.Version {
					t.Fatal("incorrect audit token or call", pid, flavor, data)
				}
				if mode == "permission" {
					return 0, syscall.EPERM
				}
				if mode == "atomic-reuse" {
					return 0, syscall.ESRCH
				}
				return 0, nil
			}
			err := signalProcessIdentity(context.Background(), original, sig, call)
			if mode == "term" || mode == "kill" {
				if err != nil || signals != 1 {
					t.Fatal(err, signals)
				}
			} else {
				if err == nil {
					t.Fatal("unsafe signal accepted")
				}
				want := 0
				if mode == "permission" || mode == "atomic-reuse" {
					want = 1
				}
				if signals != want {
					t.Fatal("signal fallback/retry", signals)
				}
			}
		})
	}
}

func TestProcessDetailsRejectExecBetweenReads(t *testing.T) {
	p := fakeProcess(701, "Safari", "/Applications/Safari.app/Contents/MacOS/Safari", time.Now())
	reads := 0
	call := syntheticProcessCalls(p, func(_ int, flavor int, data []byte) {
		if flavor == 17 {
			reads++
			if reads == 2 {
				binary.LittleEndian.PutUint32(data[32:36], p.Version+1)
			}
		}
	})
	if _, err := processDetails(p, call); !errors.Is(err, errProcessChanged) {
		t.Fatal(err)
	}
}
