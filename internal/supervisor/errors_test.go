package supervisor

import (
	"errors"
	"fmt"
	"testing"

	"github.com/actions/scaleset"

	"github.com/Nezhinskiy/local-ci-pool/internal/github"
)

func TestAuthenticationFailureClassification(t *testing.T) {
	for name, tc := range map[string]struct {
		err  error
		want bool
	}{
		"github client 401":   {fmt.Errorf("listing: %w", &github.StatusError{Status: 401}), true},
		"scale set 401":       {errors.New(`failed to get runner registration token on refresh: request POST http://x/registration-token failed(status="401 Unauthorized"): bad credentials`), true},
		"queue token expired": {fmt.Errorf(`request GET http://x/queue failed(status="401 Unauthorized"): %w: expired`, scaleset.MessageQueueTokenExpiredError), false},
		"forbidden":           {errors.New(`request GET http://x failed(status="403 Forbidden"): no`), false},
		"nil":                 {nil, false},
	} {
		if got := isUnauthorized(tc.err); got != tc.want {
			t.Errorf("%s: isUnauthorized = %v, want %v", name, got, tc.want)
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
