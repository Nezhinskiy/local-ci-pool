package main

import (
	"fmt"
	"log/slog"
	"os"
	"path/filepath"

	"github.com/moby/moby/client"

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
	})
	return sup, gh, nil
}

func newRealForgetter(*slog.Logger) (forgetter, error) {
	gh := github.New(ghauth.NewSource(nil), "")
	return forgetter{source: gh, deleter: gh, machine: machine.LocalName}, nil
}

// newDockerClient connects the way the docker CLI does by default. Under
// launchd DOCKER_HOST is unset, and a Mac whose Docker Desktop does not link
// /var/run/docker.sock has its socket under ~/.docker/run instead.
func newDockerClient() (*client.Client, error) {
	opts := []client.Opt{client.FromEnv}
	if os.Getenv("DOCKER_HOST") == "" {
		if _, err := os.Stat("/var/run/docker.sock"); err != nil {
			if home, herr := os.UserHomeDir(); herr == nil {
				sock := filepath.Join(home, ".docker", "run", "docker.sock")
				if _, err := os.Stat(sock); err == nil {
					opts = append(opts, client.WithHost("unix://"+sock))
				}
			}
		}
	}
	return client.New(opts...)
}
