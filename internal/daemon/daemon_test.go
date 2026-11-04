package daemon

import (
	"testing"
	"time"

	"github.com/badam1/personal-review-bot/internal/gh"
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
