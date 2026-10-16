//go:build unix

package budget

import (
	"errors"
	"testing"
)

func TestResolveSettingsPathHome(t *testing.T) {
	noHome := func() (string, error) { return "", errors.New("no home") }
	noUCD := func() (string, error) { return "", errors.New("no user config dir") }
	never := func(string) bool { return false }
	xdg := func(v string) func(string) string {
		return func(k string) string {
			if k == "XDG_CONFIG_HOME" {
				return v
			}
			return ""
		}
	}

	got, err := resolveSettingsPath(xdg("/x"), noHome, noUCD, never)
	if err != nil || got != "/x/local-ci-pool/config.toml" {
		t.Fatalf("absolute XDG_CONFIG_HOME, no home: %q, %v; want /x/local-ci-pool/config.toml, nil", got, err)
	}
	for _, v := range []string{"", "rel"} {
		if got, err := resolveSettingsPath(xdg(v), noHome, noUCD, never); err == nil {
			t.Fatalf("XDG_CONFIG_HOME=%q, no home: %q, nil; want an error", v, got)
		}
	}
	home := func() (string, error) { return "/home/alpha", nil }
	got, err = resolveSettingsPath(xdg(""), home, noUCD, never)
	if err != nil || got != "/home/alpha/.config/local-ci-pool/config.toml" {
		t.Fatalf("home, no user config dir: %q, %v", got, err)
	}
}

func TestSettingsPath(t *testing.T) {
	const (
		home = "/home/alpha"
		ucd  = "/home/alpha/Library/Application Support"
	)
	first := home + "/.config/local-ci-pool/config.toml"
	fallback := ucd + "/local-ci-pool/config.toml"
	for _, tc := range []struct {
		name     string
		xdg      string
		existing []string
		want     string
	}{
		{name: "absolute XDG_CONFIG_HOME", xdg: "/x", want: "/x/local-ci-pool/config.toml"},
		{name: "relative XDG_CONFIG_HOME", xdg: "rel", want: first},
		{name: "unset", want: first},
		{name: "only the user config dir file", existing: []string{fallback}, want: fallback},
		{name: "both", existing: []string{first, fallback}, want: first},
		{name: "neither", want: first},
	} {
		t.Run(tc.name, func(t *testing.T) {
			getenv := func(k string) string {
				if k == "XDG_CONFIG_HOME" {
					return tc.xdg
				}
				return ""
			}
			exists := func(p string) bool {
				for _, e := range tc.existing {
					if e == p {
						return true
					}
				}
				return false
			}
			if got := settingsPath(getenv, home, ucd, exists); got != tc.want {
				t.Fatalf("settingsPath = %q, want %q", got, tc.want)
			}
		})
	}
}
