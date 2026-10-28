package main

import (
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"testing"
)

// writeContext writes <cfg>/config.json naming current (when not empty) and
// <cfg>/contexts/meta/<sha256 of name>/meta.json holding host, the way the
// docker CLI lays them out.
func writeContext(t *testing.T, cfg, current, name, host string) {
	t.Helper()
	if err := os.MkdirAll(cfg, 0o755); err != nil {
		t.Fatal(err)
	}
	if current != "" {
		body := `{"currentContext":"` + current + `"}`
		if err := os.WriteFile(filepath.Join(cfg, "config.json"), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if name == "" {
		return
	}
	sum := sha256.Sum256([]byte(name))
	dir := filepath.Join(cfg, "contexts", "meta", hex.EncodeToString(sum[:]))
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	meta := `{"Name":"` + name + `","Endpoints":{"docker":{"Host":"` + host + `","SkipTLSVerify":false}}}`
	if err := os.WriteFile(filepath.Join(dir, "meta.json"), []byte(meta), 0o600); err != nil {
		t.Fatal(err)
	}
}

func envOf(m map[string]string) func(string) string {
	return func(k string) string { return m[k] }
}

func touch(t *testing.T, path string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, nil, 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestDockerHostPrefersDockerHost(t *testing.T) {
	home := t.TempDir()
	writeContext(t, filepath.Join(home, ".docker"), "orbstack", "orbstack", "unix:///x/docker.sock")
	sock := filepath.Join(home, "docker.sock")
	touch(t, sock)
	got := dockerHost(envOf(map[string]string{"DOCKER_HOST": "tcp://x:2375", "DOCKER_CONTEXT": "orbstack"}), home, []string{sock})
	if got != "tcp://x:2375" {
		t.Fatalf("got %q, want tcp://x:2375", got)
	}
}

func TestDockerHostFollowsTheCurrentContext(t *testing.T) {
	home := t.TempDir()
	const want = "unix:///Users/u/.orbstack/run/docker.sock"
	writeContext(t, filepath.Join(home, ".docker"), "orbstack", "orbstack", want)
	sock := filepath.Join(home, "other.sock")
	touch(t, sock)
	if got := dockerHost(envOf(nil), home, []string{sock}); got != want {
		t.Fatalf("got %q, want %q", got, want)
	}
}

func TestDockerHostContextEnvBeatsConfig(t *testing.T) {
	home := t.TempDir()
	cfg := filepath.Join(home, ".docker")
	writeContext(t, cfg, "orbstack", "orbstack", "unix:///orb.sock")
	writeContext(t, cfg, "", "colima", "unix:///colima.sock")
	got := dockerHost(envOf(map[string]string{"DOCKER_CONTEXT": "colima"}), home, nil)
	if got != "unix:///colima.sock" {
		t.Fatalf("got %q, want unix:///colima.sock", got)
	}
}

func TestDockerHostHonoursDockerConfig(t *testing.T) {
	home := t.TempDir()
	writeContext(t, filepath.Join(home, ".docker"), "orbstack", "orbstack", "unix:///home.sock")
	cfg := filepath.Join(t.TempDir(), "elsewhere")
	writeContext(t, cfg, "colima", "colima", "unix:///elsewhere.sock")
	got := dockerHost(envOf(map[string]string{"DOCKER_CONFIG": cfg}), home, nil)
	if got != "unix:///elsewhere.sock" {
		t.Fatalf("got %q, want unix:///elsewhere.sock", got)
	}
}

func TestDockerHostKnownSockets(t *testing.T) {
	for _, tc := range []struct {
		name  string
		exist []int // indexes into the four candidate sockets
		want  int   // index of the expected one, -1 for none
	}{
		{"only the first", []int{0}, 0},
		{"only the second", []int{1}, 1},
		{"only the third", []int{2}, 2},
		{"only the fourth", []int{3}, 3},
		{"the first existing wins", []int{2, 3}, 2},
		{"none", nil, -1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			home := t.TempDir()
			socks := []string{
				filepath.Join(home, "var-run", "docker.sock"),
				filepath.Join(home, ".docker", "run", "docker.sock"),
				filepath.Join(home, ".orbstack", "run", "docker.sock"),
				filepath.Join(home, ".colima", "default", "docker.sock"),
			}
			for _, i := range tc.exist {
				touch(t, socks[i])
			}
			want := ""
			if tc.want >= 0 {
				want = "unix://" + socks[tc.want]
			}
			if got := dockerHost(envOf(nil), home, socks); got != want {
				t.Fatalf("got %q, want %q", got, want)
			}
		})
	}
}

func TestDockerHostDefault(t *testing.T) {
	home := t.TempDir()
	if got := dockerHost(envOf(nil), home, []string{filepath.Join(home, "none.sock")}); got != "" {
		t.Fatalf("got %q, want the empty default", got)
	}
}

func TestDockerHostBrokenContextFallsThrough(t *testing.T) {
	home := t.TempDir()
	// currentContext names a context whose meta.json is missing.
	writeContext(t, filepath.Join(home, ".docker"), "ghost", "", "")
	sock := filepath.Join(home, ".orbstack", "run", "docker.sock")
	touch(t, sock)
	if got := dockerHost(envOf(nil), home, []string{sock}); got != "unix://"+sock {
		t.Fatalf("got %q, want the known socket", got)
	}
}
