package main

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"time"

	"github.com/moby/moby/client"

	"github.com/Nezhinskiy/local-ci-pool/internal/budget"
	"github.com/Nezhinskiy/local-ci-pool/internal/ghauth"
	"github.com/Nezhinskiy/local-ci-pool/internal/github"
	"github.com/Nezhinskiy/local-ci-pool/internal/health"
	"github.com/Nezhinskiy/local-ci-pool/internal/machine"
	"github.com/Nezhinskiy/local-ci-pool/internal/mirror"
	"github.com/Nezhinskiy/local-ci-pool/internal/supervisor"
)

// realServices wires the pool to Docker, GitHub and this Mac. Nothing here
// talks to either of them yet: the clients connect when first used.
func realServices() services {
	return services{
		newPool:      newRealPool,
		newForgetter: newRealForgetter,
		fetchHealth:  fetchHealth,
	}
}

func newRealPool(cfg supervisor.Config, log *slog.Logger) (pool, varWriter, error) {
	docker, err := newDockerClient()
	if err != nil {
		return nil, nil, fmt.Errorf("creating the Docker client: %w", err)
	}
	cache, err := os.UserCacheDir()
	if err != nil {
		return nil, nil, fmt.Errorf("finding the cache directory: %w", err)
	}
	tok := ghauth.NewSource(nil)
	gh := github.New(tok, "")
	var sup *supervisor.Supervisor
	sup = supervisor.New(cfg, supervisor.Deps{
		Docker:  docker,
		GitHub:  gh,
		Token:   tok,
		Clients: supervisor.NewClientFactory(""),
		// The mirrors are derived state: deleting them costs a fetch.
		Mirror: mirror.Mirror{Root: filepath.Join(cache, "local-ci-pool", "mirror")},
		Log:    log,
		Serve:  health.Serve(func() supervisor.Snapshot { return sup.Snapshot() }, supportDir(), log),
		// The settings file's path by the default rule. New reads nothing
		// and opens nothing: coreslot opens at the first admission.
		Budget: budgetDep(budget.New(budget.Config{Log: log})),
	})
	return sup, gh, nil
}

// budgetDep is the gate as the supervisor's Budget: a nil interface for a
// nil gate, never a typed nil inside it.
func budgetDep(g *budget.Gate) supervisor.Budget {
	if g == nil {
		return nil
	}
	return g
}

func newRealForgetter(*slog.Logger) (forgetter, error) {
	gh := github.New(ghauth.NewSource(nil), "")
	return forgetter{source: gh, deleter: gh, machine: machine.LocalName}, nil
}

// newDockerClient connects the way the docker CLI does: under launchd
// DOCKER_HOST is unset, and the socket of Docker Desktop, OrbStack or Colima
// may be reachable only through the current docker context or a path under
// the home directory.
func newDockerClient() (*client.Client, error) {
	opts := []client.Opt{client.FromEnv}
	if home, err := os.UserHomeDir(); err == nil {
		if h := dockerHost(os.Getenv, home, knownSockets(home)); h != "" {
			opts = append(opts, client.WithHost(h))
		}
	}
	return client.New(opts...)
}

// supportDir is the pool's directory under Application Support, where the
// installer puts the binary and the pool its health token; "" when the
// configuration directory is unknown.
func supportDir() string {
	dir, err := os.UserConfigDir()
	if err != nil {
		return ""
	}
	return filepath.Join(dir, "local-ci-pool")
}

// fetchHealth is GET http://<addr>/healthz, with the pool's health token when
// it can be read, for the body with the full reasons. A 503 is an answer, with
// the body that says why; only a pool that does not answer is an error.
func fetchHealth(ctx context.Context, addr string) (int, []byte, error) {
	ctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://"+addr+"/healthz", nil)
	if err != nil {
		return 0, nil, err
	}
	if dir := supportDir(); dir != "" {
		if token := health.ReadToken(dir, addr); token != "" {
			req.Header.Set("Authorization", "Bearer "+token)
		}
	}
	// A redirect is an answer, not something to follow: the address was
	// checked to be loopback, and wherever it sends us was not.
	client := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error {
		return http.ErrUseLastResponse
	}}
	resp, err := client.Do(req)
	if err != nil {
		return 0, nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return 0, nil, err
	}
	return resp.StatusCode, body, nil
}
