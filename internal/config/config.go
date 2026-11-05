// Package config holds personal-review-bot's settings. Each one is read from a PERSONAL_REVIEW_BOT_* environment
// variable, then from config.env in the config directory, then falls back to its default.
package config

import (
	"bufio"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"
)

type Config struct {
	Org          string        // only review PRs in this GitHub org ("" = anywhere)
	PollInterval time.Duration // how often to ask GitHub for new review requests
	Threshold    int           // verified certainty (0-100) at which a confirmed finding is posted without asking
	AutoPost     []string      // severities posted without asking, from low to high (empty = none)
	ApproveText  string        // said with your first approval of a PR ("" = approve without a comment)
	WebAddr      string        // confirmation page; 127.0.0.1 only
	MaxParallel  int           // reviews running at the same time
	AgentTimeout time.Duration // per Claude run
	SaveAt       int           // % of the Claude plan's 5-hour or weekly limit from which reviews use cheaper models
	MinAt        int           // % from which they use the cheapest, at low effort, without the design review
	SkipDrafts   bool
	MaxAge       time.Duration // don't review PRs automatically that have had no activity for this long (0 = no limit)
	Watermark    string        // footer on every comment the bot posts, followed by the finding's confidence
	UpdateCheck  bool          // look for new releases every hour, to offer them on the page and in the TUI
	UpdateRepo   string        // the GitHub repo releases come from
	ConfigDir    string        // config.env
	StateDir     string        // SQLite database
	CacheDir     string        // repo clones and per-review worktrees
}

// Load reads the settings. The environment wins over config.env, so a one-off run can override the file.
func Load() Config {
	home, _ := os.UserHomeDir()
	dir := os.Getenv("PERSONAL_REVIEW_BOT_CONFIG_DIR")
	if dir == "" {
		dir = filepath.Join(xdg("XDG_CONFIG_HOME", filepath.Join(home, ".config")), "personal-review-bot")
	}
	s := settings(readEnvFile(filepath.Join(dir, "config.env")))
	return Config{
		Org:          s.str("ORG", ""),
		PollInterval: time.Duration(s.int("POLL_SECONDS", 120)) * time.Second,
		Threshold:    s.int("THRESHOLD", 80),
		AutoPost:     autoPost(s),
		ApproveText:  s.str("APPROVE_TEXT", "LGTM :+1:"),
		WebAddr:      s.str("ADDR", "127.0.0.1:12346"),
		MaxParallel:  s.int("PARALLEL", 2),
		AgentTimeout: time.Duration(s.int("AGENT_TIMEOUT_MINUTES", 20)) * time.Minute,
		SaveAt:       s.int("SAVE_AT", 75),
		MinAt:        s.int("MIN_AT", 90),
		SkipDrafts:   s.str("SKIP_DRAFTS", "1") == "1",
		MaxAge:       time.Duration(s.int("MAX_AGE_DAYS", 14)) * 24 * time.Hour,
		Watermark:    s.str("WATERMARK", "🤖 personal-review-bot"),
		UpdateCheck:  s.str("UPDATE_CHECK", "1") == "1",
		UpdateRepo:   s.str("UPDATE_REPO", "badam1/personal-review-bot"),
		ConfigDir:    dir,
		StateDir:     s.str("STATE_DIR", filepath.Join(xdg("XDG_STATE_HOME", filepath.Join(home, ".local", "state")), "personal-review-bot")),
		CacheDir:     s.str("CACHE_DIR", filepath.Join(xdg("XDG_CACHE_HOME", filepath.Join(home, ".cache")), "personal-review-bot")),
	}
}

func (c Config) DBPath() string { return filepath.Join(c.StateDir, "reviews.db") }

var severities = []string{"low", "medium", "high"}

// autoPost reads AUTO_POST, the severities posted without asking (default low and medium: a high one is the costly
// kind to get wrong, so it waits for you). A misspelt word is left out, so it can only tighten the rule. Without
// AUTO_POST, the older AUTO_SEVERITY still means that severity or higher (anything unknown: high).
func autoPost(s settings) []string {
	list, ok := s.lookup("AUTO_POST")
	if !ok {
		old, ok := s.lookup("AUTO_SEVERITY")
		if !ok {
			return []string{"low", "medium"}
		}
		from := slices.Index(severities, strings.ToLower(strings.TrimSpace(old)))
		if from < 0 {
			from = len(severities) - 1
		}
		return slices.Clone(severities[from:])
	}
	words := strings.FieldsFunc(strings.ToLower(list), func(r rune) bool { return r == ',' || r == ' ' })
	var out []string
	for _, sev := range severities {
		if slices.Contains(words, sev) {
			out = append(out, sev)
		}
	}
	return out
}

