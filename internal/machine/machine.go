// Package machine derives what the pool needs to know about the Mac it runs
// on: a short name, a session owner that tells a restart from a second Mac of
// the same name, and how many runner slots Docker's VM can carry.
package machine

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os/exec"
	"regexp"
	"strings"
)

const (
	maxNameLen = 12
	gib        = int64(1) << 30
	maxSlots   = 8
)

// Fingerprint identifies the Docker VM. The pool compares it with its start
// value on every watchdog ping and exits when it changes.
type Fingerprint struct {
	ID   string
	Mem  int64 // bytes
	CPUs int
}

// Machine is this Mac as the pool sees it.
type Machine struct {
	Name  string
	Owner string
	Slots int
	FP    Fingerprint
}

// Name turns LocalHostName into the machine name: lower case, [a-z0-9] only,
// at most 12 characters, and not empty.
func Name(localHostName string) (string, error) {
	var b strings.Builder
	for _, r := range strings.ToLower(localHostName) {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') {
			b.WriteRune(r)
			if b.Len() == maxNameLen {
				break
			}
		}
	}
	if b.Len() == 0 {
		return "", fmt.Errorf("the host name %q has no letters or digits", localHostName)
	}
	return b.String(), nil
}

// Owner is the scale-set session owner: the name plus the first 8 hex of
// sha256(platformUUID). It is computed at run time and never committed.
func Owner(name, platformUUID string) string {
	sum := sha256.Sum256([]byte(platformUUID))
	return name + "-" + hex.EncodeToString(sum[:])[:8]
}

// Slots is the number of runners the Docker VM can carry:
//
//	min(floor((round(mem GiB) - 2) / 4), floor(cpus / 2), 8)
//
// and never below 0. The memory term rounds to the nearest GiB because Docker
// Desktop's VM reports MemTotal slightly below its slider (7.75 GiB at an
// 8 GB slider), and flooring would take a slot away for no reason.
func Slots(memBytes int64, cpus int) int {
	if memBytes <= 0 || cpus <= 0 {
		return 0
	}
	rounded := (memBytes + gib/2) / gib
	n := (rounded - 2) / 4
	if rounded < 2 {
		n = 0
	}
	if c := int64(cpus / 2); c < n {
		n = c
	}
	if n > maxSlots {
		n = maxSlots
	}
	if n < 0 {
		n = 0
	}
	return int(n)
}

// ScaleSetName is "<identity>-<machine>". A name longer than limit is cut and
// gets "-" plus the first 6 hex of sha256 of the full name, so two long names
// with a common prefix stay distinct and the result is stable.
func ScaleSetName(identity, machine string, limit int) string {
	full := identity + "-" + machine
	if len(full) <= limit {
		return full
	}
	sum := sha256.Sum256([]byte(full))
	hash := hex.EncodeToString(sum[:])[:6]
	keep := limit - len(hash) - 1
	if keep < 1 {
		if limit < len(hash) {
			if limit < 0 {
				limit = 0
			}
			return hash[:limit]
		}
		return hash
	}
	prefix := strings.TrimRight(full[:keep], "-")
	if prefix == "" {
		return hash
	}
	return prefix + "-" + hash
}

// RunnerArch maps Docker's Architecture to the actions/runner asset token.
func RunnerArch(dockerArch string) (string, error) {
	switch strings.ToLower(dockerArch) {
	case "aarch64", "arm64":
		return "arm64", nil
	case "x86_64", "amd64":
		return "x64", nil
	}
	return "", fmt.Errorf("unsupported Docker architecture %q", dockerArch)
}

type runFunc func(ctx context.Context, name string, args ...string) ([]byte, error)

func execRun(ctx context.Context, name string, args ...string) ([]byte, error) {
	return exec.CommandContext(ctx, name, args...).Output()
}

var platformUUID = regexp.MustCompile(`"IOPlatformUUID"\s*=\s*"([0-9A-Fa-f-]{8,64})"`)

// Detect reads the host name (scutil) and the platform UUID (ioreg), asks info
// for the Docker VM, and derives the machine. A machine with fewer than one
// slot is returned as it is: refusing to start is the caller's decision.
func Detect(ctx context.Context, info func(ctx context.Context) (Fingerprint, error)) (Machine, error) {
	return detect(ctx, execRun, info)
}

func detect(ctx context.Context, run runFunc, info func(ctx context.Context) (Fingerprint, error)) (Machine, error) {
	out, err := run(ctx, "scutil", "--get", "LocalHostName")
	if err != nil {
		return Machine{}, fmt.Errorf("reading LocalHostName: %w", err)
	}
	name, err := Name(strings.TrimSpace(string(out)))
	if err != nil {
		return Machine{}, fmt.Errorf("deriving the machine name: %w; the host name must contain a letter or a digit", err)
	}
	out, err = run(ctx, "ioreg", "-rd1", "-c", "IOPlatformExpertDevice")
	if err != nil {
		return Machine{}, fmt.Errorf("reading IOPlatformUUID: %w", err)
	}
	m := platformUUID.FindSubmatch(out)
	if m == nil {
		return Machine{}, errors.New("reading IOPlatformUUID: not found in the ioreg output")
	}
	fp, err := info(ctx)
	if err != nil {
		return Machine{}, fmt.Errorf("reading the Docker VM: %w", err)
	}
	return Machine{
		Name:  name,
		Owner: Owner(name, string(m[1])),
		Slots: Slots(fp.Mem, fp.CPUs),
		FP:    fp,
	}, nil
}
