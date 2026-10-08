package localcipool

import (
	"os"
	"regexp"
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
