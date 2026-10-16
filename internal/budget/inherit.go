//go:build unix

package budget

import (
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"

	"golang.org/x/sys/unix"
)

// maxTokenFDs is the most descriptors CORESLOT_TOKEN_FDS may name, as the coreslot CLI allows.
const maxTokenFDs = 512

// DropInherited closes the inherited coreslot grant and unsets its variables. A process started
// inside `coreslot run` inherits that run's grant: the descriptors CORESLOT_HELD_FD and
// CORESLOT_TOKEN_FDS name, and the variables CORESLOT_GRANTED_* and CORESLOT_UNBUDGETED. Kept,
// they would hold the parent's cores for the pool's whole life and leak into every child.
//
// Only a named descriptor of 3 or more that is a regular file is closed: a standard stream or a
// non-file cannot be a grant's (its number was reused), and closing it could close the runtime's
// own. A value that does not parse closes nothing, and is still unset. Call it first, before
// anything else is opened.
func DropInherited() {
	var fds []int
	if fd, ok := parseFD(os.Getenv("CORESLOT_HELD_FD")); ok {
		fds = append(fds, fd)
	}
	if v, ok := os.LookupEnv("CORESLOT_TOKEN_FDS"); ok {
		if tokens, err := parseTokenFDs(v); err == nil {
			fds = append(fds, tokens...)
		}
	}
	for _, fd := range fds {
		var st unix.Stat_t
		if fd < 3 || unix.Fstat(fd, &st) != nil || st.Mode&unix.S_IFMT != unix.S_IFREG {
			continue
		}
		unix.Close(fd)
	}
	for _, kv := range os.Environ() {
		k, _, _ := strings.Cut(kv, "=")
		if k == "CORESLOT_HELD_FD" || k == "CORESLOT_TOKEN_FDS" || k == "CORESLOT_UNBUDGETED" ||
			strings.HasPrefix(k, "CORESLOT_GRANTED_") {
			os.Unsetenv(k)
		}
	}
}

// parseTokenFDs parses CORESLOT_TOKEN_FDS: `item *("," item)`, each item `fd` or `a-b` with
// a <= b. It refuses more than 512 descriptors in all, before expanding any range, and a
// descriptor named twice. The descriptors are returned in the order listed.
func parseTokenFDs(s string) ([]int, error) {
	type span struct{ a, b int }
	var spans []span
	total := 0
	for _, item := range strings.Split(s, ",") {
		lo, hi, isRange := strings.Cut(item, "-")
		a, ok := parseFD(lo)
		b := a
		if ok && isRange {
			b, ok = parseFD(hi)
		}
		if !ok || b < a {
			return nil, fmt.Errorf("CORESLOT_TOKEN_FDS=%q is malformed", s)
		}
		if total += b - a + 1; total > maxTokenFDs {
			return nil, fmt.Errorf("CORESLOT_TOKEN_FDS=%q names more than %d descriptors", s, maxTokenFDs)
		}
		spans = append(spans, span{a, b})
	}
	seen := make(map[int]bool, total)
	fds := make([]int, 0, total)
	for _, sp := range spans {
		for fd := sp.a; fd <= sp.b; fd++ {
			if seen[fd] {
				return nil, errors.New("CORESLOT_TOKEN_FDS names a descriptor twice")
			}
			seen[fd] = true
			fds = append(fds, fd)
		}
	}
	return fds, nil
}

// parseFD parses one descriptor number: one to nine decimal digits, nothing else.
func parseFD(s string) (int, bool) {
	if s == "" || len(s) > 9 || strings.Trim(s, "0123456789") != "" {
		return 0, false
	}
	n, err := strconv.Atoi(s)
	return n, err == nil
}
