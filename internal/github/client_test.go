package github

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

const testToken = "ghp_TESTTOKENVALUE"

type fakeTokens struct {
	mu          sync.Mutex
	reads       int
	invalidated int
}

func (f *fakeTokens) Token(context.Context) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.reads++
	return testToken, nil
}

func (f *fakeTokens) Invalidate() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.invalidated++
}

func newClient(t *testing.T, h http.HandlerFunc) (*Client, *fakeTokens, *httptest.Server) {
	t.Helper()
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	tok := &fakeTokens{}
	return New(tok, srv.URL), tok, srv
}

func TestListPrivateOwnedReposFollowsLinkNext(t *testing.T) {
	var srvURL string
	c, _, srv := newClient(t, func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Authorization"); got != "Bearer "+testToken {
			t.Errorf("Authorization = %q", got)
		}
		if r.URL.Path != "/user/repos" {
			t.Errorf("path = %q", r.URL.Path)
		}
		switch r.URL.Query().Get("page") {
		case "":
			q := r.URL.Query()
			if q.Get("visibility") != "private" || q.Get("affiliation") != "owner" {
				t.Errorf("first page query = %v", q)
			}
			w.Header().Set("Link", fmt.Sprintf(`<%s/user/repos?page=2>; rel="next", <%s/user/repos?page=2>; rel="last"`, srvURL, srvURL))
			_, _ = io.WriteString(w, `[{"full_name":"o/alpha","name":"alpha","default_branch":"main","private":true}]`)
		case "2":
			_, _ = io.WriteString(w, `[{"full_name":"o/beta","name":"beta","default_branch":"trunk","private":true}]`)
		default:
			t.Errorf("unexpected page %q", r.URL.Query().Get("page"))
		}
	})
	srvURL = srv.URL
	repos, err := c.ListPrivateOwnedRepos(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	want := []Repo{
		{FullName: "o/alpha", Name: "alpha", DefaultBranch: "main", Private: true},
		{FullName: "o/beta", Name: "beta", DefaultBranch: "trunk", Private: true},
	}
	if len(repos) != 2 || repos[0] != want[0] || repos[1] != want[1] {
		t.Fatalf("repos = %+v", repos)
	}
}

func TestPaginationRefusesForeignHost(t *testing.T) {
	var foreignHit bool
	foreign := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		foreignHit = true
		if r.Header.Get("Authorization") != "" {
			t.Error("the token was sent to a foreign host")
		}
		_, _ = io.WriteString(w, `[]`)
	}))
	defer foreign.Close()
	c, _, _ := newClient(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Link", fmt.Sprintf(`<%s/user/repos?page=2>; rel="next"`, foreign.URL))
		_, _ = io.WriteString(w, `[]`)
	})
	_, err := c.ListPrivateOwnedRepos(context.Background())
	if err == nil || !strings.Contains(err.Error(), "foreign") {
		t.Fatalf("want a foreign-host refusal, got %v", err)
	}
	if foreignHit {
		t.Fatal("the client followed a Link to a foreign host")
	}
}

func TestRepoAndReadFile(t *testing.T) {
	c, _, _ := newClient(t, func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/repos/o/alpha":
			_, _ = io.WriteString(w, `{"full_name":"o/alpha","name":"alpha","default_branch":"main","private":false}`)
		case "/repos/o/alpha/contents/.github/local-ci.json":
			if got := r.Header.Get("Accept"); got != "application/vnd.github.raw" {
				t.Errorf("Accept = %q", got)
			}
			if got := r.URL.Query().Get("ref"); got != "feature/x" {
				t.Errorf("ref = %q", got)
			}
			_, _ = io.WriteString(w, `{"image":"x"}`)
		case "/repos/o/alpha/contents/missing.json":
			http.Error(w, `{"message":"Not Found"}`, http.StatusNotFound)
		default:
			t.Errorf("unexpected path %q", r.URL.Path)
			http.NotFound(w, r)
		}
	})
	ctx := context.Background()
	r, err := c.Repo(ctx, "o/alpha")
	if err != nil || r.FullName != "o/alpha" || r.Private {
		t.Fatalf("Repo = %+v, %v", r, err)
	}
	b, err := c.ReadFile(ctx, "o/alpha", "feature/x", ".github/local-ci.json")
	if err != nil || string(b) != `{"image":"x"}` {
		t.Fatalf("ReadFile = %q, %v", b, err)
	}
	_, err = c.ReadFile(ctx, "o/alpha", "main", "missing.json")
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("want ErrNotFound, got %v", err)
	}
}

