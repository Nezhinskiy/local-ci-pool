package main

import (
	"runtime/debug"
	"strings"
)

// buildIdentity returns the version and the commit of this binary: the
// -ldflags values when a release build set them, otherwise what the Go
// toolchain recorded ("dev" and the vcs revision for a local build).
func buildIdentity() (ver, rev string) {
	ver, rev = version, commit
	info, ok := debug.ReadBuildInfo()
	if ver == "" {
		ver = "dev"
		if ok && info.Main.Version != "" && info.Main.Version != "(devel)" {
			ver = info.Main.Version
		}
	}
	if rev == "" {
		rev = "unknown"
		if ok {
			rev = vcsRevision(info.Settings)
		}
	}
	return ver, rev
}

func vcsRevision(settings []debug.BuildSetting) string {
	rev, dirty := "", false
	for _, s := range settings {
		switch s.Key {
		case "vcs.revision":
			rev = s.Value
		case "vcs.modified":
			dirty = s.Value == "true"
		}
	}
	if rev == "" {
		return "unknown"
	}
	rev = rev[:min(len(rev), 7)]
	if dirty {
		rev += "-dirty"
	}
	return strings.TrimSpace(rev)
}
