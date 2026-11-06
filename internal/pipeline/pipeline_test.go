package pipeline

import (
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/badam1/personal-review-bot/internal/agents"
	"github.com/badam1/personal-review-bot/internal/config"
	"github.com/badam1/personal-review-bot/internal/gh"
	"github.com/badam1/personal-review-bot/internal/store"
	"github.com/badam1/personal-review-bot/internal/triage"
)

// A review where a reviewer failed is never summed up as clean, and always goes to the page.
func TestOutcome(t *testing.T) {
	cases := []struct {
		name                       string
		posted                     string
		pending, failed, reviewers int
		want                       string
		needsYou                   bool
	}{
		{"clean", "", 0, 0, 4, "no issues: approve?", true},
		{"posted everything", "posted 2", 0, 0, 4, "posted 2", false},
		{"some below threshold", "posted 1", 2, 0, 4, "posted 1, 2 need you", true},
		{"reviewers failed, rest found nothing", "", 0, 2, 3, "2 of 3 reviewers failed: re-review", true},
		{"reviewers failed after posting", "posted 1", 0, 1, 4, "posted 1, 1 of 4 reviewers failed: re-review", true},
		{"reviewers failed with pending", "", 3, 1, 4, "3 need you, 1 of 4 reviewers failed: re-review", true},
	}
	for _, c := range cases {
		got, needsYou := outcome(c.posted, c.pending, c.failed, c.reviewers)
		if got != c.want || needsYou != c.needsYou {
			t.Errorf("%s: got %q (needs you %v), want %q (%v)", c.name, got, needsYou, c.want, c.needsYou)
		}
	}
}

// The design reviewer runs on a PR's first review or one you asked for, never on automatic re-reviews.
func TestDesignNow(t *testing.T) {
	worth := triage.Plan{Design: true}
	cases := []struct {
		name         string
		plan         triage.Plan
		prev, source string
		want         bool
	}{
		{"first review", worth, "", "auto", true},
		{"first review of a PR skipped as idle", worth, "", "activity", true},
		{"Re-review button", worth, "abc", "manual", true},
		{"re-request", worth, "abc", "re-request", false},
		{"new commits", worth, "abc", "new commits", false},
		{"small PR", triage.Plan{}, "", "auto", false},
	}
	for _, c := range cases {
		if got := designNow(c.plan, c.prev, c.source); got != c.want {
			t.Errorf("%s: got %v, want %v", c.name, got, c.want)
		}
	}
}

// Design concerns lead the review body with their own footer; findings stay inline where they can.
func TestReviewBody(t *testing.T) {
	cfg := config.Config{Watermark: "🤖 acme-bot"}
	body, comments := reviewBody(cfg, []store.Finding{
		{Agent: "logic", Path: "a.go", Line: 3, Inline: true, Comment: "Off by one.", Confidence: 90},
		{Agent: "logic", Path: "b.go", Line: 9, Comment: "Outside the diff.", Confidence: 70},
		{Agent: agents.DesignAgent, Title: "Second release path", Comment: "Use the shared one.", Confidence: 85},
	})
	if len(comments) != 1 || comments[0].Path != "a.go" || !strings.Contains(comments[0].Body, "🤖 acme-bot · 90%") {
		t.Errorf("inline comments = %+v", comments)
	}
	if len(body) != 2 {
		t.Fatalf("body = %q", body)
	}
	if !strings.HasPrefix(body[0], "**Design: Second release path**") || !strings.Contains(body[0], "🤖 acme-bot · design · 85%") {
		t.Errorf("design concern should lead the body with its own footer: %q", body[0])
	}
	if !strings.HasPrefix(body[1], "**`b.go:9`**") {
		t.Errorf("out-of-diff finding = %q", body[1])
	}
}

// Runs finish out of order: an older usage reading never replaces a newer one.
func TestSaveUsage(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "reviews.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	if _, ok := LoadUsage(st); ok {
		t.Fatal("a new database has no usage reading")
	}
	now := time.Now().Round(time.Second)
	saveUsage(st, agents.Usage{Week: 0.50, Seen: now})
	saveUsage(st, agents.Usage{Week: 0.40, Seen: now.Add(-time.Minute)})
	if u, ok := LoadUsage(st); !ok || u.Week != 0.50 || !u.Seen.Equal(now) {
		t.Errorf("usage = %+v, want the newer reading (50%%)", u)
	}
}

