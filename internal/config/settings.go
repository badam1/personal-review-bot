package config

import (
	"errors"
	"fmt"
	"io/fs"
	"net"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"github.com/badam1/personal-review-bot/internal/atomicfile"
)

// Defaults are the settings' values when neither the environment nor config.env sets them, written as config.env
// writes them. The directories aren't here: their defaults depend on the home directory.
var Defaults = map[string]string{
	"ORG":                   "",
	"POLL_SECONDS":          "120",
	"THRESHOLD":             "80",
	"AUTO_POST":             "low,medium",
	"APPROVE_TEXT":          "LGTM :+1:",
	"ADDR":                  "127.0.0.1:12346",
	"PARALLEL":              "2",
	"AGENT_TIMEOUT_MINUTES": "20",
	"SAVE_AT":               "75",
	"MIN_AT":                "90",
	"SKIP_DRAFTS":           "1",
	"MAX_AGE_DAYS":          "14",
	"WATERMARK":             "🤖 personal-review-bot",
	"UPDATE_CHECK":          "1",
	"UPDATE_REPO":           "badam1/personal-review-bot",
	"SHOW_LABELS":           "risk/,auto-risk/",
}

// Values are the settings as config.env would write them: what the Settings page shows and compares a form with.
func (c Config) Values() map[string]string {
	flag := func(b bool) string {
		if b {
			return "1"
		}
		return "0"
	}
	return map[string]string{
		"ORG":                   strings.Join(c.Orgs, ","),
		"POLL_SECONDS":          strconv.Itoa(int(c.PollInterval / time.Second)),
		"THRESHOLD":             strconv.Itoa(c.Threshold),
		"AUTO_POST":             strings.Join(c.AutoPost, ","),
		"APPROVE_TEXT":          c.ApproveText,
		"ADDR":                  c.WebAddr,
		"PARALLEL":              strconv.Itoa(c.MaxParallel),
		"AGENT_TIMEOUT_MINUTES": strconv.Itoa(int(c.AgentTimeout / time.Minute)),
		"SAVE_AT":               strconv.Itoa(c.SaveAt),
		"MIN_AT":                strconv.Itoa(c.MinAt),
		"SKIP_DRAFTS":           flag(c.SkipDrafts),
		"MAX_AGE_DAYS":          strconv.Itoa(int(c.MaxAge / (24 * time.Hour))),
		"WATERMARK":             c.Watermark,
		"UPDATE_CHECK":          flag(c.UpdateCheck),
		"UPDATE_REPO":           c.UpdateRepo,
		"SHOW_LABELS":           strings.Join(c.ShowLabels, ","),
	}
}

// FromEnv reports whether a setting is set in the environment, which wins over config.env and so over the page.
func FromEnv(name string) bool {
	_, ok := os.LookupEnv(envPrefix + name)
	return ok
}

// File is config.env's path.
func (c Config) File() string { return filepath.Join(c.ConfigDir, "config.env") }

var (
	onOff    = []string{"0", "1"}
	maxLabel = 50
)

// Check says what is wrong with a value for a setting, as the Settings page would save it (nil: nothing).
func Check(name, value string) error {
	if strings.ContainsAny(value, "\r\n") {
		return errors.New("must be on one line")
	}
	number := func(lo, hi int) error {
		n, err := strconv.Atoi(value)
		if err != nil || n < lo || n > hi {
			return fmt.Errorf("must be a whole number from %d to %d", lo, hi)
		}
		return nil
	}
	switch name {
	case "ORG":
		// ORG takes several orgs, so each name is checked rather than the whole value.
		for _, o := range SplitOrgs(value) {
			if !ValidOrg(o) {
				return fmt.Errorf("must be a GitHub organisation's name: letters, digits and dashes;"+
					" separate several with commas (%s is not one)", strconv.Quote(o))
			}
		}
	case "POLL_SECONDS":
		return number(30, 3600)
	case "THRESHOLD":
		return number(1, 100)
	case "AUTO_POST":
		for _, w := range list(value) {
			if !slices.Contains(severities, w) {
				return fmt.Errorf("%q is not a severity (low, medium or high)", w)
			}
		}
	case "APPROVE_TEXT":
		if len(value) > 200 {
			return errors.New("must be at most 200 characters")
		}
	case "WATERMARK":
		if strings.TrimSpace(value) == "" || len(value) > 80 || strings.ContainsAny(value, "<>") {
			return errors.New("must be 1 to 80 characters, without < or >")
		}
	case "ADDR":
		host, port, err := net.SplitHostPort(value)
		n, perr := strconv.Atoi(port)
		if err != nil || host != "127.0.0.1" || perr != nil || n < 1024 || n > 65535 {
			return errors.New("must be 127.0.0.1 and a port from 1024 to 65535, e.g. 127.0.0.1:12346")
		}
	case "PARALLEL":
		return number(1, 8)
	case "AGENT_TIMEOUT_MINUTES":
		return number(1, 120)
	case "SAVE_AT", "MIN_AT":
		return number(0, 100)
	case "MAX_AGE_DAYS":
		return number(0, 365)
	case "SKIP_DRAFTS", "UPDATE_CHECK":
		if !slices.Contains(onOff, value) {
			return errors.New("must be 1 or 0")
		}
	case "SHOW_LABELS":
		for _, p := range list(value) {
			if len(p) > maxLabel {
				return fmt.Errorf("%q is longer than %d characters", p, maxLabel)
			}
		}
	default:
		return fmt.Errorf("%s can't be changed here", name)
	}
	return nil
}

