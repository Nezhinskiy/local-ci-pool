// Package health serves the pool's state over HTTP on a loopback address.
package health

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/Nezhinskiy/local-ci-pool/internal/supervisor"
)

const (
	path            = "/healthz"
	readHeaderLimit = 5 * time.Second
	shutdownLimit   = 5 * time.Second
)

// Handler answers GET /healthz with the Snapshot as JSON: 200 while Docker
// answers and gh gives the pool a token, 503 otherwise. The body is the same
// either way, so the cause of a 503 is in it. Every other path is a 404 and every other method a 405.
//
// The full Snapshot goes only to a request that carries token as a bearer
// token (pool status reads it from TokenPath). Any other request gets the
// redacted one (Redact): a job reaches the endpoint through Docker Desktop's
// host gateway, and a reason can carry another project's build or preflight
// output. An empty token gives every request the redacted body.
//
// A request whose Host header is not a loopback name is refused with 403, so a
// web page cannot read the endpoint through a DNS name that it rebinds to
// 127.0.0.1.
func Handler(snap func() supervisor.Snapshot, token string) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET "+path, func(w http.ResponseWriter, r *http.Request) {
		s := snap()
		if !authorized(r, token) {
			s = Redact(s)
		}
		if s.Projects == nil {
			s.Projects = []supervisor.ProjectHealth{} // an empty list, not null
		}
		body, err := json.Marshal(s)
		if err != nil {
			http.Error(w, "encoding the snapshot", http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		if !s.Docker || !s.Login {
			w.WriteHeader(http.StatusServiceUnavailable)
		}
		_, _ = w.Write(append(body, '\n'))
	})
	return loopbackHost(mux)
}

func authorized(r *http.Request, token string) bool {
	got, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
	return ok && token != "" && subtle.ConstantTimeCompare([]byte(got), []byte(token)) == 1
}

// Redact is the Snapshot without what one project must not read of another:
// each reason is cut to its class, and the image and the stuck jobs are left
// out. The pool's own state, the repositories and the counts stay.
func Redact(s supervisor.Snapshot) supervisor.Snapshot {
	s.Redacted = true
	projects := make([]supervisor.ProjectHealth, 0, len(s.Projects))
	for _, p := range s.Projects {
		p.Reason, p.Image, p.Stuck = p.Class, "", nil
		projects = append(projects, p)
	}
	s.Projects = projects
	return s
}

// TokenPath is the file in dir that holds the token of the pool serving on
// addr. It is keyed by the port, so a probe on another port keeps its own.
func TokenPath(dir, addr string) string {
	_, port, err := net.SplitHostPort(addr)
	if err != nil {
		port = addr
	}
	return filepath.Join(dir, "health-"+port+".token")
}

// ReadToken returns the token of the pool serving on addr, "" when there is
// none to read.
func ReadToken(dir, addr string) string {
	b, err := os.ReadFile(TokenPath(dir, addr))
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(b))
}

// writeToken makes a new random token and writes it to path, mode 0600, by a
// rename so a reader never sees half of it.
func writeToken(path string) (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	token := hex.EncodeToString(b)
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return "", err
	}
	f, err := os.CreateTemp(filepath.Dir(path), ".health-*.tmp")
	if err != nil {
		return "", err
	}
	_, werr := f.WriteString(token + "\n")
	cerr := f.Close()
	if err := errors.Join(werr, cerr, os.Chmod(f.Name(), 0o600)); err != nil {
		_ = os.Remove(f.Name())
		return "", err
	}
	if err := os.Rename(f.Name(), path); err != nil {
		_ = os.Remove(f.Name())
		return "", err
	}
	return token, nil
}

// LoopbackHost reports whether host (a name or an address, without a port) is
// "localhost" or a loopback IP address.
func LoopbackHost(host string) bool {
	if strings.EqualFold(host, "localhost") {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

func loopbackHost(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		host, _, err := net.SplitHostPort(r.Host)
		if err != nil {
			host = strings.Trim(r.Host, "[]") // no port
		}
		if !LoopbackHost(host) {
			http.Error(w, "forbidden host", http.StatusForbidden)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// Serve returns a function with the shape of supervisor.Deps.Serve: it serves
// Handler on the listener the supervisor bound until ctx ends. The listener
// is the pool's single-instance lock, so Serve never opens one of its own.
// Only once the lock is held does it write a new token to TokenPath in dir, so
// a second pool that loses the lock never replaces the token of the first. A
// token that cannot be written leaves pool status the redacted body.
func Serve(snap func() supervisor.Snapshot, dir string, log *slog.Logger) func(ctx context.Context, ln net.Listener) {
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	return func(ctx context.Context, ln net.Listener) {
		token := ""
		if dir != "" {
			var err error
			if token, err = writeToken(TokenPath(dir, ln.Addr().String())); err != nil {
				log.Warn("writing the health token; pool status shows reasons as classes only", "error", err.Error())
			}
		}
		srv := &http.Server{
			Handler:           Handler(snap, token),
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
