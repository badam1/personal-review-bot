package tui

import (
	"strings"
	"testing"

	"github.com/badam1/personal-review-bot/internal/update"
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
