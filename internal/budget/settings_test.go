//go:build unix

package budget

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

// logRecorder is a slog handler that keeps every record, for asserting exact log lines.
type logRecorder struct {
	mu   sync.Mutex
	recs []slog.Record
}

func (r *logRecorder) Enabled(context.Context, slog.Level) bool { return true }
func (r *logRecorder) WithAttrs([]slog.Attr) slog.Handler       { return r }
func (r *logRecorder) WithGroup(string) slog.Handler            { return r }

func (r *logRecorder) Handle(_ context.Context, rec slog.Record) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.recs = append(r.recs, rec.Clone())
	return nil
}

func (r *logRecorder) logger() *slog.Logger { return slog.New(r) }

// lines returns the attributes of every record with msg, as "k=v k=v", in order.
func (r *logRecorder) lines(msg string) []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	var out []string
	for _, rec := range r.recs {
		if rec.Message == msg {
			out = append(out, attrLine(rec))
		}
	}
	return out
}

func attrLine(rec slog.Record) string {
	var parts []string
	rec.Attrs(func(a slog.Attr) bool {
		parts = append(parts, a.Key+"="+a.Value.String())
		return true
	})
	return strings.Join(parts, " ")
}

// settingsAt returns a settingsFile reading p, and its log.
func settingsAt(p string) (*settingsFile, *logRecorder) {
	rec := &logRecorder{}
	return newSettingsFile(func() (string, error) { return p, nil }, rec.logger()), rec
}

func writeSettings(t *testing.T, p, body string) {
	t.Helper()
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
}

func checkLoad(t *testing.T, f *settingsFile, want Settings, wantInvalid bool) {
	t.Helper()
	got, invalid := f.Load()
	if got != want || invalid != wantInvalid {
		t.Fatalf("Load() = %+v, invalid %v; want %+v, invalid %v", got, invalid, want, wantInvalid)
	}
}

func TestSettingsAbsentIsOff(t *testing.T) {
	dir := t.TempDir()
	f, _ := settingsAt(filepath.Join(dir, "config.toml"))
	checkLoad(t, f, Settings{}, false)

	link := filepath.Join(dir, "link.toml")
	if err := os.Symlink(filepath.Join(dir, "missing.toml"), link); err != nil {
		t.Fatal(err)
	}
	f, _ = settingsAt(link)
	checkLoad(t, f, Settings{}, false)
}

func TestSettingsOn(t *testing.T) {
	p := filepath.Join(t.TempDir(), "config.toml")
	writeSettings(t, p, "[budget]\nenabled = true\ncpu = 3\n")
	f, _ := settingsAt(p)
	checkLoad(t, f, Settings{Enabled: true, CPU: 3}, false)
}

func TestSettingsOffForms(t *testing.T) {
	for _, body := range []string{"", "[other]\nx = 1\n", "[budget]\nenabled = false\n"} {
		p := filepath.Join(t.TempDir(), "config.toml")
		writeSettings(t, p, body)
		f, _ := settingsAt(p)
		got, invalid := f.Load()
		if got != (Settings{}) || invalid {
			t.Errorf("%q: Load() = %+v, invalid %v; want off, valid", body, got, invalid)
		}
	}
}

func TestSettingsInvalid(t *testing.T) {
	for _, body := range []string{
		"[budget]\nenabled = true\ncpu = 0\n",
		"[budget]\nenabled = true\ncpu = 257\n",
		"[budget]\nenabled = true\ncpu = \"3\"\n",
		"[budget]\nenabled = true\ncpu = 03\n",
		"[budget]\nenabled = true\n",
		"[budget]\nenabled = \"yes\"\ncpu = 3\n",
		"[budget]\nenabled = true\ncpu = 3\ncpus = 4\n",
		"[budget]\nenabled = true\nCPU = 3\n",
		"[budget]\nEnabled = true\ncpu = 3\n",
		"not toml\n",
	} {
		p := filepath.Join(t.TempDir(), "config.toml")
		writeSettings(t, p, body)
		f, _ := settingsAt(p)
		got, invalid := f.Load()
		if got != (Settings{}) || !invalid {
			t.Errorf("%q: Load() = %+v, invalid %v; want off, invalid", body, got, invalid)
		}
	}

	p := filepath.Join(t.TempDir(), "config.toml")
	writeSettings(t, p, "[other]\nx = 1\n\n[budget]\nenabled = true\ncpu = 4\n")
	f, _ := settingsAt(p)
	checkLoad(t, f, Settings{Enabled: true, CPU: 4}, false)
}

