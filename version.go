package main

import (
	"fmt"
	"runtime/debug"
	"strings"
)

// version is set via -ldflags at build time; falls back to debug.BuildInfo.
var version = "dev"

// versionString returns a human-readable version string including Go version
// and, when version was not overridden at link time, VCS revision and time
// extracted from the embedded build info.
func versionString() string {
	bi, ok := debug.ReadBuildInfo()
	if !ok {
		return "opencrow " + version
	}

	parts := []string{bi.GoVersion}

	if version == "dev" {
		var rev, revTime string

		for _, s := range bi.Settings {
			switch s.Key {
			case "vcs.revision":
				rev = s.Value
			case "vcs.time":
				revTime = s.Value
			}
		}

		if len(rev) > 7 {
			rev = rev[:7]
		}

		if rev != "" {
			parts = append(parts, "rev "+rev)
		}

		if revTime != "" {
			parts = append(parts, revTime)
		}
	}

	return fmt.Sprintf("opencrow %s (%s)", version, strings.Join(parts, ", "))
}
