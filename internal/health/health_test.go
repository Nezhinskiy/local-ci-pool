package health

import (
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/Nezhinskiy/local-ci-pool/internal/supervisor"
)

func sample() supervisor.Snapshot {
	return supervisor.Snapshot{
		Version: "v0.1.0", Commit: "abc1234", Machine: "examplemac",
		Slots: 2, InUse: 1, Busy: 1, Docker: true,
		Projects: []supervisor.ProjectHealth{
			{Repo: "example/alpha", Identity: "alpha", Image: "example.invalid/alpha@sha256:0", Healthy: true},
			{Repo: "example/beta", Identity: "beta", Reason: "preflight failed: no CA bundle"},
		},
	}
}

func get(t *testing.T, h http.Handler, method, path string) *httptest.ResponseRecorder {
	t.Helper()
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(method, path, nil))
	return rec
}

func TestHealthzStatusAndShape(t *testing.T) {
	snap := sample()
	rec := get(t, Handler(func() supervisor.Snapshot { return snap }), http.MethodGet, "/healthz")
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
	for _, k := range []string{"version", "commit", "machine", "slots", "in_use", "busy", "docker", "probe", "projects"} {
		if _, ok := fields[k]; !ok {
			t.Errorf("no %q field in %s", k, rec.Body.String())
		}
	}
	if len(fields) != 9 {
		t.Errorf("%d fields, want 9: %s", len(fields), rec.Body.String())
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

func TestHealthzDockerDownIs503WithTheSameBody(t *testing.T) {
	snap := sample()
	snap.Docker = false
	rec := get(t, Handler(func() supervisor.Snapshot { return snap }), http.MethodGet, "/healthz")
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503 while Docker is down", rec.Code)
	}
	var back supervisor.Snapshot
	if err := json.Unmarshal(rec.Body.Bytes(), &back); err != nil || back.Docker || back.Machine != "examplemac" {
		t.Fatalf("body = %s (%v)", rec.Body.String(), err)
	}
}

// Before any project is served the snapshot has none; the operator sees an
// empty list, not null.
func TestHealthzNoProjectsIsAnEmptyList(t *testing.T) {
	rec := get(t, Handler(func() supervisor.Snapshot { return supervisor.Snapshot{Docker: true, Slots: 2} }), http.MethodGet, "/healthz")
	if !strings.Contains(rec.Body.String(), `"projects":[]`) {
		t.Fatalf("body = %s, want \"projects\":[]", rec.Body.String())
	}
}

func TestHealthzOnlyAnswersGetOnItsOwnPath(t *testing.T) {
	h := Handler(func() supervisor.Snapshot { return sample() })
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
		Serve(func() supervisor.Snapshot { return sample() }, nil)(ctx, ln)
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
