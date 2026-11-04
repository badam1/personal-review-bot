package store

import (
	"path/filepath"
	"testing"
	"time"
)

// Superseding closes only the same PR's older reviews that still wait for you.
func TestSupersede(t *testing.T) {
	s, err := Open(filepath.Join(t.TempDir(), "reviews.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	add := func(repo string, number int, status string) int64 {
		id, err := s.Insert(&Review{Repo: repo, Number: number, Status: status})
		if err != nil {
			t.Fatal(err)
		}
		return id
	}
	old := add("o/r", 1, AwaitingYou)
	done := add("o/r", 1, Done)
	other := add("o/r", 2, AwaitingYou)
	keep := add("o/r", 1, AwaitingYou)
	later := add("o/r", 1, AwaitingYou) // newer than keep: not replaced by it

	if err := s.Supersede("o/r", 1, keep); err != nil {
		t.Fatal(err)
	}
	want := map[int64]string{old: Done, done: Done, other: AwaitingYou, keep: AwaitingYou, later: AwaitingYou}
	for id, status := range want {
		if r, _ := s.Get(id); r.Status != status {
			t.Errorf("review %d: status %s, want %s", id, r.Status, status)
		}
	}
	if r, _ := s.Get(old); r.Decision != "superseded by review 4" {
		t.Errorf("superseded decision = %q", r.Decision)
	}
}

// A re-review is told about the last review in which every reviewer ran, not a skipped, failed or partial one.
func TestPreviousComplete(t *testing.T) {
	s, err := Open(filepath.Join(t.TempDir(), "reviews.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	add := func(sha, status, tier, errText string) int64 {
		id, err := s.Insert(&Review{Repo: "o/r", Number: 1, HeadSHA: sha, Status: Running})
		if err != nil {
			t.Fatal(err)
		}
		_ = s.SetPlan(id, "", "", sha, tier, "", "", 0, 0, 0)
		if err := s.Finish(id, status, "", "", errText); err != nil {
			t.Fatal(err)
		}
		return id
	}
	if r, _ := s.PreviousComplete("o/r", 1, 1); r != nil {
		t.Fatal("no earlier reviews, want nil")
	}
	add("aaa", Done, "large", "")
	add("bbb", AwaitingYou, "large", "security: timed out") // partial
	add("ccc", Failed, "large", "fetch PR: 502")
	add("ddd", Ignored, "", "")
	now := add("eee", Running, "large", "")
	r, err := s.PreviousComplete("o/r", 1, now)
	if err != nil || r == nil || r.HeadSHA != "aaa" {
		t.Fatalf("got %+v, %v; want the review of aaa", r, err)
	}

	if err := s.AddRun(&Run{ReviewID: now, Agent: "verifier", Model: "opus", Findings: 2}); err != nil {
		t.Fatal(err)
	}
	if err := s.AddRun(&Run{ReviewID: now, Agent: "security", Model: "opus", Turns: 14, Denials: 1, Checked: "read x"}); err != nil {
		t.Fatal(err)
	}
	runs, err := s.Runs(now)
	if err != nil || len(runs) != 2 || runs[0].Agent != "security" || runs[0].Denials != 1 || runs[1].Agent != "verifier" {
		t.Fatalf("runs = %+v, %v; want security then verifier", runs, err)
	}
}

// The breakdown counts only real (not dry-run) findings scored since the scoring changed, by certainty and severity.
func TestDecisions(t *testing.T) {
	s, err := Open(filepath.Join(t.TempDir(), "reviews.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	review := func(dry bool) int64 {
		id, err := s.Insert(&Review{Repo: "o/r", Number: 1, Status: Done, DryRun: dry})
		if err != nil {
			t.Fatal(err)
		}
		return id
	}
	add := func(rid int64, conf int, sev, status string) {
		id, err := s.AddFinding(&Finding{ReviewID: rid, Agent: "logic", Path: "a", Line: 1, Severity: sev, Title: "t",
			Comment: "c", Status: Candidate})
		if err != nil {
			t.Fatal(err)
		}
		if err := s.UpdateFinding(&Finding{ID: id, Line: 1, Comment: "c", Confidence: conf, Status: status}); err != nil {
			t.Fatal(err)
		}
	}
	live, dry := review(false), review(true)
	add(live, 92, "high", AutoPosted)
	add(live, 85, "low", Discarded)
	add(live, 88, "low", Posted)
	add(live, 65, "medium", Posted)
	add(live, 30, "high", Posted)
	add(live, 70, "high", Rejected) // never decided by you: not counted
	add(dry, 95, "high", WouldPost) // dry run: not counted
	add(dry, 50, "high", Discarded)

	since, rows, err := s.Decisions(80)
	if err != nil {
		t.Fatal(err)
	}
	if since.IsZero() {
		t.Error("scoring_since should be set when the database is opened")
	}
	want := []DecisionRow{
		{"≥ 80% · high", 1, 0, 0}, {"≥ 80% · medium", 0, 0, 0}, {"≥ 80% · low", 0, 1, 1},
		{"60–79%", 0, 1, 0}, {"40–59%", 0, 0, 0}, {"< 40%", 0, 1, 0},
	}
	for i, w := range want {
		if rows[i] != w {
			t.Errorf("row %d = %+v, want %+v", i, rows[i], w)
		}
	}

	// Findings from reviews before the scoring changed are left out.
	if err := s.SetMeta("scoring_since", "2999-01-01T00:00:00Z"); err != nil {
		t.Fatal(err)
	}
	if _, rows, _ := s.Decisions(80); rows[0].BotPosted != 0 || rows[2].Posted != 0 {
		t.Errorf("findings scored before scoring_since were counted: %+v", rows)
	}
}

// Design concerns carry no certainty: they get their own row, outside the certainty ranges.
func TestDecisionsDesignRow(t *testing.T) {
	s, err := Open(filepath.Join(t.TempDir(), "reviews.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	rid, _ := s.Insert(&Review{Repo: "o/r", Number: 1, Status: Done})
	for _, status := range []string{Posted, Discarded, Discarded} {
		id, _ := s.AddFinding(&Finding{ReviewID: rid, Agent: "design", Severity: "medium", Title: "t", Comment: "c", Status: Pending})
		_ = s.UpdateFinding(&Finding{ID: id, Comment: "c", Status: status})
	}
	_, rows, err := s.Decisions(80)
	if err != nil {
		t.Fatal(err)
	}
	last := rows[len(rows)-1]
	if last.Label != "design concerns" || last.Posted != 1 || last.Discarded != 2 {
		t.Errorf("design row = %+v", last)
	}
	if rows[5].Posted+rows[5].Discarded != 0 {
		t.Errorf("design concerns leaked into the < 40%% range: %+v", rows[5])
	}
}

// Rows from when the design reviewer was called "approach" are renamed when the database opens.
func TestOpenRenamesApproach(t *testing.T) {
	path := filepath.Join(t.TempDir(), "reviews.db")
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	rid, _ := s.Insert(&Review{Repo: "o/r", Number: 1, Status: Done})
	fid, _ := s.AddFinding(&Finding{ReviewID: rid, Agent: "approach", Severity: "low", Title: "t", Comment: "c", Status: Pending})
	_ = s.UpdateFinding(&Finding{ID: fid, Comment: "c", Verdict: "approach", Status: Posted})
	_ = s.AddRun(&Run{ReviewID: rid, Agent: "approach", Model: "opus"})
	s.Close()

	s, err = Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	fs, _ := s.Findings(rid)
	runs, _ := s.Runs(rid)
	if len(fs) != 1 || fs[0].Agent != "design" || fs[0].Verdict != "design" || len(runs) != 1 || runs[0].Agent != "design" {
		t.Errorf("not renamed: findings %+v, runs %+v", fs, runs)
	}
}

// Failures lists a PR only while its latest review is a recent failure, and never an ignored PR or a dry run.
func TestFailures(t *testing.T) {
	s, err := Open(filepath.Join(t.TempDir(), "reviews.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	add := func(number int, status string, dry bool) int64 {
		id, err := s.Insert(&Review{Repo: "o/r", Number: number, Status: Running, DryRun: dry})
		if err != nil {
			t.Fatal(err)
		}
		if err := s.Finish(id, status, "", "", "fetch PR: 502"); err != nil {
			t.Fatal(err)
		}
		return id
	}
	failed := add(1, Failed, false)
	add(2, Failed, false)
	add(2, Done, false) // a later review of #2 went through
	add(3, Failed, false)
	_ = s.IgnorePR("o/r", 3)
	add(4, Failed, true)
	old := add(5, Failed, false)
	if _, err := s.db.Exec(`UPDATE reviews SET created_at = ? WHERE id = ?`,
		time.Now().Add(-10*24*time.Hour).UTC().Format(time.RFC3339), old); err != nil {
		t.Fatal(err)
	}

	got, err := s.Failures(time.Now().Add(-7 * 24 * time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].ID != failed {
		t.Errorf("got %+v, want only review %d (o/r#1)", got, failed)
	}
}
