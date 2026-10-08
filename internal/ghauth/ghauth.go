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

// Source reads and caches the token.
type Source struct {
	run func(ctx context.Context) ([]byte, error)

	mu  sync.Mutex
	tok string
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
	out, err := s.run(ctx)
	if err != nil {
		return "", fmt.Errorf("reading the GitHub token: %w", err)
	}
	first, _, _ := strings.Cut(string(out), "\n")
	tok := strings.TrimSpace(first)
	if tok == "" {
		return "", errors.New("reading the GitHub token: the token is empty (is `gh` logged in?)")
	}
	s.tok = tok
	return tok, nil
}

// Invalidate drops the cached token, so the next Token call reads it again.
func (s *Source) Invalidate() {
	s.mu.Lock()
	s.tok = ""
	s.mu.Unlock()
}
