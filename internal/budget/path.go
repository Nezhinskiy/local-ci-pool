//go:build unix

package budget

import (
	"os"
	"path/filepath"
)

// DefaultSettingsPath resolves the settings file's path from the process environment, on every
// call: $XDG_CONFIG_HOME/local-ci-pool/config.toml when XDG_CONFIG_HOME is absolute, otherwise
// ~/.config/local-ci-pool/config.toml; when that file does not exist and the one under
// os.UserConfigDir() does, the latter.
func DefaultSettingsPath() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	ucd, err := os.UserConfigDir()
	if err != nil {
		ucd = ""
	}
	return settingsPath(os.Getenv, home, ucd, func(p string) bool {
		_, err := os.Stat(p)
		return err == nil
	}), nil
}

func settingsPath(getenv func(string) string, home, userConfigDir string, exists func(string) bool) string {
	base := filepath.Join(home, ".config")
	if x := getenv("XDG_CONFIG_HOME"); filepath.IsAbs(x) {
		base = x
	}
	first := filepath.Join(base, "local-ci-pool", "config.toml")
	if userConfigDir == "" {
		return first
	}
	if second := filepath.Join(userConfigDir, "local-ci-pool", "config.toml"); second != first &&
		!exists(first) && exists(second) {
		return second
	}
	return first
}
