package health

import (
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/Nezhinskiy/local-ci-pool/internal/budget"
	"github.com/Nezhinskiy/local-ci-pool/internal/supervisor"
)

func sample() supervisor.Snapshot {
	return supervisor.Snapshot{
		Version: "v0.1.0", Commit: "abc1234", Machine: "examplemac",
		Slots: 2, InUse: 1, Busy: 1, Docker: true, Login: true,
		Budget: budget.State{State: "wedged", Error: "state", OldestWaitS: 42},
		Projects: []supervisor.ProjectHealth{
			{Repo: "example/alpha", Identity: "alpha", Image: "example.invalid/alpha@sha256:0", Healthy: true,
				Waiting: 2, Admitted: supervisor.AdmittedHealth{Queued: 1, Adopted: 1, FailOpen: 1}},
			{Repo: "example/beta", Identity: "beta", Image: "local-ci/beta:0123456789ab", Reason: "preflight failed: no CA bundle; build arg SECRET_URL=https://example.invalid/x", Class: "preflight",
				Stuck: []supervisor.StuckJob{{RunURL: "https://github.com/example/beta/actions/runs/1", Job: "deploy", QueuedS: 900}}},
		},
	}
}

const testToken = "0123456789abcdef"

// handler is Handler with the test's token; get sends it.
func handler(snap func() supervisor.Snapshot) http.Handler { return Handler(snap, testToken) }

func get(t *testing.T, h http.Handler, method, path string) *httptest.ResponseRecorder {
	t.Helper()
	return getWith(t, h, method, path, "Bearer "+testToken)
}

func getWith(t *testing.T, h http.Handler, method, path, auth string) *httptest.ResponseRecorder {
	t.Helper()
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(method, "http://127.0.0.1:8737"+path, nil)
	if auth != "" {
		req.Header.Set("Authorization", auth)
	}
	h.ServeHTTP(rec, req)
	return rec
}

// Without the token, or with another, the body keeps the pool's state and
// each project's health but cuts every reason to its class and leaves out the
// image and the stuck jobs: a job reaches the endpoint through the host
// gateway, and must not read another project's build output (L3 of the
// security review).
func TestHealthzWithoutTheTokenIsRedacted(t *testing.T) {
	snap := sample()
	h := handler(func() supervisor.Snapshot { return snap })
	for name, auth := range map[string]string{
		"none":           "",
		"wrong":          "Bearer fedcba9876543210",
		"prefix":         "Bearer " + testToken[:8],
		"not a bearer":   testToken,
		"basic":          "Basic " + testToken,
		"empty bearer":   "Bearer ",
		"token appended": "Bearer " + testToken + "x",
	} {
		t.Run(name, func(t *testing.T) {
			rec := getWith(t, h, http.MethodGet, "/healthz", auth)
			if rec.Code != http.StatusOK {
				t.Fatalf("status = %d, want 200: the status code is not a secret", rec.Code)
			}
			body := rec.Body.String()
			for _, leak := range []string{"SECRET_URL", "no CA bundle", "deploy", "actions/runs", "local-ci/beta", "example.invalid/alpha"} {
				if strings.Contains(body, leak) {
					t.Errorf("the redacted body carries %q: %s", leak, body)
				}
			}
			var back supervisor.Snapshot
			if err := json.Unmarshal(rec.Body.Bytes(), &back); err != nil {
				t.Fatal(err)
			}
			if !back.Redacted || back.Version != "v0.1.0" || back.Budget.State != "wedged" || len(back.Projects) != 2 {
				t.Fatalf("redacted body %+v", back)
			}
			if p := back.Projects[1]; p.Repo != "example/beta" || p.Healthy || p.Reason != "preflight" || p.Class != "preflight" {
				t.Fatalf("beta %+v, want unhealthy with the reason cut to its class", p)
			}
		})
	}
	// The snapshot the handler was given is left alone.
	if snap.Projects[1].Reason == "preflight" {
		t.Fatal("Redact changed the caller's snapshot")
	}
	// An empty token unlocks nothing.
	rec := getWith(t, Handler(func() supervisor.Snapshot { return snap }, ""), http.MethodGet, "/healthz", "Bearer ")
	if strings.Contains(rec.Body.String(), "SECRET_URL") {
		t.Fatalf("an empty token unlocked the full body: %s", rec.Body.String())
	}
}

