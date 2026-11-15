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

// PostedToCheck yields the posted inline findings whose downstream state is due, and is bounded: a finding drops out
// once its PR is merged or closed, once its review predates `since`, or when it was checked since `before`. Discarded,
// pending, non-inline and dry-run findings are never in it.
func TestPostedToCheck(t *testing.T) {
	s, err := Open(filepath.Join(t.TempDir(), "reviews.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	post := func(rev int64, status string, inline bool, line int) int64 {
		id, err := s.AddFinding(&Finding{ReviewID: rev, Agent: "logic", Path: "a.go", Line: line, Severity: "low",
			Title: "t", Comment: "c", Status: Candidate, Inline: inline})
		if err != nil {
			t.Fatal(err)
		}
		if err := s.UpdateFinding(&Finding{ID: id, Path: "a.go", Line: line, Comment: "c", Status: status,
			Inline: inline, PostedURL: "u"}); err != nil {
			t.Fatal(err)
		}
		return id
	}
	open, _ := s.Insert(&Review{Repo: "o/r", Number: 1, Status: Done})
	merged, _ := s.Insert(&Review{Repo: "o/r", Number: 2, Status: Done})
	dry, _ := s.Insert(&Review{Repo: "o/r", Number: 3, Status: Done, DryRun: true})

	wantA := post(open, Posted, true, 10)
	wantB := post(open, AutoPosted, true, 11)
	post(open, Posted, false, 0)    // non-inline (design): no thread to read
	post(open, Discarded, true, 12) // never posted
	post(open, Pending, true, 13)   // not posted yet
	onMerged := post(merged, Posted, true, 10)
	post(dry, Posted, true, 10) // dry run posts nothing
	if err := s.SetPRStatus("o/r", 2, PRStatus{State: PRMerged}); err != nil {
		t.Fatal(err)
	}

	const before, since = "3000-01-01T00:00:00Z", "2000-01-01T00:00:00Z"
	due, err := s.PostedToCheck(before, since, 100)
	if err != nil {
		t.Fatal(err)
	}
	got := map[int64]PostedFinding{}
	for _, pf := range due {
		got[pf.ID] = pf
	}
	if _, a := got[wantA]; !a {
		t.Errorf("open PR's posted finding %d is missing", wantA)
	}
	if _, b := got[wantB]; !b {
		t.Errorf("open PR's auto-posted finding %d is missing", wantB)
	}
	if _, m := got[onMerged]; m || len(got) != 2 {
		t.Fatalf("due = %v, want exactly %d and %d (the merged PR's %d must drop out)", keysOf(got), wantA, wantB, onMerged)
	}
	if pf := got[wantA]; pf.Repo != "o/r" || pf.Number != 1 || !pf.Inline || pf.Comment != "c" {
		t.Errorf("carried the wrong shape for MatchThread: %+v", pf)
	}

	// Age cap: a `since` in the future makes every review too old, so nothing is due.
	if none, _ := s.PostedToCheck(before, "3999-01-01T00:00:00Z", 100); len(none) != 0 {
		t.Errorf("age cap ignored: %d still due", len(none))
	}
	// Staleness: a finding checked after `before` waits; one never checked is still due.
	if err := s.SetDownstream(wantA, "endorsed", "2500-01-01T00:00:00Z"); err != nil {
		t.Fatal(err)
	}
	for _, pf := range mustCheck(t, s, "2400-01-01T00:00:00Z", since) {
		if pf.ID == wantA {
			t.Error("a finding checked more recently than `before` should wait its turn")
		}
	}
}

func mustCheck(t *testing.T, s *Store, before, since string) []PostedFinding {
	t.Helper()
	due, err := s.PostedToCheck(before, since, 100)
	if err != nil {
		t.Fatal(err)
	}
	return due
}

func keysOf(m map[int64]PostedFinding) []int64 {
	var ks []int64
	for k := range m {
		ks = append(ks, k)
	}
	return ks
}

// SetDownstream records a signal, and an unknown result never erases a signal already seen — it only moves the check
// time forward.
func TestSetDownstream(t *testing.T) {
	s, err := Open(filepath.Join(t.TempDir(), "reviews.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	rev, _ := s.Insert(&Review{Repo: "o/r", Number: 1, Status: Done})
	id, _ := s.AddFinding(&Finding{ReviewID: rev, Agent: "logic", Path: "a.go", Line: 1, Severity: "low", Title: "t",
		Comment: "c", Status: Candidate, Inline: true})
	_ = s.UpdateFinding(&Finding{ID: id, Path: "a.go", Line: 1, Comment: "c", Status: Posted, Inline: true, PostedURL: "u"})

	if err := s.SetDownstream(id, "endorsed", "2026-01-01T00:00:00Z"); err != nil {
		t.Fatal(err)
	}
	if f, _ := s.Finding(id); f.Downstream != "endorsed" {
		t.Fatalf("downstream = %q, want endorsed", f.Downstream)
	}
	// unknown must not clobber the endorsement, but should move the check time on.
	if err := s.SetDownstream(id, "unknown", "2026-02-02T00:00:00Z"); err != nil {
		t.Fatal(err)
	}
	if f, _ := s.Finding(id); f.Downstream != "endorsed" {
		t.Errorf("an unknown result erased the signal: now %q, want endorsed kept", f.Downstream)
	} else if !f.DownstreamAt.Equal(parseTime("2026-02-02T00:00:00Z")) {
		t.Errorf("check time not moved on: %v", f.DownstreamAt)
	}
	// a real signal does overwrite.
	if err := s.SetDownstream(id, "disputed", "2026-03-03T00:00:00Z"); err != nil {
		t.Fatal(err)
	}
	if f, _ := s.Finding(id); f.Downstream != "disputed" {
		t.Errorf("downstream = %q, want disputed", f.Downstream)
	}
}

// DownstreamMix counts posted findings by what became of them; dry-run and non-inline findings are left out.
func TestDownstreamMix(t *testing.T) {
	s, err := Open(filepath.Join(t.TempDir(), "reviews.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	rev, _ := s.Insert(&Review{Repo: "o/r", Number: 1, Status: Done})
	post := func(signal string) {
		id, _ := s.AddFinding(&Finding{ReviewID: rev, Agent: "logic", Path: "a.go", Line: 1, Severity: "low",
			Title: "t", Comment: "c", Status: Candidate, Inline: true})
		_ = s.UpdateFinding(&Finding{ID: id, Path: "a.go", Line: 1, Comment: "c", Status: Posted, Inline: true, PostedURL: "u"})
		if signal != "" {
			_ = s.SetDownstream(id, signal, "2026-01-01T00:00:00Z")
		}
	}
	post("endorsed")
	post("endorsed")
	post("disputed")
	post("quiet")
	post("") // unchecked

	m, err := s.DownstreamMix()
	if err != nil {
		t.Fatal(err)
	}
	if want := (DownstreamMix{Endorsed: 2, Quiet: 1, Disputed: 1, Unchecked: 1}); m != want {
		t.Errorf("DownstreamMix = %+v, want %+v", m, want)
	}
	if m.Known() != 4 {
		t.Errorf("Known = %d, want 4", m.Known())
	}
}
