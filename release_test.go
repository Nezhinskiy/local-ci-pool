package localcipool

import (
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"
)

// The released binary is built with the toolchain CI tests and scans with
// (setup-go "stable"), not with the older one go.mod names, and the release
// job does not restore a build cache that a pull request could have written.
func TestReleaseWorkflowBuildsWithStableGoAndNoCache(t *testing.T) {
	b, err := os.ReadFile(".github/workflows/release.yml")
	if err != nil {
		t.Fatal(err)
	}
	release := string(b)
	if !regexp.MustCompile(`(?m)^\s+go-version:\s*stable\s*$`).MatchString(release) {
		t.Error("release.yml: setup-go must use go-version: stable")
	}
	if strings.Contains(release, "go-version-file") {
		t.Error("release.yml: go-version-file would pick the go.mod toolchain, not the one CI scans with")
	}
	if !regexp.MustCompile(`(?m)^\s+cache:\s*false\s*$`).MatchString(release) {
		t.Error("release.yml: setup-go must set cache: false")
	}
	ci, err := os.ReadFile(".github/workflows/ci.yml")
	if err != nil {
		t.Fatal(err)
	}
	if !regexp.MustCompile(`(?m)^\s+go-version:\s*stable\s*$`).Match(ci) {
		t.Error("ci.yml no longer uses go-version: stable, so the release job's premise changed")
	}
}

// A release is built only after the CI checks passed on the tagged commit:
// release.yml calls ci.yml and its release job needs that call.
func TestReleaseRunsTheChecksFirst(t *testing.T) {
	b, err := os.ReadFile(".github/workflows/release.yml")
	if err != nil {
		t.Fatal(err)
	}
	release := string(b)
	if !regexp.MustCompile(`(?m)^  ci:\n    uses: \./\.github/workflows/ci\.yml$`).MatchString(release) {
		t.Error("release.yml: a job named ci must call ./.github/workflows/ci.yml")
	}
	if !regexp.MustCompile(`(?m)^  release:\n    needs: ci$`).MatchString(release) {
		t.Error("release.yml: the release job must need ci")
	}
	ci, err := os.ReadFile(".github/workflows/ci.yml")
	if err != nil {
		t.Fatal(err)
	}
	if !regexp.MustCompile(`(?m)^  workflow_call:$`).Match(ci) {
		t.Error("ci.yml cannot be called: it lacks on.workflow_call")
	}
}

// Every tool version a workflow pins outside a uses: line (go install
// <module>/cmd/<tool>@v..., goreleaser-action's version:) is found by one of
// Renovate's custom managers, with the version as its current value.
func TestRenovateTracksEveryToolPin(t *testing.T) {
	var cfg struct {
		EnabledManagers []string `json:"enabledManagers"`
		CustomManagers  []struct {
			MatchStrings []string `json:"matchStrings"`
		} `json:"customManagers"`
		PackageRules []map[string]any `json:"packageRules"`
	}
	b, err := os.ReadFile(".github/renovate.json")
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(b, &cfg); err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(cfg.EnabledManagers, "custom.regex") {
		t.Error("enabledManagers lacks custom.regex, so the custom managers never run")
	}
	for _, r := range cfg.PackageRules {
		if v, ok := r["groupName"]; ok && v == nil {
			t.Error("a package rule sets groupName to null; the schema wants a string")
		}
	}
	var res []*regexp.Regexp
	for _, m := range cfg.CustomManagers {
		for _, s := range m.MatchStrings {
			res = append(res, regexp.MustCompile(s))
		}
	}
	pin := regexp.MustCompile(`go install \S+@(v\S+)|version: (v[0-9]\S*)`)
	files, _ := filepath.Glob(".github/workflows/*.yml")
	found := 0
	for _, f := range files {
		b, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		for _, m := range pin.FindAllStringSubmatch(string(b), -1) {
			found++
			want := m[1] + m[2]
			tracked := false
			for _, re := range res {
				for _, hit := range re.FindAllStringSubmatch(string(b), -1) {
					if v := hit[re.SubexpIndex("currentValue")]; v == want && strings.Contains(hit[0], strings.TrimSpace(m[0])) {
						tracked = true
					}
				}
			}
			if !tracked {
				t.Errorf("%s: %q is not tracked by any Renovate custom manager", f, m[0])
			}
		}
	}
	if found < 5 {
		t.Fatalf("found %d tool pins, want at least the three go install pins and two goreleaser versions", found)
	}
}