// The excerpt is the code around a finding's line, with the PR's added lines marked; paths stay in the checkout.
func TestExcerpt(t *testing.T) {
	dir := t.TempDir()
	_ = os.MkdirAll(filepath.Join(dir, "pkg"), 0o755)
	_ = os.WriteFile(filepath.Join(dir, "pkg", "a.go"), []byte("1\n2\n3\n4\n5\n6\n7\n8\n9\n10\n"), 0o644)
	got := excerpt(dir, "pkg/a.go", 5, map[int]bool{5: true})
	want := "2\t \t2\n3\t \t3\n4\t \t4\n5\t+\t5\n6\t \t6\n7\t \t7\n8\t \t8"
	if got != want {
		t.Errorf("excerpt = %q, want %q", got, want)
	}
	if got := excerpt(dir, "pkg/a.go", 1, nil); !strings.HasPrefix(got, "1\t \t1\n") || strings.Count(got, "\n") != 3 {
		t.Errorf("at the top of the file: %q", got)
	}
	for _, path := range []string{"../../etc/passwd", "pkg/missing.go", ""} {
		if got := excerpt(dir, path, 1, nil); got != "" {
			t.Errorf("excerpt(%q) = %q, want nothing", path, got)
		}
	}
	if got := excerpt(dir, "pkg/a.go", 99, nil); got != "" {
		t.Errorf("past the end: %q", got)
	}
}

// Re-reviews are told which posted findings you later marked wrong.
func TestFormatPriorWrong(t *testing.T) {
	got := formatPrior([]store.Finding{{Agent: "logic", Path: "a.go", Line: 3, Title: "Off by one", Confidence: 90,
		Status: store.AutoPosted, Wrong: true}}, "@octocat")
	if !strings.Contains(got, "[posted by the bot, then @octocat marked it WRONG] a.go:3 Off by one") {
		t.Errorf("prior = %q", got)
	}
}

// The follow-up runs when earlier findings are unsettled and the PR moved, or when someone asked for the review.
func TestFollowupNow(t *testing.T) {
	on := triage.Plan{Followup: true}
	earlier := []agents.Earlier{{ID: 1, Commit: "aaa"}}
	cases := []struct {
		name         string
		plan         triage.Plan
		earlier      []agents.Earlier
		head, source string
		want         bool
	}{
		{"new commits", on, earlier, "bbb", "new commits", true},
		{"re-request, same commit", on, earlier, "aaa", "re-request", true},
		{"you asked, same commit", on, earlier, "aaa", "manual", true},
		{"automatic, same commit", on, earlier, "aaa", "activity", false},
		{"nothing to check", on, nil, "bbb", "new commits", false},
		{"near the usage limit", triage.Plan{}, earlier, "bbb", "new commits", false},
	}
	for _, c := range cases {
		if got := followupNow(c.plan, c.earlier, c.head, c.source); got != c.want {
			t.Errorf("%s: got %v, want %v", c.name, got, c.want)
		}
	}
}

// Only posted findings that are neither settled nor marked wrong are checked, each with its review's commit.
func TestUnsettled(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "reviews.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	rev, _ := st.Insert(&store.Review{Repo: "o/r", Number: 1, HeadSHA: "aaa", Status: store.Running})
	add := func(title string) int64 {
		id, _ := st.AddFinding(&store.Finding{ReviewID: rev, Agent: "logic", Severity: "high", Path: "a.go", Line: 3,
			Title: title, Comment: "c", Status: store.Candidate})
		_ = st.UpdateFinding(&store.Finding{ID: id, Line: 3, Comment: "c", Status: store.Posted})
		return id
	}
	open, fixed, wrong, partly := add("open"), add("fixed"), add("wrong"), add("partly")
	_ = st.SetFollowup(fixed, store.FollowFixed, "", rev)
	_ = st.SetWrong(wrong, true)
	_ = st.SetFollowup(partly, store.FollowPartly, "", rev)
	now, _ := st.Insert(&store.Review{Repo: "o/r", Number: 1, HeadSHA: "bbb", Status: store.Running})

	got := unsettled(Deps{Store: st, Log: t.Logf}, "o/r", 1, now)
	if len(got) != 2 || got[0].ID != open || got[1].ID != partly || got[0].Commit != "aaa" || got[0].Path != "a.go" {
		t.Errorf("unsettled = %+v", got)
	}
	if prior := formatPrior([]store.Finding{{Agent: "logic", Path: "a.go", Line: 3, Title: "t", Status: store.Posted,
		Followup: store.FollowOpen}}, "@octocat"); !strings.Contains(prior, "posted by @octocat; last follow-up: still open") {
		t.Errorf("prior = %q", prior)
	}
}

