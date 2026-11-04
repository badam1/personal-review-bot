// Package version says which build of personal-review-bot is running.
package version

import "runtime/debug"

// Version is stamped at build time: the release tag (the release workflow) or `git describe` (install.sh), with
// -ldflags "-X github.com/badam1/personal-review-bot/internal/version.Version=...".
var Version = ""

// String is the version: the stamped one, else the version Go records (the module version for `go install`, a
// pseudo-version with the commit for a `go build` in a clone), else the commit ("dev (abc1234)", "+dirty" with local
// changes).
func String() string {
	if Version != "" {
		return Version
	}
	info, ok := debug.ReadBuildInfo()
	if !ok {
		return "dev"
	}
	return fromBuildInfo(info)
}

func fromBuildInfo(info *debug.BuildInfo) string {
	if v := info.Main.Version; v != "" && v != "(devel)" {
		return v
	}
	settings := map[string]string{}
	for _, s := range info.Settings {
		settings[s.Key] = s.Value
	}
	rev := settings["vcs.revision"]
	if rev == "" {
		return "dev"
	}
	v := "dev (" + rev[:min(7, len(rev))]
	if settings["vcs.modified"] == "true" {
		v += "+dirty"
	}
	return v + ")"
}