func TestSettingsBadKinds(t *testing.T) {
	dir := t.TempDir()

	t.Run("directory", func(t *testing.T) {
		p := filepath.Join(dir, "a directory")
		if err := os.Mkdir(p, 0o700); err != nil {
			t.Fatal(err)
		}
		f, _ := settingsAt(p)
		checkLoad(t, f, Settings{}, true)
	})

	t.Run("fifo", func(t *testing.T) {
		p := filepath.Join(dir, "fifo")
		if err := unix.Mkfifo(p, 0o600); err != nil {
			t.Fatal(err)
		}
		f, _ := settingsAt(p)
		done := make(chan struct{})
		var got Settings
		var invalid bool
		go func() {
			got, invalid = f.Load()
			close(done)
		}()
		select {
		case <-done:
		case <-time.After(time.Second):
			t.Fatal("Load on a FIFO did not return within 1 s")
		}
		if got != (Settings{}) || !invalid {
			t.Fatalf("FIFO: Load() = %+v, invalid %v; want off, invalid", got, invalid)
		}
	})

	t.Run("mode 000", func(t *testing.T) {
		if os.Geteuid() == 0 {
			t.Skip("root reads a mode 000 file")
		}
		p := filepath.Join(dir, "unreadable.toml")
		writeSettings(t, p, "[budget]\nenabled = true\ncpu = 3\n")
		if err := os.Chmod(p, 0); err != nil {
			t.Fatal(err)
		}
		f, _ := settingsAt(p)
		checkLoad(t, f, Settings{}, true)
	})

	t.Run("larger than 64 KiB", func(t *testing.T) {
		p := filepath.Join(dir, "large.toml")
		body := "[budget]\nenabled = true\ncpu = 3\n" + strings.Repeat("# padding padding padding padding\n", 2000)
		if len(body) <= 64<<10 {
			t.Fatalf("padded body is %d bytes, not past 64 KiB", len(body))
		}
		writeSettings(t, p, body)
		f, _ := settingsAt(p)
		checkLoad(t, f, Settings{}, true)
	})
}

func TestSettingsLastValidKept(t *testing.T) {
	p := filepath.Join(t.TempDir(), "config.toml")
	f, rec := settingsAt(p)
	invalidLines := func() int { return len(rec.lines("budget settings invalid")) }

	writeSettings(t, p, "[budget]\nenabled = true\ncpu = 3\n")
	checkLoad(t, f, Settings{Enabled: true, CPU: 3}, false)

	writeSettings(t, p, "not toml\n")
	checkLoad(t, f, Settings{Enabled: true, CPU: 3}, true)
	if n := invalidLines(); n != 1 {
		t.Fatalf("after the first invalid read: %d invalid lines, want 1", n)
	}
	checkLoad(t, f, Settings{Enabled: true, CPU: 3}, true)
	if n := invalidLines(); n != 1 {
		t.Fatalf("after the same invalid read again: %d invalid lines, want 1", n)
	}

	writeSettings(t, p, "[budget]\nenabled = true\ncpu = 0\n")
	checkLoad(t, f, Settings{Enabled: true, CPU: 3}, true)
	if n := invalidLines(); n != 2 {
		t.Fatalf("after another invalid text: %d invalid lines, want 2", n)
	}

	writeSettings(t, p, "[budget]\nenabled = true\ncpu = 5\n")
	checkLoad(t, f, Settings{Enabled: true, CPU: 5}, false)

	if err := os.Remove(p); err != nil {
		t.Fatal(err)
	}
	checkLoad(t, f, Settings{}, false)

	writeSettings(t, p, "not toml\n")
	checkLoad(t, f, Settings{}, true)
	if n := invalidLines(); n != 3 {
		t.Fatalf("after a recurrence: %d invalid lines, want 3", n)
	}
}

func TestSettingsReadOnEveryLoad(t *testing.T) {
	p := filepath.Join(t.TempDir(), "config.toml")
	writeSettings(t, p, "[budget]\nenabled = true\ncpu = 3\n")
	f, _ := settingsAt(p)
	checkLoad(t, f, Settings{Enabled: true, CPU: 3}, false)

	fh, err := os.OpenFile(p, os.O_WRONLY|os.O_TRUNC, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := fh.WriteString("[budget]\nenabled = true\ncpu = 12\n"); err != nil {
		t.Fatal(err)
	}
	if err := fh.Close(); err != nil {
		t.Fatal(err)
	}
	checkLoad(t, f, Settings{Enabled: true, CPU: 12}, false)
}

func TestSettingsPathLogged(t *testing.T) {
	dir := t.TempDir()
	p1, p2 := filepath.Join(dir, "one.toml"), filepath.Join(dir, "two.toml")
	paths := []string{p1, p1, p2}
	i := 0
	rec := &logRecorder{}
	f := newSettingsFile(func() (string, error) {
		p := paths[i]
		i++
		return p, nil
	}, rec.logger())
	for range paths {
		f.Load()
	}
	got := rec.lines("budget settings")
	want := []string{"path=" + p1, "path=" + p2}
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("budget settings lines = %q, want %q", got, want)
	}
}
