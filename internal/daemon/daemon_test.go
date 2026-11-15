package daemon

import (
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/badam1/personal-review-bot/internal/config"
	"github.com/badam1/personal-review-bot/internal/gh"
	"github.com/badam1/personal-review-bot/internal/pipeline"
	"github.com/badam1/personal-review-bot/internal/store"
)

// Only a PR nobody has touched for maxAge is skipped, however long ago it was opened.
func TestIdle(t *testing.T) {
	now := time.Now()
	day := 24 * time.Hour
	twoWeeks := 14 * day
	cases := []struct {
		name             string
		created, updated time.Time
		maxAge           time.Duration
		want             bool
	}{
		{"opened yesterday", now.Add(-day), now.Add(-day), twoWeeks, false},
		{"opened 30 days ago, commented yesterday", now.Add(-30 * day), now.Add(-day), twoWeeks, false},
		{"opened 30 days ago, last touched 13 days ago", now.Add(-30 * day), now.Add(-13 * day), twoWeeks, false},
		{"opened 30 days ago, last touched 15 days ago", now.Add(-30 * day), now.Add(-15 * day), twoWeeks, true},
		{"no activity known, opened 15 days ago", now.Add(-15 * day), time.Time{}, twoWeeks, true},
		{"no limit", now.Add(-365 * day), now.Add(-365 * day), 0, false},
		{"nothing known", time.Time{}, time.Time{}, twoWeeks, false},
	}
	for _, c := range cases {
		if got := idle(gh.PR{CreatedAt: c.created, UpdatedAt: c.updated}, c.maxAge, now); got != c.want {
			t.Errorf("%s: got %v, want %v", c.name, got, c.want)
		}
	}
}

// A poll is due once its time has come by the wall clock, which runs on while the Mac sleeps; one planned further off
// than an interval (the clock set back, the interval shortened) is due at once.
func TestPollDue(t *testing.T) {
	now := time.Date(2026, 11, 12, 9, 0, 0, 0, time.UTC)
	cases := []struct {
		name string
		next time.Time
		want bool
	}{
		{"the first poll", time.Time{}, true},
		{"not yet", now.Add(time.Minute), false},
		{"its time", now, true},
		{"woke long after it", now.Add(-time.Hour), true},
		{"further off than an interval", now.Add(5 * time.Minute), true},
	}
	for _, c := range cases {
		if got := pollDue(now, c.next, 2*time.Minute); got != c.want {
			t.Errorf("%s: due %v, want %v", c.name, got, c.want)
		}
	}
}

func TestReviewReason(t *testing.T) {
	now := time.Now()
	reviewed := func(status, sha string, ago time.Duration) *store.Review {
		return &store.Review{Status: status, HeadSHA: sha, CreatedAt: now.Add(-ago)}
	}
	skippedIdle := &store.Review{Status: store.Ignored, Source: idleSkip, HeadSHA: "a", CreatedAt: now.Add(-3 * 24 * time.Hour)}
	cases := []struct {
		name string
		pr   gh.PR
		last *store.Review
		want string
	}{
		{"never seen", gh.PR{HeadSHA: "a"}, nil, "auto"},
		{"already running", gh.PR{HeadSHA: "b"}, reviewed(store.Running, "a", time.Hour), ""},
		{"same head, no new request", gh.PR{HeadSHA: "a", RequestedAt: now.Add(-2 * time.Hour)}, reviewed(store.Done, "a", time.Hour), ""},
		{"author re-requested", gh.PR{HeadSHA: "a", RequestedAt: now.Add(-time.Minute)}, reviewed(store.Done, "a", time.Hour), "re-request"},
		{"new commits after cooldown", gh.PR{HeadSHA: "b"}, reviewed(store.AwaitingYou, "a", time.Hour), "new commits"},
		{"new commits during cooldown", gh.PR{HeadSHA: "b"}, reviewed(store.Done, "a", 5*time.Minute), ""},
		{"skipped at first start, then pushed", gh.PR{HeadSHA: "b"}, reviewed(store.Ignored, "a", 2*time.Hour), "new commits"},
		{"skipped as idle, active again", gh.PR{HeadSHA: "a", UpdatedAt: now.Add(-time.Hour)}, skippedIdle, "activity"},
		{"skipped as idle, then pushed", gh.PR{HeadSHA: "b", UpdatedAt: now.Add(-time.Hour)}, skippedIdle, "new commits"},
		{"skipped at first start, then only commented", gh.PR{HeadSHA: "a", UpdatedAt: now.Add(-time.Minute)}, reviewed(store.Ignored, "a", 2*time.Hour), ""},
		{"reviewed, then only commented", gh.PR{HeadSHA: "a", UpdatedAt: now.Add(-time.Minute)}, reviewed(store.Done, "a", time.Hour), ""},
	}
	for _, c := range cases {
		if got := reviewReason(c.pr, c.last); got != c.want {
			t.Errorf("%s: got %q, want %q", c.name, got, c.want)
		}
	}
}

