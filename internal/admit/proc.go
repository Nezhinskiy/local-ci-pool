package admit

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"strings"
	"sync"
)

// line is the hold's protocol line.
type line struct {
	Hold       *string `json:"hold"`
	DockerHost *string `json:"docker_host"`
	Unbudgeted *string `json:"unbudgeted"`
}

// proc is a request running as a child process.
type proc struct {
	cmd       *exec.Cmd
	stdin     io.WriteCloser
	admitted  chan Grant
	done      chan int
	closeOnce sync.Once
}

// StartProc starts `path args...` with env added to the pool's environment.
// The first line on the child's stdout is the grant; the child's stderr goes
// to the default logger at debug. The request is not tied to ctx: it ends by
// Withdraw or Close, so that coreslot can withdraw its ticket.
func StartProc(_ context.Context, path string, args, env []string) (Proc, error) {
	cmd := exec.Command(path, args...)
	cmd.Env = append(os.Environ(), env...)
	cmd.Stderr = &debugWriter{log: slog.Default().With(slog.String("child", "coreslot"))}
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, err
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	p := &proc{cmd: cmd, stdin: stdin, admitted: make(chan Grant, 1), done: make(chan int, 1)}
	go p.run(stdout)
	return p, nil
}

// run reads the grant, then the rest of stdout, then reaps the child.
func (p *proc) run(stdout io.Reader) {
	r := bufio.NewReader(stdout)
	text, err := r.ReadString('\n')
	if err == nil {
		if g, ok := parseGrant(text); ok {
			p.admitted <- g
		} else {
			// Not the hold's line: end the hold rather than hold room for a
			// grant nobody can read.
			p.Close()
		}
	}
	close(p.admitted)
	_, _ = io.Copy(io.Discard, r)
	status := -1
	err = p.cmd.Wait()
	var exit *exec.ExitError
	switch {
	case err == nil:
		status = 0
	case errors.As(err, &exit):
		status = exit.ExitCode()
	}
	p.done <- status
}

// parseGrant reads the protocol line; all three fields must be present.
func parseGrant(text string) (Grant, bool) {
	var l line
	if err := json.Unmarshal([]byte(strings.TrimSpace(text)), &l); err != nil {
		return Grant{}, false
	}
	if l.Hold == nil || l.DockerHost == nil || l.Unbudgeted == nil {
		return Grant{}, false
	}
	return Grant{Hold: *l.Hold, DockerHost: *l.DockerHost, Unbudgeted: *l.Unbudgeted}, true
}

func (p *proc) Admitted() <-chan Grant { return p.admitted }
func (p *proc) Done() <-chan int       { return p.done }

// Withdraw sends SIGINT to coreslot.
func (p *proc) Withdraw() { _ = p.cmd.Process.Signal(os.Interrupt) }

// Close closes the hold's stdin.
func (p *proc) Close() { p.closeOnce.Do(func() { _ = p.stdin.Close() }) }

// debugWriter logs each write at debug.
type debugWriter struct{ log *slog.Logger }

func (w *debugWriter) Write(b []byte) (int, error) {
	if s := strings.TrimSpace(string(b)); s != "" {
		w.log.Debug(s)
	}
	return len(b), nil
}
