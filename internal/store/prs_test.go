package store

import (
	"path/filepath"
	"slices"
	"testing"
	"time"
)

func openTest(t *testing.T) *Store {
	t.Helper()
	s, err := Open(filepath.Join(t.TempDir(), "reviews.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

// A review carries its PR's state, which is kept once per PR.
func TestPRStatus(t *testing.T) {
	s := openTest(t)
	first, _ := s.Insert(&Review{Repo: "o/r", Number: 1, Status: Done})
	second, _ := s.Insert(&Review{Repo: "o/r", Number: 1, Status: AwaitingYou})
	if r, _ := s.Get(first); r.PR.State != "" {
		t.Errorf("an unchecked PR has state %q", r.PR.State)
	}
	merged := time.Date(2026, 11, 9, 10, 0, 0, 0, time.UTC)
	if err := s.SetPRStatus("o/r", 1, PRStatus{State: PRMerged, By: "lena", ChangedAt: merged}); err != nil {
		t.Fatal(err)
	}
	for _, id := range []int64{first, second} {
		r, _ := s.Get(id)
		if r.PR.State != PRMerged || r.PR.By != "lena" || !r.PR.ChangedAt.Equal(merged) || r.PR.CheckedAt.IsZero() || !r.PR.Over() {
			t.Errorf("review %d: %+v", id, r.PR)
		}
	}
}

// What is due a check: waiting PRs every few minutes, the month's others hourly, never a merged or closed one.
func TestPRsToCheck(t *testing.T) {
	s := openTest(t)
	now := time.Now()
	add := func(number int, status string, created time.Duration) {
		id, _ := s.Insert(&Review{Repo: "o/r", Number: number, Status: status})
		if _, err := s.db.Exec(`UPDATE reviews SET created_at = ? WHERE id = ?`, now.Add(-created).UTC().Format(time.RFC3339), id); err != nil {
			t.Fatal(err)
		}
	}
	checked := func(number int, state string, ago time.Duration) {
		if err := s.SetPRStatus("o/r", number, PRStatus{State: state}); err != nil {
			t.Fatal(err)
		}
		if _, err := s.db.Exec(`UPDATE prs SET checked_at = ? WHERE number = ?`, now.Add(-ago).UTC().Format(time.RFC3339), number); err != nil {
			t.Fatal(err)
		}
	}
	add(1, AwaitingYou, time.Hour) // waiting, never checked: due
	add(2, AwaitingYou, time.Hour) // waiting, checked a minute ago: not yet
	checked(2, PROpen, time.Minute)
	add(3, Done, 2*time.Hour) // done this month, checked two hours ago: due
	checked(3, PROpen, 2*time.Hour)
	add(4, Done, 2*time.Hour) // done this month, checked ten minutes ago: not yet
	checked(4, PRDraft, 10*time.Minute)
	add(5, Done, time.Hour) // merged: never again
	checked(5, PRMerged, 48*time.Hour)
	add(6, Done, 40*24*time.Hour)        // reviewed over a month ago: left alone
	add(7, AwaitingYou, 40*24*time.Hour) // but a waiting one is checked however old
	checked(7, PROpen, 11*time.Minute)

	due, err := s.PRsToCheck(now.Add(-10*time.Minute), now.Add(-time.Hour), now.Add(-30*24*time.Hour), 10)
	if err != nil {
		t.Fatal(err)
	}
	var got []int
	for _, k := range due {
		got = append(got, k.Number)
	}
	if want := []int{1, 7, 3}; !slices.Equal(got, want) {
		t.Errorf("due %v, want %v (waiting first, the least recently checked first)", got, want)
	}
	if due, _ := s.PRsToCheck(now.Add(-10*time.Minute), now.Add(-time.Hour), now.Add(-30*24*time.Hour), 1); len(due) != 1 || due[0].Number != 1 {
		t.Errorf("limit 1: %v", due)
	}

	// Asked about without an answer, a PR waits its turn again instead of being asked on every check.
	if err := s.TouchPR("o/r", 1); err != nil {
		t.Fatal(err)
	}
	if due, _ := s.PRsToCheck(now.Add(-10*time.Minute), now.Add(-time.Hour), now.Add(-30*24*time.Hour), 10); slices.ContainsFunc(due, func(k PRKey) bool { return k.Number == 1 }) {
		t.Error("a PR just asked about is due again")
	}
}

// A PR merged or closed while its review waited for you leaves "Needs you", and shows among the ones closed out.
func TestCloseOut(t *testing.T) {
	s := openTest(t)
	waiting, _ := s.Insert(&Review{Repo: "o/r", Number: 1, Status: AwaitingYou})
	fid, _ := s.AddFinding(&Finding{ReviewID: waiting, Agent: "logic", Severity: "high", Title: "t", Comment: "c", Status: Pending})
	other, _ := s.Insert(&Review{Repo: "o/r", Number: 2, Status: AwaitingYou})
	before := time.Now().Add(-time.Second)

	if n, err := s.CloseOut("o/r", 1, MergedFirst, time.Now()); err != nil || n != 1 {
		t.Fatalf("closed %d: %v", n, err)
	}
	r, _ := s.Get(waiting)
	if r.Status != Done || r.Decision != MergedFirst || r.FinishedAt.IsZero() {
		t.Errorf("closed out: %s %q", r.Status, r.Decision)
	}
	if f, _ := s.Finding(fid); f.Status != Pending {
		t.Errorf("its finding is %s, want still pending: you never decided on it", f.Status)
	}
	if r, _ := s.Get(other); r.Status != AwaitingYou {
		t.Errorf("another PR's review was closed out: %s", r.Status)
	}
	if n, _ := s.CloseOut("o/r", 1, MergedFirst, time.Now()); n != 0 {
		t.Errorf("closed out %d again", n)
	}
	// A review you asked for after the merge, of the merged code, still waits for you.
	if n, _ := s.CloseOut("o/r", 2, MergedFirst, time.Now().Add(-time.Hour)); n != 0 {
		t.Errorf("closed out %d review(s) started after the merge", n)
	}
	if got, _ := s.ClosedOut(before); len(got) != 1 || got[0].ID != waiting {
		t.Errorf("closed out since: %v", got)
	}
	if got, _ := s.ClosedOut(time.Now().Add(time.Hour)); len(got) != 0 {
		t.Errorf("closed out in the future: %v", got)
	}

	_ = s.SetPRStatus("o/r", 1, PRStatus{State: PRMerged})
	_ = s.SetPRStatus("o/r", 2, PRStatus{State: PROpen})
	if rs, total, _ := s.History(HistoryFilter{PR: PRMerged}, 10, 0); total != 1 || rs[0].Number != 1 {
		t.Errorf("history of merged PRs: %d %v", total, rs)
	}
}
