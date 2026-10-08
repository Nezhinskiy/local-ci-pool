// Package github is the small GitHub REST client the pool needs: repository
// discovery, file reads, Actions variables and the runner release. It uses
// net/http only. The token comes from a TokenSource on every request; it is
// never part of an error.
package github

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"sync/atomic"
	"time"
)

const (
	defaultBaseURL = "https://api.github.com"
	apiVersion     = "2022-11-28"
	requestTimeout = 30 * time.Second
	maxBody        = 8 << 20 // an API response or a repository file
	maxPages       = 100
	downloadIdle   = 30 * time.Second
	errSnippet     = 200
)

// Sentinel errors. A *StatusError unwraps to these for the matching status.
var (
	ErrNotFound     = errors.New("github: not found")
	ErrUnauthorized = errors.New("github: unauthorized")
)

// TokenSource supplies the bearer token and is told when GitHub refused it.
type TokenSource interface {
	Token(ctx context.Context) (string, error)
	Invalidate()
}

// StatusError is an unexpected HTTP status. Message is a short, scrubbed
// excerpt of the response body.
type StatusError struct {
	Method  string
	Path    string
	Status  int
	Message string
}

func (e *StatusError) Error() string {
	s := fmt.Sprintf("github: %s %s: status %d", e.Method, e.Path, e.Status)
	if e.Message != "" {
		s += ": " + e.Message
	}
	return s
}

func (e *StatusError) Unwrap() error {
	switch e.Status {
	case http.StatusNotFound:
		return ErrNotFound
	case http.StatusUnauthorized:
		return ErrUnauthorized
	}
	return nil
}

// Repo is the part of a repository the pool reads.
type Repo struct {
	FullName      string
	Name          string
	DefaultBranch string
	Private       bool
}

// RunnerRelease describes one actions/runner release asset for one
// architecture, with the hash GitHub publishes in two places.
type RunnerRelease struct {
	Version      string // without the leading "v"
	URL          string // the asset's download URL
	DigestSHA256 string // the asset's API `digest`, hex
	BodySHA256   string // the release body's marker, hex
}

// Client talks to the GitHub REST API.
type Client struct {
	tok      TokenSource
	base     string
	http     *http.Client
	download *http.Client
	// downloadIdle is how long a download may deliver no bytes before it is cut.
	downloadIdle time.Duration
}

// New returns a Client. An empty baseURL means https://api.github.com.
func New(tok TokenSource, baseURL string) *Client {
	if baseURL == "" {
		baseURL = defaultBaseURL
	}
	// The download client has no overall timeout, because a runner tarball is
	// large; the context bounds it, and a server that stops answering is cut
	// off at the response header.
	tr := http.DefaultTransport.(*http.Transport).Clone()
	tr.ResponseHeaderTimeout = requestTimeout
	return &Client{
		tok:      tok,
		base:     strings.TrimRight(baseURL, "/"),
		http:     &http.Client{Timeout: requestTimeout},
		download: &http.Client{Transport: tr},

		downloadIdle: downloadIdle,
	}
}

type result struct {
	status int
	header http.Header
	body   []byte
	tok    string // for scrubbing error text
}

// do sends one request. On a 401 it invalidates the token and retries once;
// the second 401 comes back to the caller like any other status.
func (c *Client) do(ctx context.Context, method, rawURL, accept string, payload any) (*result, error) {
	var body []byte
	if payload != nil {
		b, err := json.Marshal(payload)
		if err != nil {
			return nil, fmt.Errorf("github: encoding the request: %w", err)
		}
		body = b
	}
	for attempt := 0; ; attempt++ {
		tok, err := c.tok.Token(ctx)
		if err != nil {
			return nil, fmt.Errorf("github: %s %s: %w", method, c.displayPath(rawURL), err)
		}
		res, err := c.once(ctx, method, rawURL, accept, body, tok)
		if err != nil {
			return nil, err
		}
		if res.status == http.StatusUnauthorized && attempt == 0 {
			c.tok.Invalidate()
			continue
		}
		return res, nil
	}
}

