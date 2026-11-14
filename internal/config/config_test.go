//go:build unix

package config

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

// levels returns the level of every record with msg, in order.
func (r *logRecorder) levels(msg string) []slog.Level {
	r.mu.Lock()
	defer r.mu.Unlock()
	var out []slog.Level
	for _, rec := range r.recs {
		if rec.Message == msg {
			out = append(out, rec.Level)
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

// fileAt returns a File reading p, and its log.
func fileAt(p string) (*File, *logRecorder) {
	rec := &logRecorder{}
	return NewFile(func() (string, error) { return p, nil }, rec.logger()), rec
}

func write(t *testing.T, p, body string) {
	t.Helper()
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
}

func checkLoad(t *testing.T, f *File, want Settings, wantInvalid bool) {
	t.Helper()
	got, invalid := f.Load()
	if got != want || invalid != wantInvalid {
		t.Fatalf("Load() = %+v, invalid %v; want %+v, invalid %v", got, invalid, want, wantInvalid)
	}
}

func TestAbsentIsNoCap(t *testing.T) {
	dir := t.TempDir()
	f, _ := fileAt(filepath.Join(dir, "config.toml"))
	checkLoad(t, f, Settings{}, false)

	link := filepath.Join(dir, "link.toml")
	if err := os.Symlink(filepath.Join(dir, "missing.toml"), link); err != nil {
		t.Fatal(err)
	}
	f, _ = fileAt(link)
	checkLoad(t, f, Settings{}, false)
}

func TestJobsMax(t *testing.T) {
	p := filepath.Join(t.TempDir(), "config.toml")
	write(t, p, "[jobs]\nmax = 3\n")
	f, _ := fileAt(p)
	checkLoad(t, f, Settings{JobsMax: 3}, false)
}

func TestNoCapForms(t *testing.T) {
	for _, body := range []string{"", "[other]\nx = 1\n", "[jobs]\n", "[Other]\nmax = 1\n"} {
		p := filepath.Join(t.TempDir(), "config.toml")
		write(t, p, body)
		f, _ := fileAt(p)
		got, invalid := f.Load()
		if got != (Settings{}) || invalid {
			t.Errorf("%q: Load() = %+v, invalid %v; want no cap, valid", body, got, invalid)
		}
	}
}

// A value that is not a whole number of at least 1 is invalid, and so is any
// other key under [jobs] or a [jobs] in another case.
func TestInvalid(t *testing.T) {
	for _, body := range []string{
		"[jobs]\nmax = 0\n",
		"[jobs]\nmax = -2\n",
		"[jobs]\nmax = 2.5\n",
		"[jobs]\nmax = 4.0\n",
		"[jobs]\nmax = \"4\"\n",
		"[jobs]\nmax = 04\n",
		"[jobs]\nmax = true\n",
		"[jobs]\nmax = 99999999999999999999\n",
		"[jobs]\nmax = 4\nmin = 1\n",
		"[jobs]\nMax = 4\n",
		"not toml\n",
		"[Jobs]\nmax = 4\n",
		"[jobs]\nmax = 4\n[JOBS]\nmax = 7\n",
		"Jobs.max = 4\n",
	} {
		p := filepath.Join(t.TempDir(), "config.toml")
		write(t, p, body)
		f, _ := fileAt(p)
		got, invalid := f.Load()
		if got != (Settings{}) || !invalid {
			t.Errorf("%q: Load() = %+v, invalid %v; want no cap, invalid", body, got, invalid)
		}
	}

	p := filepath.Join(t.TempDir(), "config.toml")
	write(t, p, "[other]\nx = 1\n\n[jobs]\nmax = 4\n")
	f, _ := fileAt(p)
	checkLoad(t, f, Settings{JobsMax: 4}, false)
}

// A [budget] table left from the removed core budget is reported, not read,
// and not an error: it is warned about once, however often the file is read.
func TestBudgetTableIsWarnedOnce(t *testing.T) {
	p := filepath.Join(t.TempDir(), "config.toml")
	write(t, p, "[budget]\nenabled = true\ncpu = 3\n\n[jobs]\nmax = 2\n")
	f, rec := fileAt(p)
	const warning = "config: [budget] is no longer read; the core budget was removed, set [jobs] max instead"
	for range 3 {
		checkLoad(t, f, Settings{JobsMax: 2, Budget: true}, false)
	}
	if got := rec.levels(warning); len(got) != 1 || got[0] != slog.LevelWarn {
		t.Fatalf("warning levels %v, want one warning", got)
	}
	write(t, p, "[budget]\nanything = \"else\"\n")
	checkLoad(t, f, Settings{Budget: true}, false)
	if n := len(rec.levels(warning)); n != 1 {
		t.Fatalf("%d warnings after another read, want 1", n)
	}
}

func TestBadKinds(t *testing.T) {
	dir := t.TempDir()

	t.Run("directory", func(t *testing.T) {
		p := filepath.Join(dir, "a directory")
		if err := os.Mkdir(p, 0o700); err != nil {
			t.Fatal(err)
		}
		f, _ := fileAt(p)
		checkLoad(t, f, Settings{}, true)
	})

	t.Run("fifo", func(t *testing.T) {
		p := filepath.Join(dir, "fifo")
		if err := unix.Mkfifo(p, 0o600); err != nil {
			t.Fatal(err)
		}
		f, _ := fileAt(p)
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
			t.Fatalf("FIFO: Load() = %+v, invalid %v; want no cap, invalid", got, invalid)
		}
	})

	t.Run("mode 000", func(t *testing.T) {
		if os.Geteuid() == 0 {
			t.Skip("root reads a mode 000 file")
		}
		p := filepath.Join(dir, "unreadable.toml")
		write(t, p, "[jobs]\nmax = 3\n")
		if err := os.Chmod(p, 0); err != nil {
			t.Fatal(err)
		}
		f, _ := fileAt(p)
		checkLoad(t, f, Settings{}, true)
	})

	t.Run("larger than 64 KiB", func(t *testing.T) {
		p := filepath.Join(dir, "large.toml")
		body := "[jobs]\nmax = 3\n" + strings.Repeat("# padding padding padding padding\n", 2000)
		if len(body) <= 64<<10 {
			t.Fatalf("padded body is %d bytes, not past 64 KiB", len(body))
		}
		write(t, p, body)
		f, _ := fileAt(p)
		checkLoad(t, f, Settings{}, true)
	})
}

func TestLastValidKept(t *testing.T) {
	p := filepath.Join(t.TempDir(), "config.toml")
	f, rec := fileAt(p)
	invalidLines := func() int { return len(rec.lines("config invalid; keeping the last valid settings")) }

	write(t, p, "[jobs]\nmax = 3\n")
	checkLoad(t, f, Settings{JobsMax: 3}, false)

	write(t, p, "not toml\n")
	checkLoad(t, f, Settings{JobsMax: 3}, true)
	if n := invalidLines(); n != 1 {
		t.Fatalf("after the first invalid read: %d invalid lines, want 1", n)
	}
	checkLoad(t, f, Settings{JobsMax: 3}, true)
	if n := invalidLines(); n != 1 {
		t.Fatalf("after the same invalid read again: %d invalid lines, want 1", n)
	}

	write(t, p, "[jobs]\nmax = 0\n")
	checkLoad(t, f, Settings{JobsMax: 3}, true)
	if n := invalidLines(); n != 2 {
		t.Fatalf("after another invalid text: %d invalid lines, want 2", n)
	}

	write(t, p, "[jobs]\nmax = 5\n")
	checkLoad(t, f, Settings{JobsMax: 5}, false)

	if err := os.Remove(p); err != nil {
		t.Fatal(err)
	}
	checkLoad(t, f, Settings{}, false)

	write(t, p, "not toml\n")
	checkLoad(t, f, Settings{}, true)
	if n := invalidLines(); n != 3 {
		t.Fatalf("after a recurrence: %d invalid lines, want 3", n)
	}
}

func TestReadOnEveryLoad(t *testing.T) {
	p := filepath.Join(t.TempDir(), "config.toml")
	write(t, p, "[jobs]\nmax = 3\n")
	f, _ := fileAt(p)
	checkLoad(t, f, Settings{JobsMax: 3}, false)

	fh, err := os.OpenFile(p, os.O_WRONLY|os.O_TRUNC, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := fh.WriteString("[jobs]\nmax = 12\n"); err != nil {
		t.Fatal(err)
	}
	if err := fh.Close(); err != nil {
		t.Fatal(err)
	}
	checkLoad(t, f, Settings{JobsMax: 12}, false)
}

func TestPathLogged(t *testing.T) {
	dir := t.TempDir()
	p1, p2 := filepath.Join(dir, "one.toml"), filepath.Join(dir, "two.toml")
	paths := []string{p1, p1, p2}
	i := 0
	rec := &logRecorder{}
	f := NewFile(func() (string, error) {
		p := paths[i]
		i++
		return p, nil
	}, rec.logger())
	for range paths {
		f.Load()
	}
	got := rec.lines("config file")
	want := []string{"path=" + p1, "path=" + p2}
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("config file lines = %q, want %q", got, want)
	}
}

// The TOML decoder's cost grows with the square of the nesting depth, and the file is read on
// every call: a deeply nested file is refused before it is decoded.
func TestDeepNestingRefusedQuickly(t *testing.T) {
	t.Parallel()
	for name, body := range map[string]string{
		"dotted keys":   "a" + strings.Repeat(".b", 4<<10) + " = 1\n",
		"inline tables": "a = " + strings.Repeat("{b = ", 2<<10) + "1" + strings.Repeat("}", 2<<10) + "\n",
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			p := filepath.Join(t.TempDir(), "config.toml")
			write(t, p, body+"[jobs]\nmax = 3\n")
			f, _ := fileAt(p)
			type result struct {
				s       Settings
				invalid bool
			}
			done := make(chan result, 1)
			go func() {
				s, invalid := f.Load()
				done <- result{s, invalid}
			}()
			select {
			case r := <-done:
				if r.s != (Settings{}) || !r.invalid {
					t.Fatalf("Load() = %+v, invalid %v; want no cap, invalid", r.s, r.invalid)
				}
			case <-time.After(100 * time.Millisecond):
				t.Fatal("Load() did not return within 100 ms")
			}
		})
	}
}

// Dots, braces and brackets inside comments and strings are not nesting.
func TestNestingIgnoresCommentsAndStrings(t *testing.T) {
	t.Parallel()
	many := strings.Repeat(".{[", 30)
	body := "# A comment " + many + "\n" +
		"[jobs]\n" +
		"max = 3 # " + many + "\n" +
		"[other]\n" +
		"basic = \"" + many + " \\\" " + many + "\"\n" +
		"literal = '" + many + "'\n" +
		"multi = \"\"\"\n" + many + "\n\"\" " + many + "\n\"\"\"\n" +
		"multiliteral = '''\n" + many + "\n'''\n" +
		"plain = [1, 2, {x = 1.5}]\n"
	p := filepath.Join(t.TempDir(), "config.toml")
	write(t, p, body)
	f, _ := fileAt(p)
	checkLoad(t, f, Settings{JobsMax: 3}, false)
}
