// Package ghauth supplies the GitHub token the pool uses. The token comes from
// `gh auth token` at run time. It is cached in memory only and never appears in
// an error or a log line.
package ghauth

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os/exec"
	"strings"
	"sync"
)

// ErrNoToken is wrapped by every error of Token: the token could not be read
// (gh is not logged in, or not installed, or printed nothing). It does not
// prove a logout: gh exits 1 as well when its keychain lookup times out (60 s
// in gh 2.x), which a slow or stalled Mac can make it do.
var ErrNoToken = errors.New("reading the GitHub token")

// Source reads and caches the token.
type Source struct {
	run func(ctx context.Context) ([]byte, error)

	mu  sync.Mutex
	tok string

	// failed is the error of the last read, nil once one succeeded. It has
	// its own lock so ReadErr does not wait for a read in progress.
	failedMu sync.Mutex
	failed   error
}

// NewSource returns a Source that obtains the token from run. A nil run
// executes `gh auth token`.
func NewSource(run func(ctx context.Context) ([]byte, error)) *Source {
	if run == nil {
		run = ghAuthToken
	}
	return &Source{run: run}
}

func ghAuthToken(ctx context.Context) ([]byte, error) {
	cmd := exec.CommandContext(ctx, "gh", "auth", "token")
	var stdout bytes.Buffer
	cmd.Stdout = &stdout
	// stderr is dropped on purpose: the error carries the exit status only.
	if err := cmd.Run(); err != nil {
		var ee *exec.ExitError
		if errors.As(err, &ee) {
			return nil, fmt.Errorf("gh auth token exited with status %d", ee.ExitCode())
		}
		return nil, fmt.Errorf("gh auth token: %w", err)
	}
	return stdout.Bytes(), nil
}

// Token returns the cached token, reading it first when the cache is empty.
// The error never contains what the command printed.
func (s *Source) Token(ctx context.Context) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.tok != "" {
		return s.tok, nil
	}
	tok, err := s.read(ctx)
	s.failedMu.Lock()
	s.failed = err
	s.failedMu.Unlock()
	if err != nil {
		return "", err
	}
	s.tok = tok
	return tok, nil
}

func (s *Source) read(ctx context.Context) (string, error) {
	out, err := s.run(ctx)
	if err != nil {
		return "", fmt.Errorf("%w: %w", ErrNoToken, err)
	}
	first, _, _ := strings.Cut(string(out), "\n")
	tok := strings.TrimSpace(first)
	if tok == "" {
		return "", fmt.Errorf("%w: the token is empty (is `gh` logged in?)", ErrNoToken)
	}
	return tok, nil
}

// ReadErr returns the error of the last read of the token, or nil if it
// succeeded or none has been made. It never waits for a read in progress.
func (s *Source) ReadErr() error {
	s.failedMu.Lock()
	defer s.failedMu.Unlock()
	return s.failed
}

// Invalidate drops the cached token, so the next Token call reads it again.
func (s *Source) Invalidate() {
	s.mu.Lock()
	s.tok = ""
	s.mu.Unlock()
}
