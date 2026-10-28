package localcipool

import (
	"bufio"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// scalesetMention matches a citation of the upstream module's version in prose
// or in a Go comment: the module path, a space, an optional parenthesis, then a
// semantic version with a leading v.
var scalesetMention = regexp.MustCompile(`actions/scaleset \(?v(\d+\.\d+\.\d+)`)

// goModScalesetVersion is the version go.mod requires, without the "v".
func goModScalesetVersion(t *testing.T) string {
	t.Helper()
	b, err := os.ReadFile("go.mod")
	if err != nil {
		t.Fatal(err)
	}
	m := regexp.MustCompile(`(?m)^\s*(?:require\s+)?github\.com/actions/scaleset\s+v(\d+\.\d+\.\d+)\s*$`).FindStringSubmatch(string(b))
	if m == nil {
		t.Fatal("go.mod has no github.com/actions/scaleset vX.Y.Z line")
	}
	return m[1]
}

// The module is pinned to one exact version. Every place that names it, in the
// docs and in the comments of the code adapted from it, must name the version
// go.mod requires, so a dependency bump that changes only go.mod goes red here.
func TestScalesetPinIsNamedEverywhere(t *testing.T) {
	want := goModScalesetVersion(t)

	var files []string
	err := filepath.WalkDir(".", func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if path != "." && strings.HasPrefix(d.Name(), ".") {
				return filepath.SkipDir
			}
			return nil
		}
		if strings.HasSuffix(path, ".go") {
			files = append(files, path)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	files = append(files, "README.md", filepath.Join("third_party", "NOTICE"))
	docs, err := filepath.Glob(filepath.Join("docs", "*.md"))
	if err != nil {
		t.Fatal(err)
	}
	files = append(files, docs...)

	noticeMentions := 0
	for _, path := range files {
		f, err := os.Open(path)
		if err != nil {
			t.Fatal(err)
		}
		sc := bufio.NewScanner(f)
		sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)
		for n := 1; sc.Scan(); n++ {
			for _, m := range scalesetMention.FindAllStringSubmatch(sc.Text(), -1) {
				if filepath.ToSlash(path) == "third_party/NOTICE" {
					noticeMentions++
				}
				if m[1] != want {
					t.Errorf("%s:%d names actions/scaleset v%s, go.mod requires v%s", filepath.ToSlash(path), n, m[1], want)
				}
			}
		}
		if err := sc.Err(); err != nil {
			t.Fatal(err)
		}
		f.Close()
	}

	readme, err := os.ReadFile("README.md")
	if err != nil {
		t.Fatal(err)
	}
	if line := "Supported upstream: `actions/scaleset v" + want + "`"; !strings.Contains(string(readme), line) {
		t.Errorf("README.md must contain %q", line)
	}
	if noticeMentions == 0 {
		t.Error("third_party/NOTICE must name the actions/scaleset version")
	}
}
