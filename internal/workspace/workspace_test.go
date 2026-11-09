package workspace

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

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

// The cache holds private clones. A cache made by an earlier version is 0755, and MkdirAll won't change a folder
// that already exists, so Prepare tightens it on the way past — including when the clone it wants is already there
// and the first-clone branch never runs.
func TestPrepareTightensAnExistingCache(t *testing.T) {
	cache := t.TempDir()
	repos := filepath.Join(cache, "repos")
	if err := os.MkdirAll(filepath.Join(repos, "acme", "api"), 0o755); err != nil {
		t.Fatal(err)
	}
	for _, d := range []string{cache, repos} {
		if err := os.Chmod(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}

	// The clone fails (no such repository), which is fine: the modes are set before that is attempted.
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, _ = Prepare(ctx, cache, "acme/api", 1, "deadbeef")

	for _, d := range []string{cache, repos} {
		fi, err := os.Stat(d)
		if err != nil {
			t.Fatal(err)
		}
		if got := fi.Mode().Perm(); got != 0o700 {
			t.Errorf("%s is %o, want 700: a cache of private clones others can read", d, got)
		}
	}
}