// The progress line says which model each reviewer runs on, without suggesting an unpinned one chose its own.
func TestByModel(t *testing.T) {
	p := triage.Plan{AgentModel: "haiku", Agents: []string{"logic", "security", "go-tests"},
		Pinned: map[string]string{"security": "sonnet"}}
	if got := byModel(p); got != "logic, go-tests (haiku), security (sonnet)" {
		t.Errorf("byModel = %q", got)
	}
}

// A finding's thread is the one its own posted comment started: by the owner, signed, on its path, with its text.
func TestMatchThread(t *testing.T) {
	f := store.Finding{Agent: "logic", Path: "a.go", Line: 7, Inline: true, Comment: "The retry loop\nswallows the error."}
	signed := func(author, body string) gh.ThreadComment {
		return gh.ThreadComment{Author: author, Body: body + "\n\n<sub>🤖 acme-bot · 91%</sub>"}
	}
	threads := []gh.Thread{
		{ID: "other-path", Path: "b.go", Comments: []gh.ThreadComment{signed("octocat", "The retry loop swallows the error.")}},
		{ID: "someone-else", Path: "a.go", Comments: []gh.ThreadComment{signed("dev", "The retry loop swallows the error.")}},
		{ID: "unsigned", Path: "a.go", Comments: []gh.ThreadComment{{Author: "octocat", Body: "The retry loop swallows the error."}}},
		{ID: "other-text", Path: "a.go", Comments: []gh.ThreadComment{signed("octocat", "Off by one.")}},
		{ID: "it", Path: "a.go", Comments: []gh.ThreadComment{signed("octocat", "The retry loop  swallows\nthe error."),
			{Author: "dev", Body: "fixing"}, {Author: "octocat", Body: "thanks"}}},
	}
	got, ok := MatchThread(f, threads, "octocat", "🤖 acme-bot")
	if !ok || got.ID != "it" {
		t.Fatalf("matched %q, %v; want the thread its comment started", got.ID, ok)
	}
	if a, ok := Answer(got, "octocat"); !ok || a.Body != "fixing" {
		t.Errorf("answer = %+v, %v; want the latest reply by someone else", a, ok)
	}
	if _, ok := Answer(threads[0], "octocat"); ok {
		t.Error("a thread with only the owner's comment has no answer")
	}
	for _, other := range []store.Finding{{Agent: "design", Comment: f.Comment, Path: "a.go", Inline: true},
		{Agent: "logic", Comment: f.Comment, Path: "a.go", Inline: false}} {
		if _, ok := MatchThread(other, threads, "octocat", "🤖 acme-bot"); ok {
			t.Errorf("%+v has no thread: it went in a review body", other)
		}
	}
}

func TestReminderText(t *testing.T) {
	cases := []struct {
		f    store.Finding
		want string
	}{
		{store.Finding{Followup: store.FollowOpen, FollowupNote: "a.go:9 still returns nil."}, "Still open at `abcdef1`: a.go:9 still returns nil."},
		{store.Finding{Followup: store.FollowPartly, FollowupNote: "d.go is left."}, "Only partly fixed at `abcdef1`: d.go is left."},
		{store.Finding{Followup: store.FollowOpen}, "Still open at `abcdef1`."},
	}
	for _, c := range cases {
		if got := ReminderText(c.f, "abcdef1234"); got != c.want {
			t.Errorf("ReminderText = %q, want %q", got, c.want)
		}
	}
	for status, want := range map[string]bool{store.FollowOpen: true, store.FollowPartly: true, store.FollowFixed: false,
		store.FollowMoot: false, store.FollowDeclined: false, "": false} {
		if got := Remindable(store.Finding{Followup: status}); got != want {
			t.Errorf("Remindable(%q) = %v", status, got)
		}
	}
	if Remindable(store.Finding{Followup: store.FollowOpen, Wrong: true}) {
		t.Error("a finding marked wrong is never reminded of")
	}
}

