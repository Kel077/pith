//go:build darwin && arm64

package main

import (
	"context"
	"errors"
	"os/exec"
	"strconv"
	"time"

	"github.com/Kel077/pith/internal/helper"
)

func liveHelperUsage(parent context.Context, pid int) (helperUsage, error) {
	var r helperUsage
	if pid <= 1 {
		return r, errors.New("invalid helper PID")
	}
	ctx, cancel := context.WithTimeout(parent, 2*time.Second)
	defer cancel()
	identity, err := processDetails(processRecord{PID: pid}, callProcessInfo)
	if err != nil {
		return r, err
	}
	if !canonicalSame(identity.Path, helper.BinaryPath) {
		return r, errors.New("usage target is not the reviewed helper executable")
	}
	// ps(1) documents time as cumulative user+system CPU and rss in 1024-byte
	// units. Explicit fields, a single PID, no shell and no process arguments.
	cmd := exec.CommandContext(ctx, "/bin/ps", "-p", strconv.Itoa(pid), "-o", "pid=,time=,rss=")
	cmd.WaitDelay = time.Second
	data, err := cmd.Output()
	if err != nil {
		return r, err
	}
	r, err = parseHelperUsage(pid, string(data))
	if err != nil {
		return r, err
	}
	after, err := processDetails(identity, callProcessInfo)
	if err != nil {
		return r, err
	}
	if !sameProcess(identity, after) {
		return r, errors.New("helper identity changed while sampling usage")
	}
	r.Identity, r.At = identity.UniqueID, time.Now()
	return r, nil
}
