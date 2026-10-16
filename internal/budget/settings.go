//go:build unix

package budget

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

// maxSettingsSize bounds the settings file: a larger one is invalid.
const maxSettingsSize = 64 << 10

// maxCPU bounds the cores one runner may ask for.
const maxCPU = 256

// Settings is the [budget] table of the settings file.
type Settings struct {
	Enabled bool
	CPU     int // valid only when Enabled
}

// settingsFile reads the file on every Load (no cache) and keeps the last valid settings.
type settingsFile struct {
	path func() (string, error)
	log  *slog.Logger

	mu         sync.Mutex
	last       Settings // the settings in force: the last valid read, off when there is none
	lastPath   string   // the path last logged, "" before the first
	lastLogged string   // the error text last logged as invalid, "" when none
}

func newSettingsFile(path func() (string, error), log *slog.Logger) *settingsFile {
	return &settingsFile{path: path, log: log}
}

// Load returns the settings in force and whether the last read was invalid. An absent file is off
// and forgets the last valid settings; an invalid one keeps them.
func (f *settingsFile) Load() (s Settings, invalid bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	p, err := f.path()
	if err == nil {
		if p != f.lastPath {
			f.lastPath = p
			f.log.Info("budget settings", slog.String("path", p))
		}
		var present bool
		if s, present, err = readSettings(p); err == nil {
			f.lastLogged = ""
			if !present {
				s = Settings{}
			}
			f.last = s
			return s, false
		}
	}
	if msg := err.Error(); msg != f.lastLogged {
		f.lastLogged = msg
		f.log.Warn("budget settings invalid", slog.String("error", msg))
	}
	return f.last, true
}

// readSettings reads and checks the file at p. An absent file, a dangling symlink included, is
// not present and not an error.
func readSettings(p string) (s Settings, present bool, err error) {
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
	data, err := io.ReadAll(io.LimitReader(fh, maxSettingsSize+1))
	if err != nil {
		return Settings{}, false, err
	}
	if len(data) > maxSettingsSize {
		return Settings{}, false, fmt.Errorf("%s is larger than %d bytes", p, maxSettingsSize)
	}
	s, err = parseSettings(data)
	return s, true, err
}

// parseSettings checks the [budget] table: exactly the keys enabled (a bool) and cpu (an integer
// in 1..256), case-sensitive; enabled = true needs cpu. Other tables are ignored.
func parseSettings(data []byte) (Settings, error) {
	var doc struct {
		Budget *struct {
			Enabled *bool `toml:"enabled"`
			CPU     *int  `toml:"cpu"`
		} `toml:"budget"`
	}
	md, err := toml.NewDecoder(bytes.NewReader(data)).Decode(&doc)
	if err != nil {
		return Settings{}, err
	}
	// The decoder matches table and key names to fields case-insensitively and reports nothing
	// undecoded for a miscased one, so the names are checked here: a table that is [budget] but
	// for its case, and any name under [budget] but enabled and cpu, are refused.
	for _, k := range md.Keys() {
		if len(k) == 0 {
			continue
		}
		if k[0] != "budget" && strings.EqualFold(k[0], "budget") {
			return Settings{}, fmt.Errorf("unknown table %q: the settings table is [budget]", k[0])
		}
		if len(k) < 2 || k[0] != "budget" {
			continue
		}
		if len(k) != 2 || (k[1] != "enabled" && k[1] != "cpu") {
			return Settings{}, fmt.Errorf("unknown key %q in [budget]: want enabled and cpu", k.String())
		}
	}
	if doc.Budget == nil {
		return Settings{}, nil
	}
	b := doc.Budget
	if b.CPU != nil && (*b.CPU < 1 || *b.CPU > maxCPU) {
		return Settings{}, fmt.Errorf("budget.cpu = %d: want 1..%d", *b.CPU, maxCPU)
	}
	if b.Enabled == nil || !*b.Enabled {
		return Settings{}, nil
	}
	if b.CPU == nil {
		return Settings{}, errors.New("budget.enabled = true needs budget.cpu")
	}
	return Settings{Enabled: true, CPU: *b.CPU}, nil
}
