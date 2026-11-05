package config

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

func TestAutoPosts(t *testing.T) {
	c := Config{Threshold: 80, AutoPost: []string{"low", "medium"}}
	cases := []struct {
		verdict   string
		certainty int
		severity  string
		want      bool
	}{
		{"confirmed", 90, "low", true},
		{"confirmed", 80, "medium", true},
		{"confirmed", 95, "high", false},     // serious: you look first
		{"confirmed", 79, "medium", false},   // not certain enough
		{"plausible", 95, "low", false},      // not verified
		{"confirmed", 95, "critical", false}, // unknown severity never posts
	}
	for _, tc := range cases {
		if got := c.AutoPosts(tc.verdict, tc.certainty, tc.severity); got != tc.want {
			t.Errorf("AutoPosts(%s, %d, %s) = %v, want %v", tc.verdict, tc.certainty, tc.severity, got, tc.want)
		}
	}
	if got, held := c.AutoPostSeverities(), c.HeldSeverities(); got != "low or medium" || held != "high" {
		t.Errorf("posted %q, held %q", got, held)
	}
	if got := (Config{AutoPost: severities}).AutoPostSeverities(); got != "low, medium or high" {
		t.Errorf("all = %q", got)
	}
}

// AUTO_POST lists the severities posted without asking; misspelt words drop out. Without it, the older AUTO_SEVERITY
// still means that severity or higher, and with neither, low and medium post.
func TestAutoPostSetting(t *testing.T) {
	t.Setenv("PERSONAL_REVIEW_BOT_CONFIG_DIR", t.TempDir())
	for _, tc := range []struct {
		autoPost, autoSeverity *string
		want                   string
	}{
		{nil, nil, "low medium"},
		{ptr("high"), nil, "high"},
		{ptr("Medium, low"), nil, "low medium"},
		{ptr("low,hihg"), nil, "low"},
		{ptr(""), nil, ""},
		{ptr("none"), nil, ""},
		{nil, ptr("medium"), "medium high"},
		{nil, ptr("hihg"), "high"},
		{ptr("low"), ptr("high"), "low"},
	} {
		set := func(name string, v *string) {
			if v == nil {
				os.Unsetenv("PERSONAL_REVIEW_BOT_" + name)
				return
			}
			t.Setenv("PERSONAL_REVIEW_BOT_"+name, *v)
		}
		t.Setenv("PERSONAL_REVIEW_BOT_AUTO_POST", "") // registers both for cleanup before unsetting
		t.Setenv("PERSONAL_REVIEW_BOT_AUTO_SEVERITY", "")
		set("AUTO_POST", tc.autoPost)
		set("AUTO_SEVERITY", tc.autoSeverity)
		if got := strings.Join(Load().AutoPost, " "); got != tc.want {
			t.Errorf("AUTO_POST %v, AUTO_SEVERITY %v: got %q, want %q", deref(tc.autoPost), deref(tc.autoSeverity), got, tc.want)
		}
	}
}

func ptr(s string) *string { return &s }

func deref(s *string) any {
	if s == nil {
		return "unset"
	}
	return *s
}

// config.env fills in what the environment doesn't set; the environment wins, so a one-off run can override it.
func TestLoadConfigEnv(t *testing.T) {
	dir := t.TempDir()
	file := `# my settings
ORG=acme
export PERSONAL_REVIEW_BOT_THRESHOLD=90
WATERMARK="🤖 acme-bot"
APPROVE_TEXT=''
not a setting
`
	if err := os.WriteFile(filepath.Join(dir, "config.env"), []byte(file), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PERSONAL_REVIEW_BOT_CONFIG_DIR", dir)
	t.Setenv("PERSONAL_REVIEW_BOT_THRESHOLD", "85")
	c := Load()
	if c.Org != "acme" || c.Threshold != 85 || c.Watermark != "🤖 acme-bot" || c.ApproveText != "" || c.ConfigDir != dir {
		t.Errorf("got org %q, threshold %d, watermark %q, approve text %q, config dir %q", c.Org, c.Threshold, c.Watermark,
			c.ApproveText, c.ConfigDir)
	}
	if c.PollInterval.Seconds() != 120 {
		t.Errorf("unset values keep their defaults, poll interval is %s", c.PollInterval)
	}
}

// The page shows the labels that start with SHOW_LABELS's prefixes, sorted; by default the risk labels.
func TestShownLabels(t *testing.T) {
	t.Setenv("PERSONAL_REVIEW_BOT_CONFIG_DIR", t.TempDir())
	labels := []string{"size/m", "risk/medium", "bug", "auto-risk/high"}
	if got := Load().Shown(labels); !slices.Equal(got, []string{"auto-risk/high", "risk/medium"}) {
		t.Errorf("default: %q", got)
	}
	t.Setenv("PERSONAL_REVIEW_BOT_SHOW_LABELS", " size/ , bug")
	if got := Load().Shown(labels); !slices.Equal(got, []string{"bug", "size/m"}) {
		t.Errorf("size/ and bug: %q", got)
	}
	t.Setenv("PERSONAL_REVIEW_BOT_SHOW_LABELS", "")
	if got := Load().Shown(labels); got != nil {
		t.Errorf("empty shows none: %q", got)
	}
}