func (c *Client) once(ctx context.Context, method, rawURL, accept string, body []byte, tok string) (*result, error) {
	var rd io.Reader
	if body != nil {
		rd = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, method, rawURL, rd)
	if err != nil {
		return nil, fmt.Errorf("github: building %s %s: %w", method, c.displayPath(rawURL), err)
	}
	req.Header.Set("Authorization", "Bearer "+tok)
	if accept == "" {
		accept = "application/vnd.github+json"
	}
	req.Header.Set("Accept", accept)
	req.Header.Set("X-GitHub-Api-Version", apiVersion)
	req.Header.Set("User-Agent", "local-ci-pool")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("github: %s %s: %s", method, c.displayPath(rawURL), scrub(err.Error(), tok))
	}
	defer func() { _ = resp.Body.Close() }()
	b, err := io.ReadAll(io.LimitReader(resp.Body, maxBody+1))
	if err != nil {
		return nil, fmt.Errorf("github: reading %s %s: %s", method, c.displayPath(rawURL), scrub(err.Error(), tok))
	}
	if len(b) > maxBody {
		return nil, fmt.Errorf("github: %s %s: response larger than %d bytes", method, c.displayPath(rawURL), maxBody)
	}
	return &result{status: resp.StatusCode, header: resp.Header, body: b, tok: tok}, nil
}

func (c *Client) displayPath(rawURL string) string {
	return strings.TrimPrefix(rawURL, c.base)
}

func scrub(s, tok string) string {
	if tok != "" {
		s = strings.ReplaceAll(s, tok, "[redacted]")
	}
	return s
}

func (c *Client) statusErr(method, rawURL string, res *result) error {
	msg := strings.Join(strings.Fields(scrub(string(res.body), res.tok)), " ")
	if len(msg) > errSnippet {
		msg = msg[:errSnippet] + "..."
	}
	return &StatusError{Method: method, Path: c.displayPath(rawURL), Status: res.status, Message: msg}
}

// get fetches rawURL and requires a 2xx status.
func (c *Client) get(ctx context.Context, rawURL, accept string) (*result, error) {
	res, err := c.do(ctx, http.MethodGet, rawURL, accept, nil)
	if err != nil {
		return nil, err
	}
	if res.status < 200 || res.status > 299 {
		return nil, c.statusErr(http.MethodGet, rawURL, res)
	}
	return res, nil
}

type repoJSON struct {
	FullName      string `json:"full_name"`
	Name          string `json:"name"`
	DefaultBranch string `json:"default_branch"`
	Private       bool   `json:"private"`
}

var linkNext = regexp.MustCompile(`<([^>]+)>\s*;\s*rel="next"`)

// ListPrivateOwnedRepos lists the private repositories the token's account
// owns, following Link rel="next" until the last page.
func (c *Client) ListPrivateOwnedRepos(ctx context.Context) ([]Repo, error) {
	base, err := url.Parse(c.base)
	if err != nil {
		return nil, fmt.Errorf("github: bad base URL: %w", err)
	}
	next := c.base + "/user/repos?visibility=private&affiliation=owner&per_page=100"
	var out []Repo
	for page := 0; next != ""; page++ {
		if page >= maxPages {
			return nil, fmt.Errorf("github: listing repositories: more than %d pages", maxPages)
		}
		res, err := c.get(ctx, next, "")
		if err != nil {
			return nil, err
		}
		var rs []repoJSON
		if err := json.Unmarshal(res.body, &rs); err != nil {
			return nil, fmt.Errorf("github: decoding the repository list: %w", err)
		}
		for _, r := range rs {
			out = append(out, Repo(r))
		}
		next = ""
		if m := linkNext.FindStringSubmatch(strings.Join(res.header.Values("Link"), ",")); m != nil {
			u, err := url.Parse(m[1])
			if err != nil {
				return nil, fmt.Errorf("github: bad Link header: %w", err)
			}
			if u.Scheme != base.Scheme || u.Host != base.Host {
				// The token goes with every request: never to another host.
				return nil, fmt.Errorf("github: refusing to follow a Link to the foreign host %q", u.Host)
			}
			next = u.String()
		}
	}
	return out, nil
}

var fullNameRE = regexp.MustCompile(`^[A-Za-z0-9._-]+/[A-Za-z0-9._-]+$`)

