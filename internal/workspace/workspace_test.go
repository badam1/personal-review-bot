package workspace

import (
	"context"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
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

// A git that stops answering fails the review rather than holding the repository's lock, and every later review of
// it, for good. The fake leaves a child behind holding stderr, as a killed git leaves its ssh.
func TestGitGivesUp(t *testing.T) {
	bin := t.TempDir()
	started := filepath.Join(bin, "started")
	fake := "#!/bin/sh\n/bin/sleep 30 &\necho $! > " + started + "\nexec /bin/sleep 30\n"
	if err := os.WriteFile(filepath.Join(bin, "git"), []byte(fake), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin)
	t.Cleanup(func() {
		if b, err := os.ReadFile(started); err == nil {
			if pid, err := strconv.Atoi(strings.TrimSpace(string(b))); err == nil {
				_ = syscall.Kill(pid, syscall.SIGKILL)
			}
		}
	})

	ctx, cancel := context.WithCancel(context.Background())
	go func() { // stopped once it hangs, with the child already holding stderr
		defer cancel()
		for end := time.Now().Add(20 * time.Second); time.Now().Before(end); time.Sleep(10 * time.Millisecond) {
			if _, err := os.Stat(started); err == nil {
				return
			}
		}
	}()
	start := time.Now()
	if err := git(ctx, "", "fetch"); err == nil {
		t.Fatal("a git that never answered succeeded")
	}
	if took := time.Since(start); took > 15*time.Second {
		t.Errorf("gave up after %v: waited on the child", took)
	}
}
