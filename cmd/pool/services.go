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
		Serve:  health.Serve(func() supervisor.Snapshot { return sup.Snapshot() }, log),
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
		sockets := []string{
			"/var/run/docker.sock",
			filepath.Join(home, ".docker", "run", "docker.sock"),
			filepath.Join(home, ".orbstack", "run", "docker.sock"),
			filepath.Join(home, ".colima", "default", "docker.sock"),
		}
		if h := dockerHost(os.Getenv, home, sockets); h != "" {
			opts = append(opts, client.WithHost(h))
		}
	}
	return client.New(opts...)
}

// fetchHealth is GET http://<addr>/healthz. A 503 is an answer, with the body
// that says why; only a pool that does not answer is an error.
func fetchHealth(ctx context.Context, addr string) (int, []byte, error) {
	ctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://"+addr+"/healthz", nil)
	if err != nil {
		return 0, nil, err
	}
	resp, err := http.DefaultClient.Do(req)
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
