package web

import (
	"slices"
	"testing"

	"github.com/badam1/personal-review-bot/internal/store"
)

// An update's state lays out as five steps, the current one saying what it does.
func TestUpdateSteps(t *testing.T) {
	for state, want := range map[string]struct {
		at      int
		label   string
		percent int
	}{
		"starting the update to v0.2.0":                               {0, "Starting the update to v0.2.0", 10},
		"downloading v0.2.0 (6.3 MB)":                                 {0, "Downloading v0.2.0 (6.3 MB)", 10},
		"checking v0.2.0's checksum and build attestation":            {1, "Checking v0.2.0's checksum and build attestation", 30},
		"installing v0.2.0":                                           {2, "Installing v0.2.0", 50},
		"v0.2.0 installed: restarting once 1 running review finishes": {3, "Restarting once 1 running review finishes", 70},
	} {
		steps, percent := updateSteps(state)
		at := slices.IndexFunc(steps, func(s stepView) bool { return s.State == "now" })
		if at != want.at || steps[at].Label != want.label || percent != want.percent {
			t.Errorf("%q: step %d %q %d%%, want %d %q %d%%", state, at, steps[at].Label, percent, want.at, want.label, want.percent)
		}
		for i, s := range steps {
			if i < at && s.State != "done" || i > at && s.State != "todo" {
				t.Errorf("%q: step %d is %s", state, i, s.State)
			}
		}
	}
}

// A review's stage maps to the step it is on; a queued one hasn't started any.
func TestReviewSteps(t *testing.T) {
	now := func(r store.Review) int {
		return slices.IndexFunc(reviewSteps(r), func(s stepView) bool { return s.State == "now" })
	}
	for stage, want := range map[string]int{"fetching the PR": 0, "reviewing (3 agents)": 1, "verifying 2 finding(s) (opus)": 2,
		"posting": 3} {
		if got := now(store.Review{Status: store.Running, Stage: stage}); got != want {
			t.Errorf("%q: step %d, want %d", stage, got, want)
		}
	}
	if got := now(store.Review{Status: store.Queued}); got != -1 {
		t.Errorf("queued: step %d, want none", got)
	}
}

// A review waiting for you says what it holds; with nothing new it asks to approve.
func TestWaitingBadges(t *testing.T) {
	text := func(bs []badge) []string {
		var out []string
		for _, b := range bs {
			out = append(out, b.Text)
		}
		return out
	}
	fs := []store.Finding{{Agent: "logic", Status: store.Pending}, {Agent: "logic", Status: store.Pending},
		{Agent: "design", Status: store.Pending}, {Agent: "logic", Status: store.AutoPosted}}
	if got := text(waitingBadges(fs, nil)); !slices.Equal(got, []string{"2 to decide", "1 design concern"}) {
		t.Errorf("badges = %q", got)
	}
	if got := text(waitingBadges(nil, nil)); !slices.Equal(got, []string{"Nothing new: approve?"}) {
		t.Errorf("nothing new = %q", got)
	}
}

// A review sits under Reviews while it still needs something, under History once it's over.
func TestHome(t *testing.T) {
	for _, tc := range []struct {
		status  string
		retried bool
		want    string
	}{
		{store.AwaitingYou, false, "reviews"}, {store.Queued, false, "reviews"}, {store.Running, false, "reviews"},
		{store.Failed, false, "reviews"}, {store.Failed, true, "history"}, {store.Done, false, "history"},
		{store.Ignored, false, "history"},
	} {
		if got := home(store.Review{Status: tc.status}, tc.retried); got != tc.want {
			t.Errorf("%s (retried %v): %s, want %s", tc.status, tc.retried, got, tc.want)
		}
	}
}
