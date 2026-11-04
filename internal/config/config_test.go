package config

import (
	"os"
	"path/filepath"
	"testing"
)

func TestAutoPosts(t *testing.T) {
	c := Config{Threshold: 80, AutoSeverity: "medium"}
	cases := []struct {
		verdict   string
		certainty int
		severity  string
		want      bool
	}{
		{"confirmed", 90, "high", true},
		{"confirmed", 80, "medium", true},
		{"confirmed", 95, "low", false},      // real but minor: you decide
		{"confirmed", 79, "high", false},     // not certain enough
		{"plausible", 95, "high", false},     // not verified
		{"confirmed", 95, "critical", false}, // unknown severity never posts
	}
	for _, tc := range cases {
		if got := c.AutoPosts(tc.verdict, tc.certainty, tc.severity); got != tc.want {
			t.Errorf("AutoPosts(%s, %d, %s) = %v, want %v", tc.verdict, tc.certainty, tc.severity, got, tc.want)
		}
	}
	if got := severity("hihg"); got != "high" {
		t.Errorf("a misspelt severity must tighten the rule, got %q", got)
	}
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
