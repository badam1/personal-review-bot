package store

import (
	"database/sql"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// Stats count reviews that ran, leave dry runs out, and give each reviewer its findings' fate and cost.
func TestStats(t *testing.T) {
	s, err := Open(filepath.Join(t.TempDir(), "reviews.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	review := func(repo string, dry bool, cost float64, decision string) int64 {
		id, _ := s.Insert(&Review{Repo: repo, Number: 1, Status: Running, DryRun: dry})
		_ = s.SetPlan(id, "t", "a", "sha", "medium", "sonnet → opus", "", 200, 0, 1)
		_ = s.AddCost(id, cost)
		_ = s.Finish(id, Done, decision, "", "")
		return id
	}
	a := review("o/a", false, 2, "you approved")
	review("o/a", false, 1, "posted 1")
	review("o/b", true, 9, "dry run: you approved")
	_ = s.AddRun(&Run{ReviewID: a, Agent: "logic", Model: "sonnet", CostUSD: 1.2})
	_ = s.AddRun(&Run{ReviewID: a, Agent: "verifier", Model: "opus", CostUSD: 0.8, Error: "timed out"})
	for _, status := range []string{AutoPosted, Posted, Discarded, Rejected} {
		id, _ := s.AddFinding(&Finding{ReviewID: a, Agent: "logic", Severity: "high", Title: "t", Comment: "c", Status: Candidate})
		_ = s.UpdateFinding(&Finding{ID: id, Comment: "c", Status: status})
	}

	tot, err := s.Totals(time.Now().Add(-time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if tot.Reviews != 2 || tot.PRs != 1 || tot.CostUSD != 3 || tot.Approvals != 1 || tot.Posted() != 2 ||
		tot.Discarded != 1 || tot.Rejected != 1 || tot.PerReview() != 1.5 {
		t.Errorf("totals = %+v", tot)
	}
	rows, err := s.Reviewers()
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 2 || rows[0].Agent != "logic" || rows[1].Agent != "verifier" {
		t.Fatalf("reviewers = %+v", rows)
	}
	if l := rows[0]; l.Runs != 1 || l.Found != 4 || l.Posted() != 2 || l.CostPerPosted() != 0.6 || rows[1].Failed != 1 {
		t.Errorf("logic = %+v, verifier = %+v", l, rows[1])
	}
	days, err := s.DailySpend(14, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if len(days) != 14 || days[13].CostUSD != 3 || days[13].Reviews != 2 || days[0].Reviews != 0 {
		t.Errorf("today = %+v, first = %+v", days[13], days[0])
	}
	repos, err := s.Repos(10)
	if err != nil {
		t.Fatal(err)
	}
	if len(repos) != 1 || repos[0].Name != "o/a" || repos[0].Count != 2 || repos[0].Posted != 2 {
		t.Errorf("repos = %+v", repos)
	}
}

// A database from before reasons, code context and "marked wrong" existed gets the columns when it opens.
func TestOpenAddsColumns(t *testing.T) {
	path := filepath.Join(t.TempDir(), "reviews.db")
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	old := strings.NewReplacer(", reasons TEXT NOT NULL DEFAULT ''", "",
		",\n  context TEXT NOT NULL DEFAULT '', wrong INTEGER NOT NULL DEFAULT 0", "").Replace(schema)
	if old == schema {
		t.Fatal("the test no longer strips the new columns from the schema")
	}
	if _, err := db.Exec(old); err != nil {
		t.Fatal(err)
	}
	db.Close()

	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	id, _ := s.Insert(&Review{Repo: "o/r", Number: 1, Status: Running})
	if err := s.SetPlan(id, "t", "a", "sha", "small", "haiku → sonnet", "docs only", 1, 0, 1); err != nil {
		t.Fatal(err)
	}
	fid, _ := s.AddFinding(&Finding{ReviewID: id, Agent: "logic", Severity: "low", Title: "t", Comment: "c", Status: AutoPosted})
	if err := s.SetContext(fid, "1\t+\tx"); err != nil {
		t.Fatal(err)
	}
	if err := s.SetWrong(fid, true); err != nil {
		t.Fatal(err)
	}
	if err := s.SetAbout(id, "Moves the retry into the client."); err != nil {
		t.Fatal(err)
	}
	if err := s.SetApproval(id, 88, "Every changed file was read."); err != nil {
		t.Fatal(err)
	}
	if err := s.SetLabels(id, []string{"auto-risk/high", "risk/medium"}); err != nil {
		t.Fatal(err)
	}
	r, _ := s.Get(id)
	f, _ := s.Finding(fid)
	if r.Reasons != "docs only" || f.Context != "1\t+\tx" || !f.Wrong || r.About != "Moves the retry into the client." ||
		r.Approval != 88 || r.ApprovalReason != "Every changed file was read." || strings.Join(r.Labels, ",") != "auto-risk/high,risk/medium" {
		t.Errorf("review %+v, finding %+v", r, f)
	}
}

// History filters by repo, author and status and pages newest first; ignored PRs can be listed and let go.
func TestHistoryAndIgnored(t *testing.T) {
	s, err := Open(filepath.Join(t.TempDir(), "reviews.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	add := func(repo string, n int, author, status string) int64 {
		id, _ := s.Insert(&Review{Repo: repo, Number: n, Author: author, Title: fmt.Sprint(n), Status: Running})
		_ = s.Finish(id, status, "", "", "")
		return id
	}
	add("o/a", 1, "ann", Done)
	add("o/a", 2, "bob", Failed)
	add("o/b", 3, "ann", Done)
	last := add("o/b", 3, "ann", AwaitingYou)

	rs, total, err := s.History(HistoryFilter{Author: "ann"}, 2, 0)
	if err != nil || total != 3 || len(rs) != 2 || rs[0].ID != last {
		t.Errorf("ann: %d of %d, first %d, err %v", len(rs), total, rs[0].ID, err)
	}
	if rs, total, _ := s.History(HistoryFilter{Repo: "o/a", Status: Failed}, 50, 0); total != 1 || rs[0].Number != 2 {
		t.Errorf("o/a failed: %+v", rs)
	}
	if _, total, _ := s.History(HistoryFilter{Status: "active"}, 50, 0); total != 0 {
		t.Errorf("active = %d", total)
	}
	if repos, _ := s.Distinct("repo"); strings.Join(repos, ",") != "o/a,o/b" {
		t.Errorf("repos = %v", repos)
	}
	if _, err := s.Distinct("title; DROP TABLE reviews"); err == nil {
		t.Error("Distinct must refuse other columns")
	}
	if prs, _ := s.PRReviews("o/b", 3); len(prs) != 2 || prs[0].ID != last {
		t.Errorf("PR reviews = %+v", prs)
	}

	_ = s.IgnorePR("o/b", 3)
	ignored, err := s.IgnoredPRs()
	if err != nil || len(ignored) != 1 || ignored[0].ID != last {
		t.Fatalf("ignored = %+v, %v", ignored, err)
	}
	_ = s.UnignorePR("o/b", 3)
	if s.IsIgnored("o/b", 3) {
		t.Error("still ignored")
	}
}

// A review's iteration counts the earlier reviews of the same PR that ran; idle skips, dry runs and other PRs don't count.
func TestIteration(t *testing.T) {
	s, err := Open(filepath.Join(t.TempDir(), "reviews.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	add := func(n int, tier string, dry bool, status string) int64 {
		id, _ := s.Insert(&Review{Repo: "o/r", Number: n, Status: Running, DryRun: dry})
		_ = s.SetPlan(id, "t", "a", "sha", tier, "", "", 1, 0, 1)
		_ = s.Finish(id, status, "", "", "")
		return id
	}
	first := add(1, "medium", false, Done)
	add(1, "", false, Ignored)   // idle skip: never ran
	add(1, "medium", true, Done) // dry run
	add(2, "small", false, Done) // another PR
	second := add(1, "medium", false, Failed)
	third := add(1, "large", false, AwaitingYou)
	queued, _ := s.Insert(&Review{Repo: "o/r", Number: 1, Status: Queued})

	for id, want := range map[int64]int{first: 1, second: 2, third: 3, queued: 4} {
		r, _ := s.Get(id)
		if r.Iteration != want {
			t.Errorf("review %d: iteration %d, want %d", id, r.Iteration, want)
		}
		if r.Reran() != (want > 1) {
			t.Errorf("review %d: reran = %v", id, r.Reran())
		}
	}
	if rs, _ := s.PRReviews("o/r", 1); rs[0].ID != queued || rs[0].Iteration != 4 {
		t.Errorf("listed reviews carry the iteration too: %+v", rs[0])
	}
}

// Posted findings of earlier reviews come back with their follow-up; the stats count what was checked and fixed.
func TestFollowups(t *testing.T) {
	s, err := Open(filepath.Join(t.TempDir(), "reviews.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	first, _ := s.Insert(&Review{Repo: "o/r", Number: 1, Status: Running})
	add := func(review int64, status string) int64 {
		id, _ := s.AddFinding(&Finding{ReviewID: review, Agent: "logic", Severity: "high", Title: "t", Comment: "c", Status: Candidate})
		_ = s.UpdateFinding(&Finding{ID: id, Comment: "c", Status: status})
		return id
	}
	fixed, open, dropped := add(first, AutoPosted), add(first, Posted), add(first, Discarded)
	second, _ := s.Insert(&Review{Repo: "o/r", Number: 1, Status: Running})
	add(second, Posted) // not earlier than the second review itself

	_ = s.SetFollowup(fixed, FollowFixed, "fixed in abc123", second)
	_ = s.SetFollowup(open, FollowOpen, "still at a.go:3", second)
	fs, err := s.EarlierPosted("o/r", 1, second)
	if err != nil || len(fs) != 2 || fs[0].ID != fixed || fs[1].ID != open {
		t.Fatalf("earlier = %+v, %v", fs, err)
	}
	if !fs[0].Settled() || fs[1].Settled() || fs[0].FollowupNote != "fixed in abc123" || fs[0].FollowupReview != second {
		t.Errorf("follow-ups = %+v", fs)
	}
	for _, f := range fs {
		if f.ID == dropped {
			t.Error("a dropped finding was never posted, so there is nothing to follow up")
		}
	}
	rows, _ := s.Reviewers()
	if rows[0].Checked != 2 || rows[0].Fixed != 1 || rows[0].FixedShare() != 0.5 {
		t.Errorf("logic = %+v", rows[0])
	}
}