func TestServeWritesTheTokenOnceItHoldsTheListener(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(t.TempDir(), "support")
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		Serve(func() supervisor.Snapshot { return sample() }, dir, nil)(ctx, ln)
	}()
	defer func() { cancel(); <-done }()
	addr := ln.Addr().String()
	var token string
	deadline := time.Now().Add(5 * time.Second)
	for token == "" && time.Now().Before(deadline) {
		token = ReadToken(dir, addr)
		time.Sleep(10 * time.Millisecond)
	}
	if len(token) != 64 {
		t.Fatalf("token %q, want 64 hex", token)
	}
	fi, err := os.Stat(TokenPath(dir, addr))
	if err != nil || fi.Mode().Perm() != 0o600 {
		t.Fatalf("token file %v, %v; want mode 0600", fi, err)
	}
	if d, err := os.Stat(dir); err != nil || d.Mode().Perm() != 0o700 {
		t.Fatalf("support dir %v, %v; want mode 0700", d, err)
	}
	fetch := func(auth string) string {
		req, _ := http.NewRequest(http.MethodGet, "http://"+addr+"/healthz", nil)
		req.Header.Set("Authorization", auth)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = resp.Body.Close() }()
		b, _ := io.ReadAll(resp.Body)
		return string(b)
	}
	if body := fetch("Bearer " + token); !strings.Contains(body, "SECRET_URL") {
		t.Fatalf("the token did not unlock the full body: %s", body)
	}
	if body := fetch("Bearer nope"); strings.Contains(body, "SECRET_URL") || !strings.Contains(body, `"redacted":true`) {
		t.Fatalf("a wrong token got %s", body)
	}
}

func TestTokenPathIsKeyedByPort(t *testing.T) {
	if a, b := TokenPath("/d", "127.0.0.1:8737"), TokenPath("/d", "localhost:8737"); a != b || a != "/d/health-8737.token" {
		t.Fatalf("%q, %q", a, b)
	}
	if TokenPath("/d", "127.0.0.1:8738") == TokenPath("/d", "127.0.0.1:8737") {
		t.Fatal("two ports share a token file")
	}
	if got := ReadToken(t.TempDir(), "127.0.0.1:8737"); got != "" {
		t.Fatalf("ReadToken with no file = %q", got)
	}
}

func TestHealthzStatusAndShape(t *testing.T) {
	snap := sample()
	rec := get(t, handler(func() supervisor.Snapshot { return snap }), http.MethodGet, "/healthz")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 while Docker is up", rec.Code)
	}
	if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "application/json") {
		t.Fatalf("Content-Type = %q", ct)
	}

	// The field names are the Snapshot's, as the operator's curl and the
	// runbook read them.
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(rec.Body.Bytes(), &fields); err != nil {
		t.Fatal(err)
	}
	for _, k := range []string{"version", "commit", "machine", "slots", "in_use", "busy", "docker", "login", "probe", "budget", "projects"} {
		if _, ok := fields[k]; !ok {
			t.Errorf("no %q field in %s", k, rec.Body.String())
		}
	}
	if len(fields) != 11 {
		t.Errorf("%d fields, want 11: %s", len(fields), rec.Body.String())
	}
	var back supervisor.Snapshot
	if err := json.Unmarshal(rec.Body.Bytes(), &back); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(back, snap) {
		t.Fatalf("round trip = %+v, want %+v", back, snap)
	}
	if !strings.Contains(rec.Body.String(), `"slots":2`) {
		t.Errorf("body %s lacks \"slots\":2", rec.Body.String())
	}
}

// The budget object and the per-project counts have exactly these shapes;
// an error class is omitted when there is none.
func TestHealthzBudgetShape(t *testing.T) {
	snap := sample()
	body := get(t, handler(func() supervisor.Snapshot { return snap }), http.MethodGet, "/healthz").Body.String()
	for _, want := range []string{
		`"budget":{"state":"wedged","error":"state","oldest_wait_s":42}`,
		`"waiting":2,"admitted":{"queued":1,"adopted":1,"fail_open":1}`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("body %s lacks %s", body, want)
		}
	}
	snap.Budget = budget.State{State: "off"}
	body = get(t, handler(func() supervisor.Snapshot { return snap }), http.MethodGet, "/healthz").Body.String()
	if want := `"budget":{"state":"off","oldest_wait_s":0}`; !strings.Contains(body, want) {
		t.Errorf("body %s lacks %s", body, want)
	}
}

