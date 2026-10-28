package supervisor

import (
	"errors"
	"fmt"
	"testing"

	"github.com/actions/scaleset"

	"github.com/Nezhinskiy/local-ci-pool/internal/github"
)

// The scale set errors below are shaped as actions/scaleset's
// wrapResponseErrorType builds them: the status sentinel, then the request
// text with its status="…".
func TestAuthenticationFailureClassification(t *testing.T) {
	for name, tc := range map[string]struct {
		err  error
		want bool
	}{
		"github client 401": {fmt.Errorf("listing: %w", &github.StatusError{Status: 401}), true},
		"scale set 401": {fmt.Errorf(`failed to get runner registration token on refresh: %w`,
			fmt.Errorf(`%w: request POST http://x/registration-token failed(status="401 Unauthorized"): bad credentials`, scaleset.UnauthorizedError)), true},
		"queue token expired": {fmt.Errorf(`%w: request GET http://x/queue failed(status="401 Unauthorized"): %w: expired`,
			scaleset.UnauthorizedError, scaleset.MessageQueueTokenExpiredError), false},
		// It guards only the order: the queue-token exclusion is checked
		// before UnauthorizedError, which the same error also matches.
		"queue token expired 401 wraps both sentinels": {fmt.Errorf("%w: %w", scaleset.UnauthorizedError, scaleset.MessageQueueTokenExpiredError), false},
		// The status text alone no longer counts: only the typed error does.
		"bare 401 text without the sentinel": {errors.New(`request POST http://x failed(status="401 Unauthorized"): bad credentials`), false},
		"forbidden":                          {errors.New(`request GET http://x failed(status="403 Forbidden"): no`), false},
		"nil":                                {nil, false},
	} {
		if got := isUnauthorized(tc.err); got != tc.want {
			t.Errorf("%s: isUnauthorized = %v, want %v", name, got, tc.want)
		}
	}
}

func TestNotFoundClassification(t *testing.T) {
	for name, tc := range map[string]struct {
		err  error
		want bool
	}{
		"scale set 404": {fmt.Errorf("opening the session: %w",
			fmt.Errorf(`%w: request POST http://x/sessions failed(status="404 Not Found"): gone`, scaleset.NotFoundError)), true},
		"bare 404 text without the sentinel": {errors.New(`request POST http://x/sessions failed(status="404 Not Found"): gone`), false},
		"unauthorized":                       {fmt.Errorf("%w: no", scaleset.UnauthorizedError), false},
		"nil":                                {nil, false},
	} {
		if got := isNotFound(tc.err); got != tc.want {
			t.Errorf("%s: isNotFound = %v, want %v", name, got, tc.want)
		}
	}
}

func TestSessionConflictClassification(t *testing.T) {
	conflict := errors.New(`failed to create message session: request POST http://x/sessions failed(status="409 Conflict"): RunnerScaleSetSessionConflictException: The actions runner scaleset alpha-examplemac already has an active session.`)
	if !isSessionConflict(conflict) {
		t.Error("the measured 409 is not a conflict")
	}
	if isSessionConflict(errors.New(`request POST http://x/sessions failed(status="400 Bad Request"): no`)) || isSessionConflict(nil) {
		t.Error("a non-409 counted as a conflict")
	}
	if err := terminal("x %d", 1); !errors.Is(err, ErrTerminal) || err.Error() != "x 1" {
		t.Errorf("terminal(...) = %q, matches ErrTerminal %v", err, errors.Is(err, ErrTerminal))
	}
}
