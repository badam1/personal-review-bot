package tui

import "testing"

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
