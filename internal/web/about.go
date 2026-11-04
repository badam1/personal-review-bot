package web

import (
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"

	"github.com/badam1/personal-review-bot/internal/config"
	"github.com/badam1/personal-review-bot/internal/version"
)

// About is what the footer of every page says: which build runs, where to report problems, and where your files are.
type About struct {
	Version, Platform string
	Repo              string // https://github.com/owner/name: UPDATE_REPO, so a fork's footer points to the fork
	ConfigDir         string
	StateDir          string
	Log               string // where the service logs, by OS ("" if unknown)
}

// about is set once by Serve, before the page serves anything, and read by the templates' about function. Tests and
// the default describe the published repo.
var about = newAbout(config.Config{UpdateRepo: "badam1/personal-review-bot"})

func newAbout(cfg config.Config) About {
	a := About{Version: version.String(), Platform: runtime.GOOS + "/" + runtime.GOARCH,
		Repo: "https://github.com/" + cfg.UpdateRepo, ConfigDir: tilde(cfg.ConfigDir), StateDir: tilde(cfg.StateDir)}
	switch runtime.GOOS {
	case "darwin":
		a.Log = "~/Library/Logs/personal-review-bot.log"
	case "linux":
		a.Log = "journalctl --user -u personal-review-bot"
	}
	return a
}

// releaseTag matches a release's own version, not a build after it (git describe adds "-3-gabc1234" or "-dirty").
var releaseTag = regexp.MustCompile(`^v[0-9]+\.[0-9]+\.[0-9]+(-[0-9A-Za-z]+(\.[0-9A-Za-z]+)*)?$`)

// ReleaseURL is the running release's page, or the list of releases for a build between releases.
func (a About) ReleaseURL() string {
	if releaseTag.MatchString(a.Version) {
		return a.Repo + "/releases/tag/" + a.Version
	}
	return a.Repo + "/releases"
}

// BugURL opens the bug report form with the version and platform filled in.
func (a About) BugURL() string {
	return a.Repo + "/issues/new?" + url.Values{"template": {"bug_report.yml"}, "version": {a.Version},
		"platform": {a.Platform}}.Encode()
}

func (a About) IdeaURL() string { return a.Repo + "/issues/new?template=feature_request.yml" }

// tilde shortens a path under your home folder to ~/….
func tilde(path string) string {
	home, err := os.UserHomeDir()
	if err != nil || path == "" {
		return path
	}
	if rel, err := filepath.Rel(home, path); err == nil && !strings.HasPrefix(rel, "..") {
		return filepath.Join("~", rel)
	}
	return path
}
