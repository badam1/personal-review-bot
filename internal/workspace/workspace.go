// Package workspace checks out a PR's head commit into a throwaway git worktree, so the reviewers can read the
// surrounding code. Clones are cached (partial clones, blobs fetched on demand) under the cache dir. Git uses the
// protocol gh is set to (`gh config get git_protocol`) and, over HTTPS, gh's own login, so `gh auth login` is all
// it needs.
package workspace

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
)

type Workspace struct {
	Dir     string
	repoDir string
}

var (
	locksMu sync.Mutex
	locks   = map[string]*sync.Mutex{}
)

// repoLock serializes git operations on one cached clone (two reviews of the same repo can run at once).
func repoLock(repo string) *sync.Mutex {
	locksMu.Lock()
	defer locksMu.Unlock()
	if locks[repo] == nil {
		locks[repo] = &sync.Mutex{}
	}
	return locks[repo]
}

func Prepare(ctx context.Context, cacheDir, repo string, number int, sha string) (*Workspace, error) {
	repoDir := filepath.Join(cacheDir, "repos", repo)
	dir := filepath.Join(cacheDir, "work", fmt.Sprintf("%s-%d-%.12s", strings.ReplaceAll(repo, "/", "-"), number, sha))

	l := repoLock(repo)
	l.Lock()
	defer l.Unlock()

	// The clones hold private source, so the cache is the user's own to read. This runs before the clone check and
	// not inside it, because MkdirAll sets the mode only on folders it creates: a cache left at 0755 by an earlier
	// version has its repositories already, so the branch below is the one path that would never reach it.
	if err := os.MkdirAll(filepath.Dir(repoDir), 0o700); err != nil {
		return nil, err
	}
	// cacheDir is the one that matters — 0700 on it stops anyone else walking in, whatever the modes below it,
	// and "work" is made by git with the umask rather than by us — but each folder we own is tightened as well.
	for _, d := range []string{cacheDir, filepath.Join(cacheDir, "repos"), filepath.Dir(repoDir),
		filepath.Join(cacheDir, "work")} {
		if err := os.Chmod(d, 0o700); err != nil && !os.IsNotExist(err) {
			return nil, err
		}
	}

	if _, err := os.Stat(filepath.Join(repoDir, ".git")); err != nil {
		if err := git(ctx, "", "clone", "--quiet", "--filter=blob:none", "--no-checkout",
			cloneURL(ghProtocol(ctx), repo), repoDir); err != nil {
			return nil, err
		}
	}
	if err := git(ctx, repoDir, "fetch", "--quiet", "--filter=blob:none", "origin", fmt.Sprintf("pull/%d/head", number)); err != nil {
		return nil, err
	}
	_ = os.RemoveAll(dir)
	_ = git(ctx, repoDir, "worktree", "prune")
	if err := git(ctx, repoDir, "worktree", "add", "--quiet", "--detach", dir, sha); err != nil {
		return nil, err
	}
	return &Workspace{Dir: dir, repoDir: repoDir}, nil
}

func (w *Workspace) Cleanup() {
	_ = git(context.Background(), w.repoDir, "worktree", "remove", "--force", w.Dir)
	_ = os.RemoveAll(w.Dir)
}

// ghProtocol is the git protocol gh is set to use for github.com: "ssh" or "https" (its default).
func ghProtocol(ctx context.Context) string {
	out, err := exec.CommandContext(ctx, "gh", "config", "get", "git_protocol", "-h", "github.com").Output()
	if err != nil {
		return "https"
	}
	return strings.TrimSpace(string(out))
}

func cloneURL(protocol, repo string) string {
	if protocol == "ssh" {
		return "git@github.com:" + repo + ".git"
	}
	return "https://github.com/" + repo + ".git"
}

// credentials makes git ask gh for github.com HTTPS credentials (what `gh auth setup-git` would configure), without
// changing your git config. The empty helper first clears any other helper, such as a keychain with stale tokens.
var credentials = []string{"-c", "credential.https://github.com.helper=", "-c",
	"credential.https://github.com.helper=!gh auth git-credential"}

func git(ctx context.Context, dir string, args ...string) error {
	cmd := exec.CommandContext(ctx, "git", append(credentials[:len(credentials):len(credentials)], args...)...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "GIT_TERMINAL_PROMPT=0", "GIT_SSH_COMMAND=ssh -o BatchMode=yes")
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("git %s: %v: %s", args[0], err, strings.TrimSpace(stderr.String()))
	}
	return nil
}