func TestRepoNamesAreValidated(t *testing.T) {
	c, _, _ := newClient(t, func(w http.ResponseWriter, r *http.Request) {
		t.Errorf("a request was sent for an invalid name: %s", r.URL)
	})
	for _, full := range []string{"", "alpha", "o/alpha/x", "o/../etc", "o/a b", "o/alpha?x=1", "../alpha"} {
		if _, err := c.Repo(context.Background(), full); err == nil {
			t.Errorf("Repo(%q): want an error", full)
		}
	}
	if err := c.SetVariable(context.Background(), "o/alpha", "BAD NAME", "v"); err == nil {
		t.Error("SetVariable with a bad variable name: want an error")
	}
}

type varCall struct {
	method string
	path   string
	body   map[string]string
}

func TestSetVariablePatchThenPost(t *testing.T) {
	run := func(t *testing.T, patchStatus int) []varCall {
		var mu sync.Mutex
		var calls []varCall
		c, _, _ := newClient(t, func(w http.ResponseWriter, r *http.Request) {
			var body map[string]string
			raw, _ := io.ReadAll(r.Body)
			if len(raw) > 0 {
				if err := json.Unmarshal(raw, &body); err != nil {
					t.Errorf("bad JSON body %q: %v", raw, err)
				}
			}
			mu.Lock()
			calls = append(calls, varCall{r.Method, r.URL.Path, body})
			mu.Unlock()
			switch r.Method {
			case http.MethodPatch:
				w.WriteHeader(patchStatus)
			case http.MethodPost:
				w.WriteHeader(http.StatusCreated)
			}
		})
		if err := c.SetVariable(context.Background(), "o/alpha", "CI_POOL_HB_EXAMPLEMAC", "1000 2"); err != nil {
			t.Fatal(err)
		}
		return calls
	}

	t.Run("404 then POST", func(t *testing.T) {
		calls := run(t, http.StatusNotFound)
		if len(calls) != 2 {
			t.Fatalf("calls = %+v", calls)
		}
		if calls[0].method != "PATCH" || calls[0].path != "/repos/o/alpha/actions/variables/CI_POOL_HB_EXAMPLEMAC" {
			t.Errorf("first call = %+v", calls[0])
		}
		p := calls[1]
		if p.method != "POST" || p.path != "/repos/o/alpha/actions/variables" ||
			p.body["name"] != "CI_POOL_HB_EXAMPLEMAC" || p.body["value"] != "1000 2" {
			t.Errorf("second call = %+v", p)
		}
	})
	t.Run("204 means no POST", func(t *testing.T) {
		calls := run(t, http.StatusNoContent)
		if len(calls) != 1 || calls[0].method != "PATCH" || calls[0].body["value"] != "1000 2" {
			t.Fatalf("calls = %+v", calls)
		}
	})
}

func TestDeleteVariableTreats404AsSuccess(t *testing.T) {
	status := http.StatusNotFound
	c, _, _ := newClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodDelete || r.URL.Path != "/repos/o/alpha/actions/variables/CI_POOL_HB_EXAMPLEMAC" {
			t.Errorf("unexpected %s %s", r.Method, r.URL.Path)
		}
		w.WriteHeader(status)
	})
	ctx := context.Background()
	if err := c.DeleteVariable(ctx, "o/alpha", "CI_POOL_HB_EXAMPLEMAC"); err != nil {
		t.Fatalf("404: %v", err)
	}
	status = http.StatusNoContent
	if err := c.DeleteVariable(ctx, "o/alpha", "CI_POOL_HB_EXAMPLEMAC"); err != nil {
		t.Fatalf("204: %v", err)
	}
	status = http.StatusForbidden
	if err := c.DeleteVariable(ctx, "o/alpha", "CI_POOL_HB_EXAMPLEMAC"); err == nil {
		t.Fatal("403 must be an error")
	}
}

