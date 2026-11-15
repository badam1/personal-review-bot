package datarepo

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

// initRepo makes a throwaway git work tree with an identity, so commits succeed without touching the user's git config.
func initRepo(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	for _, args := range [][]string{
		{"init", "-q"},
		{"config", "user.name", "test"},
		{"config", "user.email", "test@example.com"},
	} {
		cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v: %s", args, err, out)
		}
	}
	return dir
}

func commitCount(t *testing.T, dir string) int {
	t.Helper()
	out, err := exec.Command("git", "-C", dir, "rev-list", "--count", "--all").CombinedOutput()
	if err != nil {
		t.Fatalf("rev-list: %v: %s", err, out)
	}
	n, err := strconv.Atoi(strings.TrimSpace(string(out)))
	if err != nil {
		t.Fatalf("parse count %q: %v", out, err)
	}
	return n
}

// Open rejects a directory that isn't a git work tree, so a misconfigured data repo fails fast instead of silently
// dropping every record.
func TestOpenRejectsNonRepo(t *testing.T) {
	if _, err := Open(t.TempDir()); err == nil {
		t.Fatal("Open on a non-git dir: want error, got nil")
	}
	if _, err := Open(initRepo(t)); err != nil {
		t.Fatalf("Open on a git work tree: %v", err)
	}
}

// Record writes the JSON to records/<owner>/<name>/<number>/<sha>.json and commits it, carrying only labels and
// numbers — never finding text.
func TestRecordWritesAndCommits(t *testing.T) {
	dir := initRepo(t)
	s, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	rec := Record{
		Repo: "badam1/personal-review-bot", Number: 42, SHA: "abc1234def5678", At: time.Now(),
		Decision: "approve", CostUSD: 0.12, Channel: "company",
		Findings: []Finding{{Agent: "security", Severity: "high", Status: "auto_posted"}},
	}
	if err := s.Record(rec); err != nil {
		t.Fatal(err)
	}

	path := filepath.Join(dir, "records", "badam1", "personal-review-bot", "42", "abc1234def5678.json")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("record not at expected path: %v", err)
	}
	var got Record
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatalf("record is not valid JSON: %v", err)
	}
	if got.Decision != "approve" || got.CostUSD != 0.12 || len(got.Findings) != 1 || got.Findings[0].Agent != "security" {
		t.Errorf("round-trip mismatch: %+v", got)
	}
	if commitCount(t, dir) != 1 {
		t.Errorf("want 1 commit, got %d", commitCount(t, dir))
	}
}

// Reviewed is the idempotency check: false until a record exists, true after, so a workflow re-run at the same commit
// doesn't review and post twice.
func TestReviewedIdempotency(t *testing.T) {
	dir := initRepo(t)
	s, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	const repo, num, sha = "badam1/x", 7, "deadbeef"
	if s.Reviewed(repo, num, sha) {
		t.Fatal("Reviewed before any record: want false")
	}
	if err := s.Record(Record{Repo: repo, Number: num, SHA: sha, Decision: "changes"}); err != nil {
		t.Fatal(err)
	}
	if !s.Reviewed(repo, num, sha) {
		t.Error("Reviewed after recording: want true")
	}
	// A different SHA on the same PR is a new review, not a repeat.
	if s.Reviewed(repo, num, "99998888") {
		t.Error("Reviewed for a different SHA: want false")
	}
}

// Re-recording identical content makes no new commit — re-running a completed review is a safe no-op.
func TestRecordUnchangedIsNoop(t *testing.T) {
	dir := initRepo(t)
	s, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	rec := Record{Repo: "o/r", Number: 1, SHA: "aaaa1111", At: time.Unix(0, 0).UTC(), Decision: "approve"}
	if err := s.Record(rec); err != nil {
		t.Fatal(err)
	}
	if err := s.Record(rec); err != nil {
		t.Fatal(err)
	}
	if n := commitCount(t, dir); n != 1 {
		t.Errorf("identical re-record made %d commits, want 1", n)
	}
	// A changed outcome at the same SHA (e.g. a re-review that now approves) does commit again.
	rec.Decision = "changes"
	if err := s.Record(rec); err != nil {
		t.Fatal(err)
	}
	if n := commitCount(t, dir); n != 2 {
		t.Errorf("changed re-record made %d commits total, want 2", n)
	}
}

// LastReviewed returns the most recent recorded head for a PR, ignoring the one about to be reviewed — the "last
// reviewed commit" that drives an incremental re-review with no local state.
func TestLastReviewed(t *testing.T) {
	dir := initRepo(t)
	s, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	const repo, num = "o/r", 3
	if _, ok := s.LastReviewed(repo, num, ""); ok {
		t.Error("a PR with no records has no last reviewed commit")
	}
	base := time.Unix(1_700_000_000, 0).UTC()
	if err := s.Record(Record{Repo: repo, Number: num, SHA: "sha-a", At: base, Decision: "approve"}); err != nil {
		t.Fatal(err)
	}
	if err := s.Record(Record{Repo: repo, Number: num, SHA: "sha-b", At: base.Add(time.Hour), Decision: "changes"}); err != nil {
		t.Fatal(err)
	}
	if sha, ok := s.LastReviewed(repo, num, ""); !ok || sha != "sha-b" {
		t.Errorf("LastReviewed = %q,%v, want sha-b,true (newest At)", sha, ok)
	}
	// Reviewing again at sha-b: its own record is ignored, so the previous distinct commit comes back.
	if sha, ok := s.LastReviewed(repo, num, "sha-b"); !ok || sha != "sha-a" {
		t.Errorf("LastReviewed(except sha-b) = %q,%v, want sha-a,true", sha, ok)
	}
	// A different PR shares nothing.
	if _, ok := s.LastReviewed(repo, 999, ""); ok {
		t.Error("an unrelated PR has no last reviewed commit")
	}
}

// A repo or ref with path-ish characters can't escape the records tree.
func TestRecordPathStaysInTree(t *testing.T) {
	got := recordPath("../../etc/pass", 1, "../../../x")
	if filepath.IsAbs(got) {
		t.Fatalf("recordPath produced an absolute path: %q", got)
	}
	clean := filepath.Clean(got)
	if clean != got || len(clean) < len("records/") || clean[:len("records")] != "records" {
		t.Errorf("recordPath escaped the records tree: %q", got)
	}
}