// Reminders with a thread become replies (reopening resolved threads); the rest are listed in the review body.
func TestReminderParts(t *testing.T) {
	cfg := config.Config{Watermark: "🤖 acme-bot"}
	open, resolved := gh.Thread{ID: "T1"}, gh.Thread{ID: "T3", Resolved: true}
	section, replies := reminderParts(cfg, []Reminder{
		{Finding: store.Finding{Title: "Retry loop"}, Text: " Still open. ", Thread: &open},
		{Finding: store.Finding{Title: "Port"}, Text: "Partly.", Thread: &resolved},
		{Finding: store.Finding{Title: "Second release path", PostedURL: "https://x/r1"}, Text: "Still open."},
		{Finding: store.Finding{Title: "Log level", Path: "e.go", Line: 7}, Text: "Unchanged."},
	})
	if len(replies) != 2 || replies[0] != (gh.Reply{ThreadID: "T1", Body: "Still open.\n\n<sub>🤖 acme-bot · reminder</sub>"}) ||
		!replies[1].Reopen || replies[0].Reopen {
		t.Errorf("replies = %+v", replies)
	}
	want := "**Still open from earlier reviews** (2 with a reply in their thread)\n\n" +
		"- **Second release path**: Still open. ([earlier review](https://x/r1))\n" +
		"- **Log level** (`e.go:7`): Unchanged.\n\n<sub>🤖 acme-bot · reminder</sub>"
	if len(section) != 1 || section[0] != want {
		t.Errorf("section = %q\nwant      %q", section, want)
	}
	if section, replies := reminderParts(cfg, nil); section != nil || replies != nil {
		t.Error("no reminders, nothing to add")
	}
}

// Re-reviews are told which findings you already reminded the author of.
func TestFormatPriorReminded(t *testing.T) {
	got := formatPrior([]store.Finding{{Agent: "logic", Path: "a.go", Line: 3, Title: "t", Status: store.Posted,
		Followup: store.FollowOpen, RemindedSHA: "abcdef1234"}}, "@octocat")
	if !strings.Contains(got, "last follow-up: still open; @octocat reminded the author at abcdef1") {
		t.Errorf("prior = %q", got)
	}
}

// Findings the auto-post rule lets through become auto_posted only once GitHub has them. A failed post leaves them
// pending, so a later review of the PR doesn't take them for comments the bot left.
func TestPostAuto(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "reviews.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	d := Deps{Store: st, Log: t.Logf}
	id, _ := st.Insert(&store.Review{Repo: "o/r", Number: 1, Status: store.Running})
	var auto []store.Finding
	for _, title := range []string{"Nil map", "Typo"} {
		f := store.Finding{ReviewID: id, Agent: "logic", Severity: "low", Path: "a.go", Line: 1, Title: title, Comment: "c",
			Status: store.Candidate}
		f.ID, _ = st.AddFinding(&f)
		f.Status, f.Confidence = store.Pending, 90 // as Run saves them before posting
		_ = st.UpdateFinding(&f)
		auto = append(auto, f)
	}
	_ = st.Finish(id, store.Failed, "failed", "", "post review: HTTP 502")
	later, _ := st.Insert(&store.Review{Repo: "o/r", Number: 1, Status: store.Running})

	failing := func([]store.Finding) (string, error) { return "", errors.New("HTTP 502") }
	if err := postAuto(d, slices.Clone(auto), failing); err == nil {
		t.Fatal("the post's error should come back")
	}
	for _, f := range auto {
		if got, _ := st.Finding(f.ID); got.Status != store.Pending || got.PostedURL != "" {
			t.Errorf("after a failed post %q is %s at %q", f.Title, got.Status, got.PostedURL)
		}
	}
	if earlier, _ := st.EarlierPosted("o/r", 1, later); len(earlier) != 0 {
		t.Errorf("a later review sees %d posted finding(s) that never were", len(earlier))
	}

	url := "https://github.com/o/r/pull/1#pullrequestreview-1"
	if err := postAuto(d, slices.Clone(auto), func([]store.Finding) (string, error) { return url, nil }); err != nil {
		t.Fatal(err)
	}
	for _, f := range auto {
		if got, _ := st.Finding(f.ID); got.Status != store.AutoPosted || got.PostedURL != url {
			t.Errorf("after the post %q is %s at %q", f.Title, got.Status, got.PostedURL)
		}
	}
}