func TestUnauthorizedRetriesOnce(t *testing.T) {
	var hits int
	c, tok, _ := newClient(t, func(w http.ResponseWriter, r *http.Request) {
		hits++
		if hits == 1 {
			http.Error(w, `{"message":"Bad credentials"}`, http.StatusUnauthorized)
			return
		}
		_, _ = io.WriteString(w, `{"full_name":"o/alpha","name":"alpha","default_branch":"main","private":true}`)
	})
	if _, err := c.Repo(context.Background(), "o/alpha"); err != nil {
		t.Fatal(err)
	}
	if tok.invalidated != 1 || tok.reads != 2 || hits != 2 {
		t.Fatalf("invalidated=%d reads=%d hits=%d, want 1/2/2", tok.invalidated, tok.reads, hits)
	}
}

func TestSecondUnauthorizedReturnsErrUnauthorized(t *testing.T) {
	var hits int
	c, tok, _ := newClient(t, func(w http.ResponseWriter, r *http.Request) {
		hits++
		http.Error(w, `{"message":"Bad credentials"}`, http.StatusUnauthorized)
	})
	_, err := c.Repo(context.Background(), "o/alpha")
	if !errors.Is(err, ErrUnauthorized) {
		t.Fatalf("want ErrUnauthorized, got %v", err)
	}
	if hits != 2 || tok.invalidated != 1 || tok.reads != 2 {
		t.Fatalf("hits=%d invalidated=%d reads=%d, want 2/1/2", hits, tok.invalidated, tok.reads)
	}
}

func TestTokenNeverInErrors(t *testing.T) {
	// The server echoes the credential it was given in every error body, the
	// worst case for a client that quotes bodies in its errors.
	status := http.StatusInternalServerError
	c, _, _ := newClient(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(status)
		_, _ = io.WriteString(w, "echo: "+r.Header.Get("Authorization"))
	})
	ctx := context.Background()
	calls := map[string]func() error{
		"list":    func() error { _, err := c.ListPrivateOwnedRepos(ctx); return err },
		"repo":    func() error { _, err := c.Repo(ctx, "o/alpha"); return err },
		"read":    func() error { _, err := c.ReadFile(ctx, "o/alpha", "main", "x"); return err },
		"release": func() error { _, err := c.LatestRunnerRelease(ctx, "arm64"); return err },
		"set":     func() error { return c.SetVariable(ctx, "o/alpha", "A", "v") },
		"delete":  func() error { return c.DeleteVariable(ctx, "o/alpha", "A") },
	}
	for _, st := range []int{http.StatusInternalServerError, http.StatusUnauthorized, http.StatusForbidden} {
		status = st
		for name, call := range calls {
			err := call()
			if err == nil {
				t.Errorf("%s/%d: want an error", name, st)
				continue
			}
			if strings.Contains(err.Error(), testToken) {
				t.Errorf("%s/%d: the error contains the token: %v", name, st, err)
			}
		}
	}
}

func TestRequestsTimeOut(t *testing.T) {
	c, _, _ := newClient(t, func(w http.ResponseWriter, r *http.Request) {})
	if c.http.Timeout != requestTimeout || requestTimeout.Seconds() != 30 {
		t.Fatalf("per-request timeout = %v", c.http.Timeout)
	}
}

const (
	arm64Hash = "628b4a7258487b80c1d3c221095a7ded349f5ae0175fd3dfab708d07428f041b"
	armHash   = "1111111111111111111111111111111111111111111111111111111111111111"
	x64Hash   = "2222222222222222222222222222222222222222222222222222222222222222"
)