// AutoPosts reports whether a finding is posted without asking: confirmed by the verifier, at or above the
// certainty threshold, and of a severity in AutoPost.
func (c Config) AutoPosts(verdict string, certainty int, severity string) bool {
	return verdict == "confirmed" && certainty >= c.Threshold && slices.Contains(c.AutoPost, severity)
}

// AutoPostSeverities names the severities posted without asking, e.g. "low or medium" ("" = none).
func (c Config) AutoPostSeverities() string { return orList(c.AutoPost) }

// HeldSeverities names the severities that always wait for you, e.g. "high" ("" = none).
func (c Config) HeldSeverities() string {
	var held []string
	for _, sev := range severities {
		if !slices.Contains(c.AutoPost, sev) {
			held = append(held, sev)
		}
	}
	return orList(held)
}

// orList joins words as a sentence does: "high", "low or medium", "low, medium or high".
func orList(words []string) string {
	if len(words) < 2 {
		return strings.Join(words, "")
	}
	return strings.Join(words[:len(words)-1], ", ") + " or " + words[len(words)-1]
}

// Footer is appended to every comment the bot posts, e.g. "🤖 personal-review-bot · 92%".
func (c Config) Footer(confidence int) string {
	return fmt.Sprintf("<sub>%s · %d%%</sub>", c.Watermark, confidence)
}

// DesignFooter ends a posted design concern, e.g. "🤖 personal-review-bot · design · 85%". The confidence is the design
// reviewer's own (no verifier checks a judgement); concerns from before it was rated (0) go without it.
func (c Config) DesignFooter(confidence int) string {
	if confidence <= 0 {
		return fmt.Sprintf("<sub>%s · design</sub>", c.Watermark)
	}
	return fmt.Sprintf("<sub>%s · design · %d%%</sub>", c.Watermark, confidence)
}

// ReminderFooter ends a reminder that an earlier finding is still open, e.g. "🤖 personal-review-bot · reminder".
func (c Config) ReminderFooter() string {
	return fmt.Sprintf("<sub>%s · reminder</sub>", c.Watermark)
}

// WebURL is the address to open in the browser (localhost rather than 127.0.0.1, same server).
func (c Config) WebURL() string {
	return "http://" + strings.Replace(c.WebAddr, "127.0.0.1", "localhost", 1)
}

// AllowedHosts are the Host headers the web server accepts (guards against DNS rebinding).
func (c Config) AllowedHosts() []string {
	_, port, _ := strings.Cut(c.WebAddr, ":")
	return []string{"127.0.0.1:" + port, "localhost:" + port}
}

// settings are the values read from config.env, by name without the PERSONAL_REVIEW_BOT_ prefix.
type settings map[string]string

const envPrefix = "PERSONAL_REVIEW_BOT_"

func (s settings) str(name, def string) string {
	if v, ok := s.lookup(name); ok {
		return v
	}
	return def
}

// lookup reads a setting and says whether it is set at all (an empty value is set).
func (s settings) lookup(name string) (string, bool) {
	if v, ok := os.LookupEnv(envPrefix + name); ok {
		return v, true
	}
	v, ok := s[name]
	return v, ok
}

func (s settings) int(name string, def int) int {
	if n, err := strconv.Atoi(s.str(name, "")); err == nil {
		return n
	}
	return def
}

// readEnvFile reads KEY=value lines (blank lines and # comments skipped, the PERSONAL_REVIEW_BOT_ prefix optional,
// surrounding quotes dropped). A missing file is no settings.
func readEnvFile(path string) settings {
	s := settings{}
	f, err := os.Open(path)
	if err != nil {
		return s
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		key, value, ok := strings.Cut(line, "=")
		if !ok || strings.HasPrefix(line, "#") {
			continue
		}
		key = strings.TrimPrefix(strings.TrimSpace(strings.TrimPrefix(key, "export ")), envPrefix)
		value = strings.TrimSpace(value)
		if len(value) >= 2 && (value[0] == '"' || value[0] == '\'') && value[len(value)-1] == value[0] {
			value = value[1 : len(value)-1]
		}
		s[key] = value
	}
	return s
}

// xdg returns an XDG base directory variable, or def when it is unset or not absolute (as the spec says to ignore).
func xdg(name, def string) string {
	if v := os.Getenv(name); filepath.IsAbs(v) {
		return v
	}
	return def
}
