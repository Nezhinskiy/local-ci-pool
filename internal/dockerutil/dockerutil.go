// Package dockerutil holds the small helpers the Docker-facing packages share.
package dockerutil

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
)

const tailLines = 10

// DrainStream reads a Docker JSON message stream (the body of an image build
// or an image pull) to its end. Docker reports a failure inside a 200 response
// as a message with an "error" field, so reading the stream is the only way to
// see it. The returned error carries the last few output lines.
func DrainStream(r io.Reader) error {
	dec := json.NewDecoder(r)
	var tail []string
	for {
		var msg struct {
			Stream string `json:"stream"`
			Status string `json:"status"`
			Error  string `json:"error"`
			Detail struct {
				Message string `json:"message"`
			} `json:"errorDetail"`
		}
		err := dec.Decode(&msg)
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return fmt.Errorf("reading the Docker message stream: %w", err)
		}
		for _, line := range strings.Split(msg.Stream, "\n") {
			if line = strings.TrimRight(line, "\r"); line != "" {
				tail = append(tail, line)
			}
		}
		if len(tail) > tailLines {
			tail = tail[len(tail)-tailLines:]
		}
		failure := msg.Error
		if failure == "" {
			failure = msg.Detail.Message
		}
		if failure != "" {
			if len(tail) == 0 {
				return errors.New(failure)
			}
			return fmt.Errorf("%s\n%s", failure, strings.Join(tail, "\n"))
		}
	}
}
