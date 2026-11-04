package version

import (
	"runtime/debug"
	"testing"
)

func TestFromBuildInfo(t *testing.T) {
	build := func(version string, settings ...string) *debug.BuildInfo {
		info := &debug.BuildInfo{Main: debug.Module{Version: version}}
		for i := 0; i+1 < len(settings); i += 2 {
			info.Settings = append(info.Settings, debug.BuildSetting{Key: settings[i], Value: settings[i+1]})
		}
		return info
	}
	cases := map[string]struct {
		info *debug.BuildInfo
		want string
	}{
		"go install @version": {build("v0.1.0-beta.1"), "v0.1.0-beta.1"},
		"go build in a clone": {build("(devel)", "vcs.revision", "ede31dd4f00d", "vcs.modified", "false"), "dev (ede31dd)"},
		"with local changes":  {build("(devel)", "vcs.revision", "ede31dd4f00d", "vcs.modified", "true"), "dev (ede31dd+dirty)"},
		"no version control":  {build("(devel)"), "dev"},
	}
	for name, c := range cases {
		if got := fromBuildInfo(c.info); got != c.want {
			t.Errorf("%s: %q, want %q", name, got, c.want)
		}
	}
}

// A stamped version wins over what the build recorded.
func TestStamped(t *testing.T) {
	defer func(v string) { Version = v }(Version)
	Version = "v1.2.3"
	if got := String(); got != "v1.2.3" {
		t.Errorf("String() = %q", got)
	}
}
