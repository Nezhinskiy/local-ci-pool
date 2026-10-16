package main

import (
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"golang.org/x/sys/unix"

	"github.com/Nezhinskiy/local-ci-pool/internal/budget"
	"github.com/Nezhinskiy/local-ci-pool/internal/supervisor"
)

// isContextVar reports whether k is a variable of an inherited coreslot
// grant, which `pool run` drops.
func isContextVar(k string) bool {
	return k == "CORESLOT_HELD_FD" || k == "CORESLOT_TOKEN_FDS" || k == "CORESLOT_UNBUDGETED" ||
		strings.HasPrefix(k, "CORESLOT_GRANTED_")
}

// TestMain unsets the coreslot grant context the test binary may inherit
// (the suite may run inside `coreslot run`): every `pool run` here drops it,
// and its variables name descriptors this process does not have.
func TestMain(m *testing.M) {
	for _, kv := range os.Environ() {
		if k, _, _ := strings.Cut(kv, "="); isContextVar(k) {
			_ = os.Unsetenv(k)
		}
	}
	os.Exit(m.Run())
}

// clearContext unsets every coreslot grant context variable for the test,
// restoring them after.
func clearContext(t *testing.T) {
	t.Helper()
	keys := []string{"CORESLOT_HELD_FD", "CORESLOT_TOKEN_FDS", "CORESLOT_UNBUDGETED"}
	for _, kv := range os.Environ() {
		if k, _, _ := strings.Cut(kv, "="); strings.HasPrefix(k, "CORESLOT_GRANTED_") {
			keys = append(keys, k)
		}
	}
	for _, k := range keys {
		t.Setenv(k, "")
		if err := os.Unsetenv(k); err != nil {
			t.Fatal(err)
		}
	}
}

func fdOpen(fd int) bool {
	_, err := unix.FcntlInt(uintptr(fd), unix.F_GETFD, 0)
	return !errors.Is(err, unix.EBADF)
}

// `pool run` drops an inherited coreslot grant before it builds anything:
// when the pool is built, the grant's descriptor is closed and its variables
// are gone, with the budget off (no settings file at all).
func TestRunDropsInheritedContextFirst(t *testing.T) {
	const fd = 200
	clearContext(t)
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, "config"))
	if fdOpen(fd) {
		t.Skipf("fd %d is already open", fd)
	}
	p := filepath.Join(t.TempDir(), "token")
	if err := os.WriteFile(p, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	// Raw descriptors only: an *os.File's finalizer would close the number
	// again after it was reused.
	raw, err := unix.Open(p, unix.O_RDONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	if err := unix.Dup2(raw, fd); err != nil {
		t.Fatal(err)
	}
	if err := unix.Close(raw); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if fdOpen(fd) {
			_ = unix.Close(fd)
		}
	})
	t.Setenv("CORESLOT_TOKEN_FDS", "200")
	t.Setenv("CORESLOT_GRANTED_CPU", "3")

	var fdErr error
	var tokens, granted bool
	svc := services{newPool: func(supervisor.Config, *slog.Logger) (pool, varWriter, error) {
		_, fdErr = unix.FcntlInt(fd, unix.F_GETFD, 0)
		_, tokens = os.LookupEnv("CORESLOT_TOKEN_FDS")
		_, granted = os.LookupEnv("CORESLOT_GRANTED_CPU")
		fp := newFakePool()
		fp.runErr = terminalErr("stop")
		return fp, &recVars{}, nil
	}}
	if code, _, stderr := do(t, svc, "run"); code != 0 {
		t.Fatalf("exit %d, stderr %q", code, stderr)
	}
	if !errors.Is(fdErr, unix.EBADF) {
		t.Errorf("fd %d was still open when the pool was built (F_GETFD error %v)", fd, fdErr)
	}
	if tokens || granted {
		t.Errorf("CORESLOT_TOKEN_FDS set %v, CORESLOT_GRANTED_CPU set %v when the pool was built; want both unset", tokens, granted)
	}
}

// A nil gate is a nil Budget, never a typed nil inside the interface.
func TestBudgetDepNilIsNilInterface(t *testing.T) {
	if b := budgetDep(nil); b != nil {
		t.Fatalf("budgetDep(nil) = %#v, want a nil interface", b)
	}
	if b := budgetDep(budget.New(budget.Config{})); b == nil {
		t.Fatal("budgetDep of a gate is nil")
	}
}
