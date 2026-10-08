package supervisor

import (
	"errors"
	"fmt"
	"strings"

	"github.com/actions/scaleset"

	"github.com/Nezhinskiy/local-ci-pool/internal/github"
)

// ErrTerminal marks an error whose cause will not fix itself, such as a
// rejected login or a scale set held elsewhere. cmd exits 0 on it, so launchd
// does not restart the pool into the same failure; every other error exits
// non-zero and launchd retries.
var ErrTerminal = errors.New("terminal")

type terminalError struct{ msg string }

func (e *terminalError) Error() string        { return e.msg }
func (e *terminalError) Is(target error) bool { return target == ErrTerminal }

// terminal returns an error matching ErrTerminal whose text is the message
// alone.
func terminal(format string, args ...any) error {
	return &terminalError{msg: fmt.Sprintf(format, args...)}
}

// isUnauthorized reports whether err is an authentication failure: the GitHub
// client's ErrUnauthorized, or a scale set API error whose response was a 401.
// actions/scaleset v0.4.0 has no typed status error; it writes the status into
// the message as status="401 Unauthorized", so that text is matched. A 401 on
// the message queue is the queue token expiring, which the library refreshes
// itself, so MessageQueueTokenExpiredError never counts.
func isUnauthorized(err error) bool {
	switch {
	case err == nil, errors.Is(err, scaleset.MessageQueueTokenExpiredError):
		return false
	case errors.Is(err, github.ErrUnauthorized):
		return true
	}
	return strings.Contains(err.Error(), `status="401 `)
}

// isSessionConflict reports whether a session create was refused because the
// scale set already has a session: a 409 with
// RunnerScaleSetSessionConflictException, measured identical for a stale
// session of this Mac and for another live holder.
func isSessionConflict(err error) bool {
	if err == nil {
		return false
	}
	msg := err.Error()
	return strings.Contains(msg, `status="409 `) || strings.Contains(msg, "RunnerScaleSetSessionConflictException")
}
