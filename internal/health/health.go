// Package health serves the pool's state over HTTP on a loopback address.
package health

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net"
	"net/http"
	"time"

	"github.com/Nezhinskiy/local-ci-pool/internal/supervisor"
)

const (
	path            = "/healthz"
	readHeaderLimit = 5 * time.Second
	shutdownLimit   = 5 * time.Second
)

// Handler answers GET /healthz with the Snapshot as JSON: 200 while Docker
// answers, 503 otherwise. The body is the same either way, so the cause of a
// 503 is in it. Every other path is a 404 and every other method a 405.
func Handler(snap func() supervisor.Snapshot) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET "+path, func(w http.ResponseWriter, _ *http.Request) {
		s := snap()
		if s.Projects == nil {
			s.Projects = []supervisor.ProjectHealth{} // an empty list, not null
		}
		body, err := json.Marshal(s)
		if err != nil {
			http.Error(w, "encoding the snapshot", http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		if !s.Docker {
			w.WriteHeader(http.StatusServiceUnavailable)
		}
		_, _ = w.Write(append(body, '\n'))
	})
	return mux
}

// Serve returns a function with the shape of supervisor.Deps.Serve: it serves
// Handler on the listener the supervisor bound until ctx ends. The listener
// is the pool's single-instance lock, so Serve never opens one of its own.
func Serve(snap func() supervisor.Snapshot, log *slog.Logger) func(ctx context.Context, ln net.Listener) {
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	return func(ctx context.Context, ln net.Listener) {
		srv := &http.Server{
			Handler:           Handler(snap),
			ReadHeaderTimeout: readHeaderLimit,
			ErrorLog:          slog.NewLogLogger(log.Handler(), slog.LevelWarn),
		}
		errc := make(chan error, 1)
		go func() { errc <- srv.Serve(ln) }()
		select {
		case <-ctx.Done():
			sctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), shutdownLimit)
			defer cancel()
			_ = srv.Shutdown(sctx)
			<-errc
		case err := <-errc:
			// The supervisor closed the listener, or it failed.
			if err != nil && !errors.Is(err, http.ErrServerClosed) && ctx.Err() == nil {
				log.Warn("health endpoint stopped", "error", err.Error())
			}
		}
	}
}
