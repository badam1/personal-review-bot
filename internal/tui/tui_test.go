package tui

import (
	"strings"
	"testing"

	"github.com/charmbracelet/lipgloss"

	"github.com/badam1/personal-review-bot/internal/config"
	"github.com/badam1/personal-review-bot/internal/store"
	"github.com/badam1/personal-review-bot/internal/update"
	"github.com/badam1/personal-review-bot/internal/version"
)

// The TUI opens links with the platform's own opener, and knows where it has none.
func TestOpener(t *testing.T) {
	cases := map[string]string{"darwin": "open", "linux": "xdg-open", "freebsd": "xdg-open", "windows": ""}
	for goos, want := range cases {
		got, ok := opener(goos)
		if got != want || ok != (want != "") {
			t.Errorf("opener(%q) = %q, %v; want %q", goos, got, ok, want)
		}
	}
}

// The TUI says when a newer release is out (u in the TUI, the CLI command in `status`), or what an update is doing.
func TestUpdateLine(t *testing.T) {
	newer := update.Release{Version: "v9.9.9"}
	cases := []struct {
		s           snapshot
		interactive bool
		want, not   string
	}{
		{snapshot{}, true, "", ""},
		{snapshot{update: newer}, true, "v9.9.9 is available (running ", ""},
		{snapshot{update: newer}, true, "u to update", "personal-review-bot update"},
		{snapshot{update: newer}, false, "personal-review-bot update", "u to update"},
		{snapshot{update: newer, updateState: "installing v9.9.9"}, true, "update: installing v9.9.9", "is available"},
		{snapshot{update: newer, updateState: "updating to v9.9.9 failed: no attestation"}, true, "failed: no attestation · v9.9.9 is available", ""},
	}
	for _, c := range cases {
		got := updateLine(c.s, c.interactive)
		if c.want == "" && got != "" || !strings.Contains(got, c.want) || c.not != "" && strings.Contains(got, c.not) {
			t.Errorf("updateLine(%+v, %v) = %q; want %q, not %q", c.s, c.interactive, got, c.want, c.not)
		}
	}
}

// The logo is the icon in half blocks: 16 columns by 8 rows, square in a terminal.
func TestLogo(t *testing.T) {
	lines := strings.Split(logo(), "\n")
	if len(lines) != 8 {
		t.Fatalf("%d rows, want 8", len(lines))
	}
	for i, l := range lines {
		if w := lipgloss.Width(l); w != 16 {
			t.Errorf("row %d is %d columns wide, want 16", i, w)
		}
	}
	for _, row := range strings.Fields(logoSprite) {
		if len(row) != 16 || strings.Trim(row, ".blmg") != "" {
			t.Errorf("sprite row %q: want 16 of . b l m g", row)
		}
	}
}

// The header shows the version, and the logo beside it only in the TUI on a terminal with room for it.
func TestHeader(t *testing.T) {
	cfg := config.Config{Threshold: 80, AutoSeverity: "high", WebAddr: "127.0.0.1:12346", UpdateRepo: "acme/prb"}
	roomy := header(cfg, snapshot{}, true, 100, 40)
	if !strings.Contains(roomy, "▀") || strings.Contains(roomy, "🤖") || !strings.Contains(roomy, "personal-review-bot "+version.String()) {
		t.Errorf("roomy terminal: want the logo, no emoji, and the version:\n%s", roomy)
	}
	for _, c := range []struct {
		interactive   bool
		width, height int
	}{{true, 50, 40}, {true, 100, 20}, {false, 160, 0}} {
		if h := header(cfg, snapshot{}, c.interactive, c.width, c.height); strings.Contains(h, "▀") || !strings.Contains(h, "🤖 personal-review-bot") {
			t.Errorf("%+v: want no logo and the emoji title:\n%s", c, h)
		}
	}
	if screen := render(cfg, snapshot{}, 0, "", true, 100, 40); !strings.Contains(screen, "bugs and ideas: github.com/acme/prb/issues") {
		t.Error("the TUI should say where to report bugs")
	}
}

// Each review shows what it cost, a running one what it has cost so far, and the header what was spent today and this
// week.
func TestCosts(t *testing.T) {
	for usd, want := range map[float64]string{0: "–", 0.004: "–", 0.02: "$0.02", 5.294: "$5.29", 113.386: "$113.39"} {
		if got := cost(usd); got != want {
			t.Errorf("cost(%v) = %q, want %q", usd, got, want)
		}
	}
	cfg := config.Config{Threshold: 80, AutoSeverity: "high", WebAddr: "127.0.0.1:12346", UpdateRepo: "acme/prb"}
	s := snapshot{spentToday: 12.5, spentWeek: 80.25,
		active: []store.Review{{Repo: "acme/api", Number: 3, Title: "Add retries", Status: store.Running, CostUSD: 0.38}},
		recent: []store.Review{{Repo: "acme/api", Number: 1, Title: "Fix login", Author: "dev", Status: store.Done, CostUSD: 1.4},
			{Repo: "acme/api", Number: 2, Title: "Old one", Author: "dev", Status: store.Ignored}}}
	screen := render(cfg, s, 0, "", false, 160, 0)
	for _, want := range []string{"spend: today $12.50 · 7 days $80.25", "$0.38 so far", "  $1.40 ", "      – "} {
		if !strings.Contains(screen, want) {
			t.Errorf("screen is missing %q:\n%s", want, screen)
		}
	}
}

// d in the TUI queues a deep review, and the list marks deep reviews.
func TestQueueDeep(t *testing.T) {
	st, err := store.Open(t.TempDir() + "/reviews.db")
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	r := store.Review{Repo: "acme/api", Number: 7, Title: "Fix login", Status: store.Done}
	if msg := queue(st, r, true); msg != "queued a deep review of acme/api#7" {
		t.Errorf("queue = %q", msg)
	}
	latest, _ := st.Latest("acme/api", 7)
	if !latest.Deep || latest.Source != "manual" {
		t.Errorf("queued %+v", latest)
	}
	screen := render(config.Config{}, snapshot{recent: []store.Review{*latest}}, -1, "", false, 160, 0)
	if !strings.Contains(screen, "🔬") {
		t.Errorf("deep reviews are marked:\n%s", screen)
	}
}