func checkFull(full string) error {
	if !fullNameRE.MatchString(full) {
		return fmt.Errorf("github: %q is not an owner/repo name", full)
	}
	for _, part := range strings.Split(full, "/") {
		if part == "." || part == ".." {
			return fmt.Errorf("github: %q is not an owner/repo name", full)
		}
	}
	return nil
}

// Repo fetches one repository.
func (c *Client) Repo(ctx context.Context, full string) (Repo, error) {
	if err := checkFull(full); err != nil {
		return Repo{}, err
	}
	res, err := c.get(ctx, c.base+"/repos/"+full, "")
	if err != nil {
		return Repo{}, err
	}
	var r repoJSON
	if err := json.Unmarshal(res.body, &r); err != nil {
		return Repo{}, fmt.Errorf("github: decoding repository %s: %w", full, err)
	}
	return Repo(r), nil
}

// ReadFile returns the raw content of path at ref ("" for the default
// branch). A missing file is ErrNotFound.
func (c *Client) ReadFile(ctx context.Context, full, ref, path string) ([]byte, error) {
	if err := checkFull(full); err != nil {
		return nil, err
	}
	segs := strings.Split(path, "/")
	for i, s := range segs {
		if s == "" || s == "." || s == ".." {
			return nil, fmt.Errorf("github: %q is not a repository file path", path)
		}
		segs[i] = url.PathEscape(s)
	}
	u := c.base + "/repos/" + full + "/contents/" + strings.Join(segs, "/")
	if ref != "" {
		u += "?ref=" + url.QueryEscape(ref)
	}
	res, err := c.get(ctx, u, "application/vnd.github.raw")
	if err != nil {
		return nil, err
	}
	return res.body, nil
}

var (
	versionRE = regexp.MustCompile(`^[0-9]+\.[0-9]+\.[0-9]+$`)
	archRE    = regexp.MustCompile(`^[a-z0-9]+$`)
	hexRE     = regexp.MustCompile(`^[0-9a-f]{64}$`)
)

// LatestRunnerRelease reads the newest actions/runner release for arch (the
// runner's token: "arm64" or "x64"). The result carries the asset's API digest
// and the hash in the release body, which the caller compares with the file.
func (c *Client) LatestRunnerRelease(ctx context.Context, arch string) (RunnerRelease, error) {
	if !archRE.MatchString(arch) {
		return RunnerRelease{}, fmt.Errorf("github: %q is not a runner architecture", arch)
	}
	res, err := c.get(ctx, c.base+"/repos/actions/runner/releases/latest", "")
	if err != nil {
		return RunnerRelease{}, err
	}
	var rel struct {
		Tag    string `json:"tag_name"`
		Body   string `json:"body"`
		Assets []struct {
			Name   string `json:"name"`
			URL    string `json:"browser_download_url"`
			Digest string `json:"digest"`
		} `json:"assets"`
	}
	if err := json.Unmarshal(res.body, &rel); err != nil {
		return RunnerRelease{}, fmt.Errorf("github: decoding the runner release: %w", err)
	}
	version := strings.TrimPrefix(rel.Tag, "v")
	if !versionRE.MatchString(version) {
		return RunnerRelease{}, fmt.Errorf("github: runner release tag %q is not a version", rel.Tag)
	}
	name := "actions-runner-linux-" + arch + "-" + version + ".tar.gz"
	var asset *struct {
		Name   string `json:"name"`
		URL    string `json:"browser_download_url"`
		Digest string `json:"digest"`
	}
	for i := range rel.Assets {
		if rel.Assets[i].Name == name {
			asset = &rel.Assets[i]
			break
		}
	}
	if asset == nil || asset.URL == "" {
		return RunnerRelease{}, fmt.Errorf("github: runner release %s has no asset %s", version, name)
	}
	digest, ok := strings.CutPrefix(asset.Digest, "sha256:")
	if !ok || !hexRE.MatchString(digest) {
		return RunnerRelease{}, fmt.Errorf("github: asset %s has no sha256 digest", name)
	}
	// The marker is inline: `<!-- BEGIN SHA linux-arm64 -->HEX<!-- END SHA
	// linux-arm64 -->`. The exact arch token keeps `linux-arm` out of arm64.
	markerRE := regexp.MustCompile(`<!-- BEGIN SHA linux-` + regexp.QuoteMeta(arch) + ` -->([0-9a-f]{64})<!-- END SHA linux-` + regexp.QuoteMeta(arch) + ` -->`)
	m := markerRE.FindStringSubmatch(rel.Body)
	if m == nil {
		return RunnerRelease{}, fmt.Errorf("github: runner release %s has no SHA marker for linux-%s", version, arch)
	}
	return RunnerRelease{Version: version, URL: asset.URL, DigestSHA256: digest, BodySHA256: m[1]}, nil
}

