//go:build unix

// Package config reads the pool's settings file, config.toml (DefaultPath). The pool writes no
// configuration: the file is the owner's, and the pool reads it again on every Load, so an edit
// takes effect without a restart.
package config

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"os"
	"strings"
	"sync"
	"syscall"

	"github.com/BurntSushi/toml"
)

// maxSize bounds the settings file: a larger one is invalid.
const maxSize = 64 << 10

// maxNesting bounds the dots, braces and brackets of the settings file outside its comments and
// strings: a file with more is invalid. The TOML decoder's cost grows with the square of a key's or
// a value's nesting depth (one dotted key filling 64 KiB takes it tens of seconds and gigabytes),
// and the file is read on every Load. The settings need a handful.
const maxNesting = 64

// Settings is what the settings file says.
type Settings struct {
	// JobsMax is [jobs] max: the most pool jobs, container jobs and docker VMs together, that run
	// on the machine at once. 0: no cap beyond the slots.
	JobsMax int
	// Budget is set when the file still has a [budget] table, which nothing reads any more: the
	// core budget was removed.
	Budget bool
}

// File reads the settings file on every Load (no cache) and keeps the last valid settings.
type File struct {
	path func() (string, error)
	log  *slog.Logger

	mu         sync.Mutex
	last       Settings // the settings in force: the last valid read, the zero Settings when there is none
	lastPath   string   // the path last logged, "" before the first
	lastLogged string   // the error text last logged as invalid, "" when none
	warned     bool     // the [budget] table was warned about
}

// NewFile returns a File at the path that path resolves on every Load; nil is DefaultPath.
func NewFile(path func() (string, error), log *slog.Logger) *File {
	if path == nil {
		path = DefaultPath
	}
	return &File{path: path, log: log}
}

// Load returns the settings in force and whether the last read was invalid. An absent file is the
// zero Settings and forgets the last valid ones; an invalid one keeps them. Each new path, each new
// invalid text and the first [budget] table seen are logged once.
func (f *File) Load() (s Settings, invalid bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	p, err := f.path()
	if err == nil {
		if p != f.lastPath {
			f.lastPath = p
			f.log.Info("config file", slog.String("path", p))
		}
		var present bool
		if s, present, err = read(p); err == nil {
			f.lastLogged = ""
			if !present {
				s = Settings{}
			}
			if s.Budget && !f.warned {
				f.warned = true
				f.log.Warn("config: [budget] is no longer read; the core budget was removed, set [jobs] max instead")
			}
			f.last = s
			return s, false
		}
	}
	if msg := err.Error(); msg != f.lastLogged {
		f.lastLogged = msg
		f.log.Warn("config invalid; keeping the last valid settings", slog.String("error", msg))
	}
	return f.last, true
}

// read reads and checks the file at p. An absent file, a dangling symlink included, is not
// present and not an error.
func read(p string) (s Settings, present bool, err error) {
	// O_NONBLOCK: a FIFO at the path is opened at once and refused below, never waited on.
	fh, err := os.OpenFile(p, os.O_RDONLY|syscall.O_NONBLOCK, 0)
	if errors.Is(err, fs.ErrNotExist) {
		return Settings{}, false, nil
	}
	if err != nil {
		return Settings{}, false, err
	}
	defer fh.Close()
	fi, err := fh.Stat()
	if err != nil {
		return Settings{}, false, err
	}
	if !fi.Mode().IsRegular() {
		return Settings{}, false, fmt.Errorf("%s is not a regular file", p)
	}
	data, err := io.ReadAll(io.LimitReader(fh, maxSize+1))
	if err != nil {
		return Settings{}, false, err
	}
	if len(data) > maxSize {
		return Settings{}, false, fmt.Errorf("%s is larger than %d bytes", p, maxSize)
	}
	s, err = parse(data)
	return s, true, err
}

// parse checks the [jobs] table: only the key max, a whole number of at least 1, case-sensitive.
// A [budget] table is reported, never read. Other tables are ignored, a table named jobs in another
// case is refused, and so is a file nested more than maxNesting allows.
func parse(data []byte) (Settings, error) {
	if n := nesting(data); n > maxNesting {
		return Settings{}, fmt.Errorf("%d dots, braces and brackets outside comments and strings: at most %d", n, maxNesting)
	}
	var doc struct {
		Jobs *struct {
			Max *int `toml:"max"`
		} `toml:"jobs"`
	}
	md, err := toml.NewDecoder(bytes.NewReader(data)).Decode(&doc)
	if err != nil {
		return Settings{}, err
	}
	var s Settings
	// The decoder matches table and key names to fields case-insensitively and reports nothing
	// undecoded for a miscased one, so the names are checked here: a table that is [jobs] but for
	// its case, and any name under [jobs] but max, are refused.
	for _, k := range md.Keys() {
		if len(k) == 0 {
			continue
		}
		if k[0] == "budget" {
			s.Budget = true
		}
		if k[0] != "jobs" && strings.EqualFold(k[0], "jobs") {
			return Settings{}, fmt.Errorf("unknown table %q: the table is [jobs]", k[0])
		}
		if len(k) < 2 || k[0] != "jobs" {
			continue
		}
		if len(k) != 2 || k[1] != "max" {
			return Settings{}, fmt.Errorf("unknown key %q in [jobs]: want max", k.String())
		}
	}
	if doc.Jobs != nil && doc.Jobs.Max != nil {
		if *doc.Jobs.Max < 1 {
			return Settings{}, fmt.Errorf("jobs.max = %d: want a whole number of at least 1", *doc.Jobs.Max)
		}
		s.JobsMax = *doc.Jobs.Max
	}
	return s, nil
}

// nesting counts the '.', '{' and '[' of data outside its comments and strings, in one pass.
func nesting(data []byte) int {
	n := 0
	for i := 0; i < len(data); i++ {
		switch c := data[i]; c {
		case '.', '{', '[':
			n++
		case '#':
			for i < len(data) && data[i] != '\n' {
				i++
			}
		case '"', '\'':
			i = stringEnd(data, i)
		}
	}
	return n
}

// stringEnd returns the index of the last quote of the string that opens at data[i]: a basic or a
// literal string, on one line or on several. A single-line string left open ends at its newline,
// a multi-line one at len(data); the decoder then refuses the file. A basic string's backslash
// escapes the byte after it.
func stringEnd(data []byte, i int) int {
	q := data[i]
	multi := i+2 < len(data) && data[i+1] == q && data[i+2] == q
	if multi {
		i += 3
	} else {
		i++
	}
	for ; i < len(data); i++ {
		switch {
		case q == '"' && data[i] == '\\':
			i++
		case data[i] == q && !multi:
			return i
		case data[i] == q && i+2 < len(data) && data[i+1] == q && data[i+2] == q:
			// Up to two more quotes right before the closing three belong to the string.
			i += 2
			for k := 0; k < 2 && i+1 < len(data) && data[i+1] == q; k++ {
				i++
			}
			return i
		case data[i] == '\n' && !multi:
			return i
		}
	}
	return len(data)
}
