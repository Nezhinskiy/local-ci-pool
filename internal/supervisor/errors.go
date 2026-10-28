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
// non-zero and launchd retries. A token gh cannot give just now is not one: the
// pool waits for it (see isTokenRead).
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
// that error is GitHub refusing a token gh has just given: it does not fix
// itself, and a pool that kept sending it would have GitHub reject even valid
// credentials for a while after many failed attempts.
//
// A token that cannot be read is not a lost login (isTokenRead): it is
// retried, and the pool reports it in the snapshot meanwhile.
func loginRejected(err error) error {
	if errors.Is(err, github.ErrUnauthorized) {
		return terminal("gh login rejected: GitHub refused the token after reading it again; run gh auth login")
	}
	return nil
}

// isTokenRead reports whether err is a failure to read the token from gh, as
// opposed to GitHub rejecting it. gh exits 1 both when it has no login and when
// its keychain lookup times out, so the failure may fix itself: every caller
// retries it.
func isTokenRead(err error) bool { return errors.Is(err, ghauth.ErrNoToken) }

// isUnauthorized reports whether err is an authentication failure: the GitHub
// client's ErrUnauthorized, or a scale set API error whose response was a 401,
// which actions/scaleset v0.5.0 wraps in its UnauthorizedError. A 401 on the
// message queue is the queue token expiring, which the library refreshes
// itself, so MessageQueueTokenExpiredError never counts; it is checked first
// because that error also wraps UnauthorizedError.
func isUnauthorized(err error) bool {
	switch {
	case err == nil, errors.Is(err, scaleset.MessageQueueTokenExpiredError):
		return false
	case errors.Is(err, github.ErrUnauthorized):
		return true
	}
	return errors.Is(err, scaleset.UnauthorizedError)
}

// isNotFound reports whether a scale set API call answered 404, which
// actions/scaleset wraps in its NotFoundError: the scale set was deleted under
// the pool.
func isNotFound(err error) bool { return errors.Is(err, scaleset.NotFoundError) }

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
