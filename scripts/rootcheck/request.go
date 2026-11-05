package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"sync"
	"time"
)

const requestLifetime = 25 * time.Second

// requestCommand is deliberately dispatched before session locking or client
// construction. The copied executable can only read the frozen session and
// make one ordinary TLS request; it never starts or controls a helper.
func requestCommand(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("_request", flag.ContinueOnError)
	dir := fs.String("session", "", "existing private acceptance session")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() != 0 || *dir == "" {
		return errors.New("internal request requires only --session DIRECTORY")
	}
	s, err := loadSession(*dir)
	if err != nil {
		return err
	}
	return serveRequest(ctx, s, os.Stdin, os.Stdout, func(ctx context.Context) (tlsEvidence, error) {
		return exchangeTLS(ctx, s, "", nil)
	})
}

func serveRequest(parent context.Context, s *session, input io.ReadCloser, output io.Writer, exchange func(context.Context) (tlsEvidence, error)) error {
	if !s.Installed || !s.cancellationRecorded() || !s.SmokePassed {
		return errors.New("internal request requires an installed session with a passed smoke check")
	}
	if _, err := s.scopes(); err != nil {
		return err
	}
	path, err := executablePath()
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(parent, requestLifetime)
	defer cancel()
	stop := context.AfterFunc(ctx, func() { _ = input.Close() })
	defer stop()
	proof := requestProof{PID: os.Getpid(), Executable: path, Target: s.Target}
	proof.TLS, err = exchange(ctx)
	if err != nil {
		proof.Problem = err.Error()
	}
	if err := json.NewEncoder(output).Encode(proof); err != nil {
		return err
	}
	// Stay alive while the parent verifies the engine's process attribution.
	// EOF also releases the child if its parent crashes or closes the pipe.
	var ack [1]byte
	_, err = input.Read(ack[:])
	if ctx.Err() != nil {
		return ctx.Err()
	}
	if err != nil && !errors.Is(err, io.EOF) {
		return err
	}
	return nil
}

// childCommand permits a mock protocol process in tests. Production always
// runs the checksum-verified copy directly, with no shell and no descendants.
type childCommand func(context.Context, string, string) *exec.Cmd

func requestFromChild(ctx context.Context, s *session, executable string, command childCommand) (requestProof, func() error, error) {
	childCtx, cancel := context.WithTimeout(ctx, requestLifetime+5*time.Second)
	if command == nil {
		command = func(ctx context.Context, executable, sessionDir string) *exec.Cmd {
			cmd := exec.CommandContext(ctx, executable, "_request", "--session", sessionDir)
			cmd.Env = []string{"PATH=/usr/bin:/bin:/usr/sbin:/sbin", "LC_ALL=C", "TMPDIR=" + os.TempDir()}
			return cmd
		}
	}
	cmd := command(childCtx, executable, s.Dir)
	cmd.WaitDelay = 2 * time.Second
	var stderr limitedOutput
	cmd.Stderr = &stderr
	stdin, err := cmd.StdinPipe()
	if err != nil {
		cancel()
		return requestProof{}, nil, err
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		stdin.Close()
		cancel()
		return requestProof{}, nil, err
	}
	if err := cmd.Start(); err != nil {
		stdin.Close()
		stdout.Close()
		cancel()
		return requestProof{}, nil, err
	}
	var once sync.Once
	var finishErr error
	finish := func() error {
		once.Do(func() {
			_ = stdin.Close()
			finishErr = cmd.Wait()
			cancel()
			if finishErr != nil {
				finishErr = fmt.Errorf("request process did not exit cleanly: %w: %s", finishErr, stderr.String())
			}
		})
		return finishErr
	}
	var proof requestProof
	err = json.NewDecoder(io.LimitReader(stdout, 64<<10)).Decode(&proof)
	if err == nil && (proof.PID != cmd.Process.Pid || proof.Executable != executable || proof.Target != s.Target) {
		err = errors.New("child request proof does not match the spawned PID, executable, or frozen target")
	}
	if err != nil {
		// Do not leave a malformed, silent, or timed-out process behind.
		cancel()
		return requestProof{}, nil, errors.Join(fmt.Errorf("read request process evidence: %w", err), finish())
	}
	return proof, finish, nil
}

type limitedOutput struct{ bytes.Buffer }

func (b *limitedOutput) Write(p []byte) (int, error) {
	const limit = 64 << 10
	n := len(p)
	if left := limit - b.Len(); left > 0 {
		if len(p) > left {
			p = p[:left]
		}
		_, _ = b.Buffer.Write(p)
	}
	return n, nil
}