func TestHealthzDockerDownIs503WithTheSameBody(t *testing.T) {
	snap := sample()
	snap.Docker = false
	rec := get(t, handler(func() supervisor.Snapshot { return snap }), http.MethodGet, "/healthz")
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503 while Docker is down", rec.Code)
	}
	var back supervisor.Snapshot
	if err := json.Unmarshal(rec.Body.Bytes(), &back); err != nil || back.Docker || back.Machine != "examplemac" {
		t.Fatalf("body = %s (%v)", rec.Body.String(), err)
	}
}

// A pool waiting for gh to give it a token cannot take jobs either.
func TestHealthzNoLoginIs503WithTheSameBody(t *testing.T) {
	snap := sample()
	snap.Login = false
	rec := get(t, handler(func() supervisor.Snapshot { return snap }), http.MethodGet, "/healthz")
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503 without a login", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), `"login":false`) {
		t.Fatalf("body = %s, want \"login\":false", rec.Body.String())
	}
}

// Before any project is served the snapshot has none; the operator sees an
// empty list, not null.
func TestHealthzNoProjectsIsAnEmptyList(t *testing.T) {
	rec := get(t, handler(func() supervisor.Snapshot { return supervisor.Snapshot{Docker: true, Slots: 2} }), http.MethodGet, "/healthz")
	if !strings.Contains(rec.Body.String(), `"projects":[]`) {
		t.Fatalf("body = %s, want \"projects\":[]", rec.Body.String())
	}
}

func TestHealthzOnlyAnswersGetOnItsOwnPath(t *testing.T) {
	h := handler(func() supervisor.Snapshot { return sample() })
	for _, tc := range []struct {
		method, path string
		want         int
	}{
		{http.MethodGet, "/", http.StatusNotFound},
		{http.MethodGet, "/healthz/extra", http.StatusNotFound},
		{http.MethodGet, "/metrics", http.StatusNotFound},
		{http.MethodPost, "/healthz", http.StatusMethodNotAllowed},
		{http.MethodDelete, "/healthz", http.StatusMethodNotAllowed},
		{http.MethodHead, "/healthz", http.StatusOK},
	} {
		if got := get(t, h, tc.method, tc.path).Code; got != tc.want {
			t.Errorf("%s %s = %d, want %d", tc.method, tc.path, got, tc.want)
		}
	}
}

func TestServeAnswersOnTheListenerAndStopsWithContext(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		Serve(func() supervisor.Snapshot { return sample() }, "", nil)(ctx, ln)
	}()
	url := "http://" + ln.Addr().String() + "/healthz"
	resp, err := http.Get(url)
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK || !strings.Contains(string(body), `"machine":"examplemac"`) {
		t.Fatalf("GET = %d %s", resp.StatusCode, body)
	}
	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Serve did not return after its context ended")
	}
	if _, err := http.Get(url); err == nil {
		t.Fatal("the endpoint still answers after Serve returned")
	}
}

func TestHealthzRefusesANonLoopbackHost(t *testing.T) {
	h := handler(func() supervisor.Snapshot { return sample() })
	for host, want := range map[string]int{
		"127.0.0.1:8737":              http.StatusOK,
		"127.0.0.1":                   http.StatusOK,
		"localhost:8737":              http.StatusOK,
		"LOCALHOST:8737":              http.StatusOK,
		"[::1]:8737":                  http.StatusOK,
		"[::1]":                       http.StatusOK,
		"127.1.2.3:8737":              http.StatusOK,
		"rebind.example":              http.StatusForbidden,
		"rebind.example:80":           http.StatusForbidden,
		"192.168.1.20:8737":           http.StatusForbidden,
		"0.0.0.0:8737":                http.StatusForbidden,
		"localhost.evil.example:8737": http.StatusForbidden,
		"":                            http.StatusForbidden,
	} {
		req := httptest.NewRequest(http.MethodGet, "http://127.0.0.1:8737/healthz", nil)
		req.Host = host
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != want {
			t.Errorf("Host %q = %d, want %d", host, rec.Code, want)
		}
		if want == http.StatusForbidden && strings.Contains(rec.Body.String(), "examplemac") {
			t.Errorf("Host %q was refused but the body still leaked the snapshot", host)
		}
	}
}

func TestLoopbackHost(t *testing.T) {
	for host, want := range map[string]bool{
		"localhost": true, "127.0.0.1": true, "::1": true, "127.5.5.5": true,
		"0.0.0.0": false, "": false, "example.com": false, "10.0.0.1": false, "::": false,
	} {
		if got := LoopbackHost(host); got != want {
			t.Errorf("LoopbackHost(%q) = %v, want %v", host, got, want)
		}
	}
}
