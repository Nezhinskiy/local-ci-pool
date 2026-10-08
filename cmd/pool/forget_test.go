package main

import (
	"context"
	"errors"
	"log/slog"
	"sort"
	"sync"
	"testing"

	"github.com/Nezhinskiy/local-ci-pool/internal/github"
)

const goodMarker = `{"image":"example.invalid/ci@sha256:0000000000000000000000000000000000000000000000000000000000000000"}`

type fakeRepo struct {
	private bool
	marker  string
	recheck error
}

type fakeSource struct {
	repos map[string]fakeRepo
}

func (s *fakeSource) ListPrivateOwnedRepos(context.Context) ([]github.Repo, error) {
	var out []github.Repo
	for full, r := range s.repos {
		out = append(out, github.Repo{FullName: full, Name: full[len("example/"):], DefaultBranch: "main", Private: r.private})
	}
	return out, nil
}

func (s *fakeSource) Repo(_ context.Context, full string) (github.Repo, error) {
	r, ok := s.repos[full]
	if !ok {
		return github.Repo{}, github.ErrNotFound
	}
	if r.recheck != nil {
		return github.Repo{}, r.recheck
	}
	return github.Repo{FullName: full, Name: full[len("example/"):], DefaultBranch: "main", Private: r.private}, nil
}

func (s *fakeSource) ReadFile(_ context.Context, full, _, _ string) ([]byte, error) {
	if m := s.repos[full].marker; m != "" {
		return []byte(m), nil
	}
	return nil, github.ErrNotFound
}

type deletion struct{ repo, name string }

type fakeDeleter struct {
	mu   sync.Mutex
	done []deletion
	fail map[string]error
}

func (d *fakeDeleter) DeleteVariable(_ context.Context, repo, name string) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.done = append(d.done, deletion{repo, name})
	return d.fail[repo]
}

func (d *fakeDeleter) repos() []string {
	d.mu.Lock()
	defer d.mu.Unlock()
	var out []string
	for _, x := range d.done {
		out = append(out, x.repo)
	}
	sort.Strings(out)
	return out
}

func forgetServices(src *fakeSource, del *fakeDeleter, machine string) services {
	return services{
		newForgetter: func(*slog.Logger) (forgetter, error) {
			return forgetter{
				source:  src,
				deleter: del,
				machine: func(context.Context) (string, error) { return machine, nil },
			}, nil
		},
	}
}

func TestForgetDeletesOwnVariableOnly(t *testing.T) {
	src := &fakeSource{repos: map[string]fakeRepo{
		"example/alpha":  {private: true, marker: goodMarker},
		"example/beta":   {private: true, marker: goodMarker},
		"example/nomark": {private: true},
		"example/public": {private: false, marker: goodMarker},
	}}
	del := &fakeDeleter{}
	code, stdout, stderr := do(t, forgetServices(src, del, "examplemac"), "forget")
	if code != 0 {
		t.Fatalf("exit %d, stderr %q", code, stderr)
	}
	if got := del.repos(); len(got) != 2 || got[0] != "example/alpha" || got[1] != "example/beta" {
		t.Fatalf("deleted in %v, want the two served repositories only", got)
	}
	for _, d := range del.done {
		if d.name != "CI_POOL_HB_EXAMPLEMAC" {
			t.Errorf("deleted %q in %s: forget must touch this machine's variable alone", d.name, d.repo)
		}
	}
	if stdout == "" {
		t.Error("forget printed nothing")
	}
}

// A repository whose marker could not be read just now may still hold the
// variable, so forget tries it too.
func TestForgetIncludesRepositoriesDiscoveryCouldNotJudge(t *testing.T) {
	src := &fakeSource{repos: map[string]fakeRepo{
		"example/alpha": {private: true, marker: goodMarker},
		"example/flaky": {private: true, recheck: errors.New("502")},
	}}
	del := &fakeDeleter{}
	if code, _, stderr := do(t, forgetServices(src, del, "examplemac"), "forget"); code != 0 {
		t.Fatalf("exit %d, stderr %q", code, stderr)
	}
	if got := del.repos(); len(got) != 2 || got[1] != "example/flaky" {
		t.Fatalf("deleted in %v, want alpha and flaky", got)
	}
}

func TestForgetContinuesPastAFailureAndExitsNonZero(t *testing.T) {
	src := &fakeSource{repos: map[string]fakeRepo{
		"example/alpha": {private: true, marker: goodMarker},
		"example/beta":  {private: true, marker: goodMarker},
	}}
	del := &fakeDeleter{fail: map[string]error{"example/alpha": errors.New("500")}}
	code, _, stderr := do(t, forgetServices(src, del, "examplemac"), "forget")
	if code != 1 {
		t.Fatalf("exit %d, want 1 when a delete failed; stderr %q", code, stderr)
	}
	if got := del.repos(); len(got) != 2 {
		t.Fatalf("deleted in %v, want both attempted", got)
	}
}

func TestForgetWithAnUnusableMachineNameDeletesNothing(t *testing.T) {
	src := &fakeSource{repos: map[string]fakeRepo{"example/alpha": {private: true, marker: goodMarker}}}
	del := &fakeDeleter{}
	code, _, _ := do(t, forgetServices(src, del, "not a machine"), "forget")
	if code == 0 || len(del.done) != 0 {
		t.Fatalf("exit %d, deleted %v; want a failure and no deletion", code, del.done)
	}
}
