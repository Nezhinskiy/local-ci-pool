package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/Nezhinskiy/local-ci-pool/internal/supervisor"
)

// cmdStatus prints the state of the pool that is running on this Mac, read
// from its health endpoint. It exits 0 when the pool is healthy, 1 when the
// endpoint answers 503 (Docker down or the login lost), and exitNotRunning
// when nothing answers.
func cmdStatus(ctx context.Context, args []string, stdout, stderr io.Writer, svc services) int {
	fs := flag.NewFlagSet("status", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	asJSON := fs.Bool("json", false, "print the endpoint's body as it is")
	addr := fs.String("health-addr", supervisor.DefaultHealthAddr, "the pool's health endpoint address")
	if err := fs.Parse(args); err != nil {
		return usageError(stderr, err.Error())
	}
	if fs.NArg() != 0 {
		return usageError(stderr, fmt.Sprintf("unexpected argument %q", fs.Arg(0)))
	}
	if err := checkHealthAddr(*addr); err != nil {
		return usageError(stderr, err.Error())
	}

	status, body, err := svc.fetchHealth(ctx, *addr)
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "pool: not running (nothing answers on %s); see launchctl print gui/$(id -u)/com.local-ci-pool.pool\n", *addr)
		return exitNotRunning
	}
	if status != http.StatusOK && status != http.StatusServiceUnavailable {
		_, _ = fmt.Fprintf(stderr, "pool: %s answered HTTP %d, which is not the pool's health endpoint\n", *addr, status)
		return exitError
	}
	code := exitOK
	if status == http.StatusServiceUnavailable {
		code = exitError
	}
	if *asJSON {
		_, _ = stdout.Write(body)
		return code
	}
	var snap supervisor.Snapshot
	if err := json.Unmarshal(body, &snap); err != nil {
		_, _ = fmt.Fprintf(stderr, "pool: %s answered something that is not the pool's state: %v\n", *addr, err)
		return exitError
	}
	_, _ = io.WriteString(stdout, formatStatus(snap))
	return code
}

// formatStatus lays the snapshot out: one line for the pool, one per project
// with the repository column padded to the longest, and a line under a project
// for each job that waits too long for a runner.
func formatStatus(s supervisor.Snapshot) string {
	var b strings.Builder
	docker, login := "ok", "ok"
	if !s.Docker {
		docker = "down"
	}
	if !s.Login {
		login = "lost"
	}
	fmt.Fprintf(&b, "pool %s (%s) on %s: docker %s, login %s, %d slots, %d in use",
		s.Version, s.Commit, s.Machine, docker, login, s.Slots, s.InUse)
	if s.Budget.State != "off" {
		fmt.Fprintf(&b, ", budget %s", s.Budget.State)
	}
	b.WriteByte('\n')

	width := 0
	for _, p := range s.Projects {
		width = max(width, len(p.Repo))
	}
	for _, p := range s.Projects {
		state := "healthy"
		if !p.Healthy {
			state = "unhealthy: " + p.Reason
		}
		if p.Waiting > 0 {
			state += fmt.Sprintf(", %d waiting for cores", p.Waiting)
		}
		fmt.Fprintf(&b, "  %-*s  %s\n", width, p.Repo, state)
		for _, j := range p.Stuck {
			fmt.Fprintf(&b, "    queued %s: %s %s\n", time.Duration(j.QueuedS)*time.Second, j.Job, j.RunURL)
		}
	}
	return b.String()
}