// Download streams a public release asset. The request carries no credential:
// the asset is public, and the token must not travel to a download host. The
// body has no overall limit, because a runner tarball is large, but a read that
// delivers nothing for downloadIdle (30 s) fails and cancels the request, so a
// stalled server cannot hold the caller forever. The caller may also bound the
// whole download with ctx.
func (c *Client) Download(ctx context.Context, rawURL string) (io.ReadCloser, error) {
	ctx, cancel := context.WithCancel(ctx)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		cancel()
		return nil, fmt.Errorf("github: download: %w", err)
	}
	req.Header.Set("User-Agent", "local-ci-pool")
	resp, err := c.download.Do(req)
	if err != nil {
		cancel()
		return nil, fmt.Errorf("github: download: %w", err)
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		_ = resp.Body.Close()
		cancel()
		return nil, fmt.Errorf("github: download: status %d", resp.StatusCode)
	}
	r := &idleReader{body: resp.Body, cancel: cancel, idle: c.downloadIdle}
	r.timer = time.AfterFunc(r.idle, func() {
		r.stalled.Store(true)
		cancel()
	})
	return r, nil
}

// idleReader fails a read when no data has arrived for the idle period.
type idleReader struct {
	body    io.ReadCloser
	cancel  context.CancelFunc
	timer   *time.Timer
	idle    time.Duration
	stalled atomic.Bool
}

func (r *idleReader) Read(p []byte) (int, error) {
	n, err := r.body.Read(p)
	if r.stalled.Load() {
		return n, fmt.Errorf("github: download stalled: no data for %s", r.idle)
	}
	if n > 0 {
		r.timer.Reset(r.idle)
	}
	return n, err
}

func (r *idleReader) Close() error {
	r.timer.Stop()
	err := r.body.Close()
	r.cancel()
	return err
}

var varNameRE = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

// SetVariable creates or updates an Actions variable: PATCH first, and POST
// when the variable does not exist yet.
func (c *Client) SetVariable(ctx context.Context, full, name, value string) error {
	if err := checkFull(full); err != nil {
		return err
	}
	if !varNameRE.MatchString(name) {
		return fmt.Errorf("github: %q is not a variable name", name)
	}
	payload := map[string]string{"name": name, "value": value}
	patchURL := c.base + "/repos/" + full + "/actions/variables/" + name
	res, err := c.do(ctx, http.MethodPatch, patchURL, "", payload)
	if err != nil {
		return err
	}
	switch {
	case res.status >= 200 && res.status <= 299:
		return nil
	case res.status != http.StatusNotFound:
		return c.statusErr(http.MethodPatch, patchURL, res)
	}
	postURL := c.base + "/repos/" + full + "/actions/variables"
	res, err = c.do(ctx, http.MethodPost, postURL, "", payload)
	if err != nil {
		return err
	}
	if res.status < 200 || res.status > 299 {
		return c.statusErr(http.MethodPost, postURL, res)
	}
	return nil
}

// DeleteVariable removes an Actions variable. A variable that is already gone
// counts as success.
func (c *Client) DeleteVariable(ctx context.Context, full, name string) error {
	if err := checkFull(full); err != nil {
		return err
	}
	if !varNameRE.MatchString(name) {
		return fmt.Errorf("github: %q is not a variable name", name)
	}
	u := c.base + "/repos/" + full + "/actions/variables/" + name
	res, err := c.do(ctx, http.MethodDelete, u, "", nil)
	if err != nil {
		return err
	}
	if res.status == http.StatusNotFound || (res.status >= 200 && res.status <= 299) {
		return nil
	}
	return c.statusErr(http.MethodDelete, u, res)
}
