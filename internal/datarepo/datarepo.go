// Package datarepo keeps the bot's durable review outcomes in a git repository, one commit per review,
// Backlog.md-style: a record holds only labels and numbers — never finding text or code — so the repo is safe to share
// and shows the whole history. It is the durable, shareable half of the store (see docs/portable-channels-scope.md, D6);
// operational, in-flight state stays in SQLite.
//
// The bot only commits. Pushing is the data repo's own concern — a post-commit hook that rebases and pushes, exactly as
// Backlog.md does — so a stale local clone or a push race is handled where git already handles it, not reinvented here.
// Writes are serialized so two reviews finishing at once don't collide on the commit.
package datarepo

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Record is one review's durable outcome. It carries only labels and numbers, so nothing sensitive is committed; the
// raw finding text and code stay on GitHub (the posted comments).
type Record struct {
	Repo     string    `json:"repo"`   // owner/name
	Number   int       `json:"number"` // PR number
	SHA      string    `json:"sha"`    // the head commit this review covered
	At       time.Time `json:"at"`
	Decision string    `json:"decision"`
	CostUSD  float64   `json:"cost_usd"`
	Channel  string    `json:"channel,omitempty"`
	Findings []Finding `json:"findings,omitempty"`
}

// Finding is the per-finding outcome kept for analytics: labels only, never the text.
type Finding struct {
	Agent      string `json:"agent"`
	Severity   string `json:"severity"`
	Status     string `json:"status"`               // posted | auto_posted | discarded | ...
	Edited     string `json:"edited,omitempty"`     // verbatim | edited | unknown (once the capture lands)
	Downstream string `json:"downstream,omitempty"` // endorsed | quiet | disputed | ... (once it lands)
}

// Store appends records to a git data repo on disk.
type Store struct {
	dir string
	mu  sync.Mutex
}

// Open uses the git working tree at dir as the data repo. It errors when dir is not a git repository, so a
// misconfiguration fails fast rather than silently dropping records.
func Open(dir string) (*Store, error) {
	cmd := exec.Command("git", "-C", dir, "rev-parse", "--is-inside-work-tree")
	if out, err := cmd.CombinedOutput(); err != nil {
		return nil, fmt.Errorf("data repo %q is not a git working tree: %v: %s", dir, err, strings.TrimSpace(string(out)))
	}
	return &Store{dir: dir}, nil
}

// Reviewed reports whether a record already exists for this repo, PR and SHA — the idempotency check a one-shot run
// makes before reviewing, so a workflow re-run at the same commit does not review and post twice.
func (s *Store) Reviewed(repo string, number int, sha string) bool {
	_, err := os.Stat(filepath.Join(s.dir, recordPath(repo, number, sha)))
	return err == nil
}

// LastReviewed returns the head SHA of the most recent record for this PR, ignoring except (the head about to be
// reviewed), so a re-review on stateless compute can focus on what changed since — the "last reviewed commit" without
// any local state. ok is false when the PR has no earlier record. Records hold no finding text, so this only yields
// the commit, not the earlier findings.
func (s *Store) LastReviewed(repo string, number int, except string) (sha string, ok bool) {
	owner, name, _ := strings.Cut(repo, "/")
	dir := filepath.Join(s.dir, "records", safe(owner), safe(name), strconv.Itoa(number))
	entries, err := os.ReadDir(dir)
	if err != nil {
		return "", false
	}
	var latest time.Time
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		data, err := os.ReadFile(filepath.Join(dir, e.Name()))
		if err != nil {
			continue
		}
		var rec Record
		if json.Unmarshal(data, &rec) != nil || rec.SHA == "" || rec.SHA == except {
			continue
		}
		if rec.At.After(latest) {
			latest, sha, ok = rec.At, rec.SHA, true
		}
	}
	return sha, ok
}

// Record writes rec and commits it. It is serialized, and a record whose content is unchanged makes no commit, so
// re-recording the same outcome is a safe no-op.
func (s *Store) Record(rec Record) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	rel := recordPath(rec.Repo, rec.Number, rec.SHA)
	full := filepath.Join(s.dir, rel)
	if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
		return err
	}
	data, err := json.MarshalIndent(rec, "", "  ")
	if err != nil {
		return err
	}
	if err := os.WriteFile(full, append(data, '\n'), 0o644); err != nil {
		return err
	}
	if err := s.git("add", "--", rel); err != nil {
		return err
	}
	// Nothing staged means the record is unchanged — a no-op, not an error.
	if s.git("diff", "--cached", "--quiet", "--", rel) == nil {
		return nil
	}
	return s.git("-c", "user.name=personal-review-bot",
		"-c", "user.email=personal-review-bot@users.noreply.github.com",
		"commit", "-m", fmt.Sprintf("record: %s#%d @ %s", rec.Repo, rec.Number, shortSHA(rec.SHA)), "--", rel)
}

func (s *Store) git(args ...string) error {
	cmd := exec.Command("git", append([]string{"-C", s.dir}, args...)...)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("git %s: %v: %s", args[0], err, strings.TrimSpace(stderr.String()))
	}
	return nil
}

// recordPath is where a review's record lives: records/<owner>/<name>/<number>/<sha>.json. Its presence is the
// idempotency key. Each segment is sanitized so an odd repo or ref name can't escape the records tree.
func recordPath(repo string, number int, sha string) string {
	owner, name, _ := strings.Cut(repo, "/")
	return filepath.Join("records", safe(owner), safe(name), strconv.Itoa(number), safe(sha)+".json")
}

// safe reduces a path segment to a conservative charset, so nothing in it is a separator or traversal. A segment of
// only dots ("." or "..") is special to the filesystem — filepath.Join would clean "records/.." away and escape the
// tree — so those are prefixed, while a dot inside a name (foo.bar) is fine and kept.
func safe(s string) string {
	if s == "" {
		return "_"
	}
	s = strings.Map(func(r rune) rune {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '.', r == '_', r == '-':
			return r
		default:
			return '-'
		}
	}, s)
	if s == "." || s == ".." {
		return "_" + s
	}
	return s
}

func shortSHA(s string) string {
	if len(s) > 10 {
		return s[:10]
	}
	return s
}
