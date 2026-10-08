package supervisor

import (
	"errors"
	"fmt"
	"strings"

	"github.com/actions/scaleset"

	"github.com/Nezhinskiy/local-ci-pool/internal/ghauth"
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

// loginRejected returns the terminal error for a GitHub REST failure that
// means the login is gone, or nil. The GitHub client has already dropped the
// cached token and read it again once before it returns ErrUnauthorized, so
// that error is the second refusal; and a token that cannot be read means gh
// is logged out. Neither fixes itself, so launchd must not respawn into it.
func loginRejected(err error) error {
	switch {
	case errors.Is(err, ghauth.ErrNoToken):
		return terminal("gh logged out: %v; run gh auth login", err)
	case errors.Is(err, github.ErrUnauthorized):
		return terminal("gh login rejected: GitHub refused the token after reading it again; run gh auth login")
	}
	return nil
}

// isTokenRead reports whether err is a failure to read the token from gh, as
// opposed to GitHub rejecting it.
func isTokenRead(err error) bool { return errors.Is(err, ghauth.ErrNoToken) }

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

// isNotFound reports whether a scale set API call answered 404: the scale set
// was deleted under the pool. The status is matched in the message for the
// same reason as in isUnauthorized.
func isNotFound(err error) bool {
	return err != nil && strings.Contains(err.Error(), `status="404 `)
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
