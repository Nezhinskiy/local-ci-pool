package main

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"strings"
	"testing"
	"time"
)

func TestHoldWritesOneLine(t *testing.T) {
	for name, c := range map[string]struct {
		env  map[string]string
		want map[string]string
	}{
		"all set": {
			env: map[string]string{
				"CORESLOT_HOLD":       "h-1",
				"DOCKER_HOST":         "unix:///x/pool/docker.sock",
				"CORESLOT_UNBUDGETED": "timeout",
			},
			want: map[string]string{"hold": "h-1", "docker_host": "unix:///x/pool/docker.sock", "unbudgeted": "timeout"},
		},
		"none set": {
			env:  map[string]string{},
			want: map[string]string{"hold": "", "docker_host": "", "unbudgeted": ""},
		},
	} {
		t.Run(name, func(t *testing.T) {
			var out bytes.Buffer
			if code := hold(envOf(c.env), strings.NewReader(""), &out); code != 0 {
				t.Fatalf("exit %d, want 0", code)
			}
			s := out.String()
			if strings.Count(s, "\n") != 1 || !strings.HasSuffix(s, "\n") {
				t.Fatalf("stdout %q, want exactly one line", s)
			}
			var got map[string]string
			if err := json.Unmarshal([]byte(s), &got); err != nil {
				t.Fatalf("stdout %q is not a JSON object of strings: %v", s, err)
			}
			if len(got) != len(c.want) {
				t.Fatalf("line %v, want %v", got, c.want)
			}
			for k, v := range c.want {
				if g, ok := got[k]; !ok || g != v {
					t.Fatalf("line %v, want %v", got, c.want)
				}
			}
		})
	}
}

// The hold lasts as long as the pool keeps the pipe open, and ends with it.
// Mutation: returning after the line without reading stdin turns it red (the
// hold returns before the writer closes).
func TestHoldExitsOnStdinEOF(t *testing.T) {
	r, w := io.Pipe()
	var out bytes.Buffer
	done := make(chan int, 1)
	go func() { done <- hold(envOf(nil), r, &out) }()
	select {
	case code := <-done:
		t.Fatalf("hold returned %d while stdin was still open", code)
	case <-time.After(200 * time.Millisecond):
	}
	_ = w.Close()
	select {
	case code := <-done:
		if code != 0 {
			t.Fatalf("exit %d at stdin EOF, want 0", code)
		}
	case <-time.After(time.Second):
		t.Fatal("hold did not return within 1s of stdin's EOF")
	}
}

func TestHoldRefusesArguments(t *testing.T) {
	var out, errb bytes.Buffer
	code := run(context.Background(), []string{"hold", "x"}, strings.NewReader(""), &out, &errb, services{})
	if code != exitUsage || !strings.Contains(errb.String(), "usage:") {
		t.Fatalf("exit %d, stderr %q; want 2 with the usage", code, errb.String())
	}
	if out.Len() != 0 {
		t.Fatalf("stdout %q, want nothing", out.String())
	}
}
