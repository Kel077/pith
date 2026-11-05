package main

import (
	"errors"
	"strconv"
	"strings"
	"time"
)

func parseHelperUsage(pid int, output string) (helperUsage, error) {
	var r helperUsage
	f := strings.Fields(output)
	if len(f) != 3 {
		return r, errors.New("unexpected helper ps fields")
	}
	n, err := strconv.Atoi(f[0])
	if err != nil || n != pid {
		return r, errors.New("helper ps PID changed")
	}
	rss, err := strconv.ParseUint(f[2], 10, 64)
	if err != nil || rss > 1<<40 {
		return r, errors.New("invalid helper RSS")
	}
	clock := f[1]
	var days uint64
	if parts := strings.Split(clock, "-"); len(parts) == 2 {
		days, err = strconv.ParseUint(parts[0], 10, 32)
		if err != nil || days > 100000 {
			return r, errors.New("invalid helper CPU days")
		}
		clock = parts[1]
	} else if len(parts) > 2 {
		return r, errors.New("invalid helper CPU time")
	}
	parts := strings.Split(clock, ":")
	if len(parts) < 2 || len(parts) > 3 {
		return r, errors.New("invalid helper CPU fields")
	}
	seconds, err := time.ParseDuration(parts[len(parts)-1] + "s")
	if err != nil || seconds < 0 || seconds >= time.Minute {
		return r, errors.New("invalid helper CPU seconds")
	}
	minutes, err := strconv.ParseUint(parts[len(parts)-2], 10, 32)
	if err != nil || minutes > 1000000 {
		return r, errors.New("invalid helper CPU minutes")
	}
	var hours uint64
	if len(parts) == 3 {
		hours, err = strconv.ParseUint(parts[0], 10, 32)
		if err != nil || hours > 1000000 || minutes >= 60 {
			return r, errors.New("invalid helper CPU hours")
		}
	}
	r.PID, r.RSS = pid, rss*1024
	r.CPU = time.Duration(days)*24*time.Hour + time.Duration(hours)*time.Hour + time.Duration(minutes)*time.Minute + seconds
	return r, nil
}