func releaseJSON(t *testing.T, body string, assets []map[string]string) string {
	t.Helper()
	b, err := json.Marshal(map[string]any{"tag_name": "v2.338.0", "body": body, "assets": assets})
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func TestLatestRunnerReleaseParsesBothDigests(t *testing.T) {
	// The body carries the marker inline, one asset per line, and a `linux-arm`
	// marker with a different hash next to the arm64 one: the arm64 parse must
	// return the arm64 hash.
	body := strings.Join([]string{
		"## Release notes",
		"- actions-runner-linux-arm-2.338.0.tar.gz <!-- BEGIN SHA linux-arm -->" + armHash + "<!-- END SHA linux-arm -->",
		"- actions-runner-linux-arm64-2.338.0.tar.gz <!-- BEGIN SHA linux-arm64 -->" + arm64Hash + "<!-- END SHA linux-arm64 -->",
		"- actions-runner-linux-x64-2.338.0.tar.gz <!-- BEGIN SHA linux-x64 -->" + x64Hash + "<!-- END SHA linux-x64 -->",
	}, "\r\n")
	assets := []map[string]string{
		{"name": "actions-runner-linux-arm-2.338.0.tar.gz", "browser_download_url": "https://example.invalid/arm.tgz", "digest": "sha256:" + armHash},
		{"name": "actions-runner-linux-arm64-2.338.0.tar.gz", "browser_download_url": "https://example.invalid/arm64.tgz", "digest": "sha256:" + arm64Hash},
		{"name": "actions-runner-linux-x64-2.338.0.tar.gz", "browser_download_url": "https://example.invalid/x64.tgz", "digest": "sha256:" + x64Hash},
	}
	c, _, _ := newClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/repos/actions/runner/releases/latest" {
			t.Errorf("path = %q", r.URL.Path)
		}
		_, _ = io.WriteString(w, releaseJSON(t, body, assets))
	})
	rel, err := c.LatestRunnerRelease(context.Background(), "arm64")
	if err != nil {
		t.Fatal(err)
	}
	want := RunnerRelease{Version: "2.338.0", URL: "https://example.invalid/arm64.tgz", DigestSHA256: arm64Hash, BodySHA256: arm64Hash}
	if rel != want {
		t.Fatalf("release = %+v, want %+v", rel, want)
	}
	rel, err = c.LatestRunnerRelease(context.Background(), "x64")
	if err != nil || rel.DigestSHA256 != x64Hash || rel.BodySHA256 != x64Hash {
		t.Fatalf("x64 release = %+v, %v", rel, err)
	}
}

func TestLatestRunnerReleaseRefusesIncompleteReleases(t *testing.T) {
	good := []map[string]string{{"name": "actions-runner-linux-arm64-2.338.0.tar.gz", "browser_download_url": "https://example.invalid/a.tgz", "digest": "sha256:" + arm64Hash}}
	marker := "<!-- BEGIN SHA linux-arm64 -->" + arm64Hash + "<!-- END SHA linux-arm64 -->"
	cases := map[string]struct {
		body   string
		assets []map[string]string
		want   string
	}{
		"no asset":            {marker, nil, "asset"},
		"no digest":           {marker, []map[string]string{{"name": "actions-runner-linux-arm64-2.338.0.tar.gz", "browser_download_url": "https://example.invalid/a.tgz"}}, "digest"},
		"digest not sha256":   {marker, []map[string]string{{"name": "actions-runner-linux-arm64-2.338.0.tar.gz", "browser_download_url": "https://example.invalid/a.tgz", "digest": "sha1:abc"}}, "digest"},
		"no marker":           {"nothing here", good, "marker"},
		"only the arm marker": {"<!-- BEGIN SHA linux-arm -->" + armHash + "<!-- END SHA linux-arm -->", good, "marker"},
		"marker not 64 hex":   {"<!-- BEGIN SHA linux-arm64 -->abc<!-- END SHA linux-arm64 -->", good, "marker"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			c, _, _ := newClient(t, func(w http.ResponseWriter, r *http.Request) {
				_, _ = io.WriteString(w, releaseJSON(t, tc.body, tc.assets))
			})
			_, err := c.LatestRunnerRelease(context.Background(), "arm64")
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("want an error mentioning %q, got %v", tc.want, err)
			}
		})
	}
}

func TestDownloadSendsNoToken(t *testing.T) {
	c, _, _ := newClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "" {
			t.Error("the download carried a credential")
		}
		_, _ = io.WriteString(w, "tarball-bytes")
	})
	rc, err := c.Download(context.Background(), c.base+"/asset.tgz")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = rc.Close() }()
	b, _ := io.ReadAll(rc)
	if string(b) != "tarball-bytes" {
		t.Fatalf("body = %q", b)
	}
}
