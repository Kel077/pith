package primary

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"strings"
	"sync"
	"time"
)

// scutil is an interactive scutil reading commands from a pipe. It flushes
// what each command prints (SCPrint in configd's SystemConfiguration.fproj),
// so a command's answer can be read before the next is sent.
type scutil struct {
	cmd   *exec.Cmd
	stdin io.WriteCloser
	lines chan string
	quit  chan struct{} // closed when nobody reads lines any more
	ended chan struct{}

	closing sync.Once
	closed  error
}

func startScutil(ctx context.Context) (session, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	// Not tied to ctx: close ends it, in order, so that the rank comes back
	// before Yield returns.
	cmd := exec.Command("/usr/sbin/scutil")
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, err
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		stdin.Close()
		return nil, err
	}
	if err := cmd.Start(); err != nil {
		stdin.Close()
		return nil, err
	}
	s := &scutil{cmd: cmd, stdin: stdin, lines: make(chan string, 64), quit: make(chan struct{}), ended: make(chan struct{})}
	go func() {
		scanner := bufio.NewScanner(stdout)
		scanner.Buffer(make([]byte, 4096), 1<<20)
		for scanner.Scan() {
			select {
			case s.lines <- scanner.Text():
			case <-s.quit: // read on to the end, so that scutil can exit
			}
		}
		close(s.lines)
		_ = cmd.Wait()
		close(s.ended)
	}()
	return s, nil
}

func (s *scutil) send(commands ...string) error {
	var b strings.Builder
	for _, c := range commands {
		if strings.ContainsAny(c, "\r\n") {
			return errors.New("primary: a scutil command spans lines")
		}
		b.WriteString(c)
		b.WriteByte('\n')
	}
	_, err := io.WriteString(s.stdin, b.String())
	return err
}

// show reads one dictionary: lines printed before it are the errors of the
// commands sent before, and make show fail.
func (s *scutil) show(ctx context.Context, key string, pattern bool) (dict, error) {
	command := "show " + key
	if pattern {
		command += " pattern"
	}
	if err := s.send(command); err != nil {
		return nil, err
	}
	timer := time.NewTimer(3 * time.Second)
	defer timer.Stop()
	var errs []string
	failed := func() error {
		if len(errs) == 0 {
			return nil
		}
		return fmt.Errorf("primary: scutil: %s", strings.Join(errs, "; "))
	}
	var root dict
	var stack []dict
	for {
		var line string
		select {
		case l, ok := <-s.lines:
			if !ok {
				return nil, errors.New("primary: scutil ended")
			}
			line = strings.TrimSpace(l)
		case <-timer.C:
			return nil, errors.New("primary: scutil did not answer")
		case <-ctx.Done():
			return nil, ctx.Err()
		}
		switch {
		case line == "":
		case len(stack) == 0 && line == "No such key":
			return nil, failed()
		case len(stack) == 0 && line == "<dictionary> {" && root == nil:
			root = dict{}
			stack = []dict{root}
		case len(stack) == 0:
			errs = append(errs, line)
		case line == "}":
			stack = stack[:len(stack)-1]
			if len(stack) == 0 {
				return root, failed()
			}
		default:
			k, v, ok := strings.Cut(line, " : ")
			if !ok {
				// An empty string prints as "key : ".
				k, ok = strings.CutSuffix(line, " :")
			}
			if !ok {
				return nil, fmt.Errorf("primary: unexpected scutil output %q", line)
			}
			if v == "<dictionary> {" || v == "<array> {" {
				child := dict{}
				stack[len(stack)-1][k] = child
				stack = append(stack, child)
			} else {
				stack[len(stack)-1][k] = v
			}
		}
	}
}

// close ends the input, at whose end scutil exits and configd removes the
// session's temporary keys; one that doesn't exit within two seconds is
// killed, which closes its session too.
func (s *scutil) close() error {
	s.closing.Do(func() {
		close(s.quit)
		_ = s.stdin.Close()
		select {
		case <-s.ended:
			return
		case <-time.After(2 * time.Second):
		}
		_ = s.cmd.Process.Kill()
		select {
		case <-s.ended:
		case <-time.After(2 * time.Second):
			s.closed = errors.New("primary: scutil did not exit")
		}
	})
	return s.closed
}

func (s *scutil) done() <-chan struct{} { return s.ended }