// The PR state check moves a review out of "Needs you" once its PR is merged or closed, leaves an open one waiting,
// and doesn't ask again on every check about a PR GitHub won't answer for.
func TestCheckPRs(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "reviews.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	merged, _ := st.Insert(&store.Review{Repo: "o/r", Number: 1, Status: store.AwaitingYou})
	closed, _ := st.Insert(&store.Review{Repo: "o/r", Number: 2, Status: store.AwaitingYou})
	open, _ := st.Insert(&store.Review{Repo: "o/r", Number: 3, Status: store.AwaitingYou})
	gone, _ := st.Insert(&store.Review{Repo: "o/r", Number: 4, Status: store.AwaitingYou})
	asked := map[int]int{}
	get := func(repo string, n int) (gh.PR, error) {
		asked[n]++
		switch n {
		case 1:
			return gh.PR{State: "merged", MergedBy: "lena", ClosedAt: time.Now()}, nil
		case 2:
			return gh.PR{State: "closed", ClosedAt: time.Now()}, nil
		case 3:
			return gh.PR{State: "open"}, nil
		}
		return gh.PR{}, errors.New("HTTP 404: Not Found")
	}
	d := pipeline.Deps{Store: st}
	checkPRs(d, get, time.Now())
	for id, want := range map[int64][2]string{merged: {store.Done, store.MergedFirst}, closed: {store.Done, store.ClosedFirst},
		open: {store.AwaitingYou, ""}, gone: {store.AwaitingYou, ""}} {
		if r, _ := st.Get(id); r.Status != want[0] || r.Decision != want[1] {
			t.Errorf("review %d: %s %q, want %s %q", id, r.Status, r.Decision, want[0], want[1])
		}
	}
	if r, _ := st.Get(merged); r.PR.State != store.PRMerged || r.PR.By != "lena" {
		t.Errorf("merged PR recorded as %+v", r.PR)
	}
	checkPRs(d, get, time.Now())
	if asked[1] != 1 || asked[3] != 1 || asked[4] != 1 {
		t.Errorf("asked again right away: %v (a merged PR never, the others after a while)", asked)
	}
}

// checkDownstream matches each posted finding to the bot's own thread before reading its state, so a colleague's thread
// on the same line is never taken for it, and a PR that can't be read keeps any signal already seen.
func TestCheckDownstream(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "reviews.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	const owner, wm = "me", "[personal-review-bot]"
	post := func(repo string, number int, line int, comment string) int64 {
		rev, _ := st.Insert(&store.Review{Repo: repo, Number: number, Status: store.Done})
		id, _ := st.AddFinding(&store.Finding{ReviewID: rev, Agent: "logic", Path: "a.go", Line: line, Severity: "low",
			Title: "t", Comment: comment, Status: store.Candidate, Inline: true})
		_ = st.UpdateFinding(&store.Finding{ID: id, Path: "a.go", Line: line, Comment: comment, Status: store.Posted,
			Inline: true, PostedURL: "u"})
		return id
	}
	endorsed := post("o/r", 1, 10, "null deref here")  // the bot's thread is resolved → endorsed
	notBot := post("o/r", 1, 20, "off-by-one in loop") // only a colleague's thread sits on this line → stays unchecked
	errored := post("o/x", 2, 5, "leaks the token")    // its PR can't be read → keep the prior signal
	if err := st.SetDownstream(errored, "disputed", "2000-01-01T00:00:00Z"); err != nil {
		t.Fatal(err)
	}

	threads := map[string][]gh.Thread{
		"o/r": {
			{Path: "a.go", Line: 10, Resolved: true, Comments: []gh.ThreadComment{{Author: owner, Body: "null deref here\n" + wm}}},
			{Path: "a.go", Line: 20, Comments: []gh.ThreadComment{{Author: "someone-else", Body: "off-by-one in loop " + wm, ThumbsDown: 1}}},
		},
	}
	threadsOf := func(repo string, _ int) ([]gh.Thread, error) {
		ts, ok := threads[repo]
		if !ok {
			return nil, errors.New("HTTP 404: Not Found")
		}
		return ts, nil
	}
	d := pipeline.Deps{Store: st, GH: &gh.Client{Login: owner}, Cfg: config.Config{Watermark: wm}}
	checkDownstream(d, threadsOf, time.Now())

	if f, _ := st.Finding(endorsed); f.Downstream != "endorsed" {
		t.Errorf("the bot's resolved thread: downstream = %q, want endorsed", f.Downstream)
	}
	if f, _ := st.Finding(notBot); f.Downstream != "" {
		t.Errorf("a colleague's 👎 on their own thread was taken for the bot's finding: downstream = %q, want unchecked", f.Downstream)
	}
	if f, _ := st.Finding(errored); f.Downstream != "disputed" {
		t.Errorf("a PR that couldn't be read erased the prior signal: downstream = %q, want disputed kept", f.Downstream)
	}
}
