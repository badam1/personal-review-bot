package config

import (
	"maps"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The defaults table is what Load falls back to, so a fresh config's values are exactly the defaults.
func TestDefaultsAreLoadsDefaults(t *testing.T) {
	t.Setenv("PERSONAL_REVIEW_BOT_CONFIG_DIR", t.TempDir())
	if got := Load().Values(); !maps.Equal(got, Defaults) {
		t.Errorf("values of an empty config:\n%v\nwant the defaults:\n%v", got, Defaults)
	}
}

// Save changes only the lines it is asked to, keeps comments, the export and prefix forms and the file's mode, and
// what it writes reads back the same.
func TestSave(t *testing.T) {
	dir := t.TempDir()
	file := `# my settings
ORG=acme
export PERSONAL_REVIEW_BOT_THRESHOLD=90

# signed as
WATERMARK="🤖 acme-bot"
SAVE_AT=70
SAVE_AT=60
not a setting
`
	path := filepath.Join(dir, "config.env")
	if err := os.WriteFile(path, []byte(file), 0o640); err != nil {
		t.Fatal(err)
	}
	set := map[string]string{"THRESHOLD": "85", "SAVE_AT": "65", "APPROVE_TEXT": `say "hi" # now`, "PARALLEL": "3"}
	if err := Save(dir, set, []string{"WATERMARK"}); err != nil {
		t.Fatal(err)
	}
	got, _ := os.ReadFile(path)
	want := `# my settings
ORG=acme
export PERSONAL_REVIEW_BOT_THRESHOLD=85

# signed as
SAVE_AT=65
not a setting
APPROVE_TEXT="say "hi" # now"
PARALLEL=3
`
	if string(got) != want {
		t.Errorf("got:\n%s\nwant:\n%s", got, want)
	}
	if info, _ := os.Stat(path); info.Mode().Perm() != 0o640 {
		t.Errorf("mode %v, want the file's own 0640", info.Mode().Perm())
	}
	t.Setenv("PERSONAL_REVIEW_BOT_CONFIG_DIR", dir)
	c := Load()
	if c.ApproveText != `say "hi" # now` || c.Threshold != 85 || c.SaveAt != 65 || c.MaxParallel != 3 || c.Watermark != Defaults["WATERMARK"] {
		t.Errorf("read back: approve %q, threshold %d, save at %d, parallel %d, watermark %q", c.ApproveText, c.Threshold,
			c.SaveAt, c.MaxParallel, c.Watermark)
	}
}

// A config.env kept elsewhere and linked in stays a link: the file it points to is the one changed.
func TestSaveFollowsSymlink(t *testing.T) {
	dir, dotfiles := t.TempDir(), t.TempDir()
	real := filepath.Join(dotfiles, "config.env")
	if err := os.WriteFile(real, []byte("ORG=acme\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(real, filepath.Join(dir, "config.env")); err != nil {
		t.Fatal(err)
	}
	if err := Save(dir, map[string]string{"ORG": "umbrella"}, nil); err != nil {
		t.Fatal(err)
	}
	if info, err := os.Lstat(filepath.Join(dir, "config.env")); err != nil || info.Mode()&os.ModeSymlink == 0 {
		t.Fatalf("config.env is no longer a link: %v %v", info, err)
	}
	if got, _ := os.ReadFile(real); string(got) != "ORG=umbrella\n" {
		t.Errorf("the linked file has %q", got)
	}
}

// Without a config.env, Save creates it (and its directory), readable only by you.
func TestSaveCreates(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "personal-review-bot")
	if err := Save(dir, map[string]string{"WATERMARK": "🤖 bot", "ORG": "acme"}, []string{"MIN_AT"}); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "config.env")
	got, _ := os.ReadFile(path)
	if string(got) != "ORG=acme\nWATERMARK=\"🤖 bot\"\n" {
		t.Errorf("got %q", got)
	}
	if info, _ := os.Stat(path); info.Mode().Perm() != 0o600 {
		t.Errorf("mode %v, want 0600", info.Mode().Perm())
	}
	if err := Save(dir, map[string]string{"ORG": "a\nPARALLEL=8"}, nil); err == nil {
		t.Error("a value with a line break was saved")
	}
}

func TestCheck(t *testing.T) {
	ok := map[string]string{"ORG": "acme-corp", "POLL_SECONDS": "300", "THRESHOLD": "85", "AUTO_POST": "low,high",
		"APPROVE_TEXT": "", "WATERMARK": "🤖 bot", "ADDR": "127.0.0.1:8080", "PARALLEL": "4", "SAVE_AT": "0",
		"MIN_AT": "100", "MAX_AGE_DAYS": "0", "SKIP_DRAFTS": "0", "UPDATE_CHECK": "1", "SHOW_LABELS": "risk/, size/",
		"AGENT_TIMEOUT_MINUTES": "30"}
	for name, v := range ok {
		if err := Check(name, v); err != nil {
			t.Errorf("%s=%q: %v", name, v, err)
		}
	}
	bad := [][2]string{{"ORG", "acme/x"}, {"ORG", "-acme"}, {"POLL_SECONDS", "5"}, {"THRESHOLD", "101"},
		{"AUTO_POST", "low,urgent"}, {"WATERMARK", " "}, {"WATERMARK", "<b>bot</b>"}, {"ADDR", "0.0.0.0:12346"},
		{"ADDR", "127.0.0.1:80"}, {"ADDR", "localhost"}, {"PARALLEL", "0"}, {"SKIP_DRAFTS", "yes"},
		{"APPROVE_TEXT", "a\nb"}, {"APPROVE_TEXT", strings.Repeat("a", 201)}, {"UPDATE_REPO", "me/fork"},
		{"STATE_DIR", "/tmp"}}
	for _, b := range bad {
		if Check(b[0], b[1]) == nil {
			t.Errorf("%s=%q passed", b[0], b[1])
		}
	}
}

// The lists' column of labels is named after them unless LABELS_TITLE names it.
func TestLabelsColumn(t *testing.T) {
	for _, c := range []struct {
		labels []string
		title  string
		want   string
	}{
		{[]string{"risk/", "auto-risk/"}, "", "Risk"},
		{[]string{"size/"}, "", "Size"},
		{[]string{"team:"}, "", "Team"},
		{[]string{"risk/", "size/"}, "", "Labels"},
		{nil, "", "Labels"},
		{[]string{"ärea/"}, "", "Ärea"},
		{[]string{"risk/"}, "Danger", "Danger"},
	} {
		if got := (Config{ShowLabels: c.labels, LabelsTitle: c.title}).LabelsColumn(); got != c.want {
			t.Errorf("%q, title %q: %q, want %q", c.labels, c.title, got, c.want)
		}
	}
	if Check("LABELS_TITLE", strings.Repeat("x", 31)) == nil || Check("LABELS_TITLE", "") != nil {
		t.Error("LABELS_TITLE takes up to 30 characters, or none")
	}
}
