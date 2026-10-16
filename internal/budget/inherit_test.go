//go:build unix

package budget

import (
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"golang.org/x/sys/unix"
)

func TestParseTokenFDs(t *testing.T) {
	for _, tc := range []struct {
		in   string
		want []int
	}{
		{"4", []int{4}},
		{"4-4", []int{4}},
		{"4-6", []int{4, 5, 6}},
		{"4,7-8", []int{4, 7, 8}},
		{"7-8,4", []int{7, 8, 4}},
	} {
		got, err := parseTokenFDs(tc.in)
		if err != nil || !slices.Equal(got, tc.want) {
			t.Errorf("parseTokenFDs(%q) = %v, %v; want %v", tc.in, got, err, tc.want)
		}
	}
	for _, in := range []string{"", "a", "4-", "-4", "6-4", "4,4", "4-6,5", "0-512", "1234567890"} {
		if got, err := parseTokenFDs(in); err == nil {
			t.Errorf("parseTokenFDs(%q) = %v, nil; want an error", in, got)
		}
	}
}

// contextVars lists the coreslot grant context variables DropInherited unsets.
var contextVars = []string{"CORESLOT_HELD_FD", "CORESLOT_TOKEN_FDS", "CORESLOT_UNBUDGETED"}

// clearInherited unsets every coreslot grant context variable for the test, restoring them after:
// the suite may itself run inside a coreslot grant, whose variables name descriptors this process
// does not have.
func clearInherited(t *testing.T) {
	t.Helper()
	keys := slices.Clone(contextVars)
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

// regularFDAt opens a new regular file and places it at fd, through raw descriptors only: an
// *os.File's finalizer would close the number again after it was reused.
func regularFDAt(t *testing.T, fd int) {
	t.Helper()
	if fdOpen(fd) {
		t.Skipf("fd %d is already open", fd)
	}
	p := filepath.Join(t.TempDir(), fmt.Sprintf("token-%d", fd))
	if err := os.WriteFile(p, nil, 0o600); err != nil {
		t.Fatal(err)
	}
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
		if t.Failed() && fdOpen(fd) {
			unix.Close(fd)
		}
	})
}

func checkUnset(t *testing.T, keys ...string) {
	t.Helper()
	for _, k := range keys {
		if v, ok := os.LookupEnv(k); ok {
			t.Errorf("%s is still set (%q)", k, v)
		}
	}
}

func TestDropInheritedClosesRegularFiles(t *testing.T) {
	clearInherited(t)
	for _, fd := range []int{200, 201, 202, 203} {
		if fdOpen(fd) {
			t.Skipf("fd %d is already open", fd)
		}
	}
	for _, fd := range []int{200, 201, 202, 203} {
		regularFDAt(t, fd)
	}
	t.Setenv("CORESLOT_TOKEN_FDS", "200-202")
	t.Setenv("CORESLOT_HELD_FD", "203")
	t.Setenv("CORESLOT_GRANTED_CPU", "3")
	t.Setenv("CORESLOT_GRANTED_MEM", "2")
	t.Setenv("CORESLOT_UNBUDGETED", "x")

	DropInherited()

	for _, fd := range []int{200, 201, 202, 203} {
		if fdOpen(fd) {
			t.Errorf("fd %d is still open", fd)
		}
	}
	checkUnset(t, "CORESLOT_TOKEN_FDS", "CORESLOT_HELD_FD", "CORESLOT_GRANTED_CPU", "CORESLOT_GRANTED_MEM",
		"CORESLOT_UNBUDGETED")
}

func TestDropInheritedLeavesNonFiles(t *testing.T) {
	clearInherited(t)
	var pipe [2]int
	if err := unix.Pipe(pipe[:]); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		unix.Close(pipe[0])
		unix.Close(pipe[1])
	})
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	sc, err := ln.(*net.TCPListener).SyscallConn()
	if err != nil {
		t.Fatal(err)
	}
	var lfd int
	if err := sc.Control(func(fd uintptr) { lfd = int(fd) }); err != nil {
		t.Fatal(err)
	}
	t.Setenv("CORESLOT_TOKEN_FDS", fmt.Sprintf("2,%d,%d,%d", pipe[0], pipe[1], lfd))

	DropInherited()

	for _, fd := range []int{2, pipe[0], pipe[1], lfd} {
		if !fdOpen(fd) {
			t.Errorf("fd %d was closed", fd)
		}
	}
	checkUnset(t, "CORESLOT_TOKEN_FDS")

	accepted := make(chan error, 1)
	go func() {
		c, err := ln.Accept()
		if err == nil {
			c.Close()
		}
		accepted <- err
	}()
	c, err := net.Dial("tcp", ln.Addr().String())
	if err != nil {
		t.Fatalf("dial the listener: %v", err)
	}
	c.Close()
	if err := <-accepted; err != nil {
		t.Fatalf("the listener no longer accepts: %v", err)
	}
}

func TestDropInheritedMalformed(t *testing.T) {
	clearInherited(t)
	regularFDAt(t, 200)
	for _, tokens := range []string{"200-", "200,200", "200-712"} {
		t.Setenv("CORESLOT_TOKEN_FDS", tokens)
		t.Setenv("CORESLOT_HELD_FD", "x")

		DropInherited()

		if !fdOpen(200) {
			t.Fatalf("CORESLOT_TOKEN_FDS=%q: fd 200 was closed", tokens)
		}
		checkUnset(t, "CORESLOT_TOKEN_FDS", "CORESLOT_HELD_FD")
	}
	unix.Close(200)
}

// A standard stream is never closed, even a regular file: under launchd the pool's stdout and
// stderr are its log file, and a variable naming them must not close the pool's own log.
func TestDropInheritedLeavesAStandardStreamThatIsAFile(t *testing.T) {
	clearInherited(t)
	saved, err := unix.Dup(2)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := unix.Dup2(saved, 2); err != nil {
			t.Errorf("restoring fd 2: %v", err)
		}
		unix.Close(saved)
	})
	p := filepath.Join(t.TempDir(), "pool.log")
	if err := os.WriteFile(p, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	raw, err := unix.Open(p, unix.O_WRONLY|unix.O_APPEND, 0)
	if err != nil {
		t.Fatal(err)
	}
	if err := unix.Dup2(raw, 2); err != nil {
		t.Fatal(err)
	}
	if err := unix.Close(raw); err != nil {
		t.Fatal(err)
	}
	t.Setenv("CORESLOT_TOKEN_FDS", "2")
	t.Setenv("CORESLOT_HELD_FD", "2")

	DropInherited()

	if !fdOpen(2) {
		t.Error("fd 2, a regular file, was closed")
	}
	checkUnset(t, "CORESLOT_TOKEN_FDS", "CORESLOT_HELD_FD")
}
