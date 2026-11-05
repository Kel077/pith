package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"testing"
	"time"
)

// This subprocess fixture emits protocol evidence only. It does not call the
// request command, load helper state, open a socket, or start a descendant.
func TestRequestProtocolFixture(t *testing.T) {
	if os.Getenv("PITH_REQUEST_FIXTURE") != "1" {
		return
	}
	mode := os.Getenv("PITH_REQUEST_MODE")
	proof := requestProof{PID: os.Getpid(), Executable: "/tmp/pith-example/request-tool/rootcheck-unmatched", Target: "192.0.2.20:443", TLS: tlsEvidence{StatusCode: 200, PublicIP: "203.0.113.20"}}
	switch mode {
	case "wrong-pid":
		proof.PID++
		_ = json.NewEncoder(os.Stdout).Encode(proof)
	case "malformed":
		fmt.Fprintln(os.Stdout, "{broken}")
	case "quiet":
	default:
		_ = json.NewEncoder(os.Stdout).Encode(proof)
	}
	_, _ = io.Copy(io.Discard, os.Stdin)
	os.Exit(0)
}

func TestRequestChildIsReapedOnSuccessAndFailure(t *testing.T) {
	for _, mode := range []string{"good", "wrong-pid", "malformed", "quiet"} {
		t.Run(mode, func(t *testing.T) {
			s, _, _ := appFixture()
			var command *exec.Cmd
			factory := func(ctx context.Context, _, _ string) *exec.Cmd {
				command = exec.CommandContext(ctx, os.Args[0], "-test.run=^TestRequestProtocolFixture$")
				command.Env = []string{"PITH_REQUEST_FIXTURE=1", "PITH_REQUEST_MODE=" + mode}
				return command
			}
			timeout := 5 * time.Second
			if mode == "quiet" {
				timeout = 100 * time.Millisecond
			}
			ctx, cancel := context.WithTimeout(context.Background(), timeout)
			defer cancel()
			proof, finish, err := requestFromChild(ctx, s, "/tmp/pith-example/request-tool/rootcheck-unmatched", factory)
			if mode == "good" {
				if err != nil || finish == nil || proof.PID != command.Process.Pid {
					t.Fatal("missing controlled-process evidence", proof, err)
				}
				if err := finish(); err != nil {
					t.Fatal("request process was not acknowledged and reaped", err)
				}
				if err := finish(); err != nil {
					t.Fatal("finish is not idempotent", err)
				}
			} else if err == nil || finish != nil {
				t.Fatal("malformed or unresponsive process was accepted", err)
			}
			if command == nil || command.ProcessState == nil {
				t.Fatal("request process was not waited for")
			}
		})
	}
}
