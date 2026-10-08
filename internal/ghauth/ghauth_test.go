package ghauth

import (
	"context"
	"errors"
	"strings"
	"sync/atomic"
	"testing"
)

func TestTokenIsCachedUntilInvalidated(t *testing.T) {
	var calls atomic.Int32
	s := NewSource(func(context.Context) ([]byte, error) {
		n := calls.Add(1)
		if n == 1 {
			return []byte("first-token\n"), nil
		}
		return []byte("second-token\n"), nil
	})
	for i := 0; i < 3; i++ {
		got, err := s.Token(context.Background())
		if err != nil || got != "first-token" {
			t.Fatalf("call %d: got %q, %v", i, got, err)
		}
	}
	if calls.Load() != 1 {
		t.Fatalf("run called %d times, want 1", calls.Load())
	}
	s.Invalidate()
	got, err := s.Token(context.Background())
	if err != nil || got != "second-token" {
		t.Fatalf("after Invalidate: got %q, %v", got, err)
	}
	if calls.Load() != 2 {
		t.Fatalf("run called %d times, want 2", calls.Load())
	}
}

func TestFailuresAreNotCachedAndEmptyTokenIsRefused(t *testing.T) {
	var calls atomic.Int32
	s := NewSource(func(context.Context) ([]byte, error) {
		if calls.Add(1) == 1 {
			return nil, errors.New("gh: not logged in")
		}
		return []byte("  \n"), nil
	})
	if _, err := s.Token(context.Background()); !errors.Is(err, ErrNoToken) {
		t.Fatalf("want ErrNoToken when gh fails, got %v", err)
	}
	_, err := s.Token(context.Background())
	if !errors.Is(err, ErrNoToken) || !strings.Contains(err.Error(), "empty") {
		t.Fatalf("want an empty-token error matching ErrNoToken, got %v", err)
	}
	if calls.Load() != 2 {
		t.Fatalf("a failure must not be cached: run called %d times", calls.Load())
	}
}

func TestOnlyFirstLineIsTheToken(t *testing.T) {
	s := NewSource(func(context.Context) ([]byte, error) {
		return []byte("tok\nWARNING: something else\n"), nil
	})
	got, err := s.Token(context.Background())
	if err != nil || got != "tok" {
		t.Fatalf("got %q, %v", got, err)
	}
}

func TestErrorsNeverCarryRunOutput(t *testing.T) {
	const secret = "ghp_SECRETVALUE123"
	s := NewSource(func(context.Context) ([]byte, error) {
		// A misbehaving runner that returns bytes together with an error.
		return []byte(secret), errors.New("exit status 1")
	})
	_, err := s.Token(context.Background())
	if err == nil {
		t.Fatal("want an error")
	}
	if strings.Contains(err.Error(), secret) {
		t.Fatalf("error leaks the output: %v", err)
	}
}
