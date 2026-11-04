package workspace

import "testing"

// Clones follow gh's git_protocol; anything but "ssh" means HTTPS, gh's default.
func TestCloneURL(t *testing.T) {
	cases := map[string]string{
		"ssh":   "git@github.com:acme/api.git",
		"https": "https://github.com/acme/api.git",
		"":      "https://github.com/acme/api.git",
	}
	for protocol, want := range cases {
		if got := cloneURL(protocol, "acme/api"); got != want {
			t.Errorf("cloneURL(%q) = %q, want %q", protocol, got, want)
		}
	}
}
