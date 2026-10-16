//go:build unix

package budget

import "testing"

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