// Save changes config.env: each setting in set gets its value and each in unset goes back to its default. Every
// other line stays as it was, comments included, and a new setting goes at the end. A config.env that is a symlink
// stays one: the file it points to is changed (see atomicfile.Write).
func Save(dir string, set map[string]string, unset []string) error {
	for name, v := range set {
		if strings.ContainsAny(v, "\r\n") {
			return fmt.Errorf("%s: must be on one line", name)
		}
	}
	path := filepath.Join(dir, "config.env")
	old, err := os.ReadFile(path)
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}

	var lines []string
	written := map[string]bool{}
	text := strings.TrimSuffix(strings.ReplaceAll(string(old), "\r\n", "\n"), "\n")
	for _, line := range strings.Split(text, "\n") {
		if text == "" {
			break
		}
		lead, name, ok := settingLine(line)
		switch {
		case !ok:
			lines = append(lines, line)
		case slices.Contains(unset, name):
		case hasKey(set, name):
			if !written[name] { // a setting written twice: the last one counted, now the first one does
				lines = append(lines, lead+name+"="+quote(set[name]))
				written[name] = true
			}
		default:
			lines = append(lines, line)
		}
	}
	var added []string
	for name := range set {
		if !written[name] {
			added = append(added, name)
		}
	}
	slices.Sort(added)
	for _, name := range added {
		lines = append(lines, name+"="+quote(set[name]))
	}
	out := strings.Join(lines, "\n")
	if out != "" {
		out += "\n"
	}

	return atomicfile.Write(path, []byte(out), 0o600)
}

func hasKey(m map[string]string, k string) bool {
	_, ok := m[k]
	return ok
}

// settingLine splits a config.env line as readEnvFile reads it: what comes before the name ("export " and the
// PERSONAL_REVIEW_BOT_ prefix, kept as they were) and the name. ok is false for comments and other lines.
func settingLine(line string) (lead, name string, ok bool) {
	trimmed := strings.TrimSpace(line)
	key, _, found := strings.Cut(trimmed, "=")
	if !found || strings.HasPrefix(trimmed, "#") {
		return "", "", false
	}
	key = strings.TrimSpace(key)
	if rest, ok := strings.CutPrefix(key, "export "); ok {
		lead, key = "export ", strings.TrimSpace(rest)
	}
	if rest, ok := strings.CutPrefix(key, envPrefix); ok {
		lead, key = lead+envPrefix, rest
	}
	return lead, key, key != ""
}

// plain values are written as they are; any other is quoted, which readEnvFile undoes.
var plain = regexp.MustCompile(`^[A-Za-z0-9_.,/:@+-]+$`)

func quote(v string) string {
	if plain.MatchString(v) {
		return v
	}
	return `"` + v + `"`
}

// Live is the configuration a running daemon works with. The Settings page replaces it, and each poll, review and
// page reads the current one, so a saved setting applies from the next of them without a restart.
type Live struct{ p atomic.Pointer[Config] }

func NewLive(c Config) *Live {
	l := &Live{}
	l.p.Store(&c)
	return l
}

func (l *Live) Get() Config  { return *l.p.Load() }
func (l *Live) Set(c Config) { l.p.Store(&c) }
