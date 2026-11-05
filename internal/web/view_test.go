package web

import (
	"fmt"
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
	if got := text(waitingBadges(store.Review{}, fs, nil)); !slices.Equal(got, []string{"2 to decide", "1 design concern"}) {
		t.Errorf("badges = %q", got)
	}
	if got := text(waitingBadges(store.Review{}, nil, nil)); !slices.Equal(got, []string{"Nothing new: approve?"}) {
		t.Errorf("nothing new = %q", got)
	}
	sure := store.Review{Approval: 92, ApprovalReason: "Every changed file was read."}
	if got := waitingBadges(sure, nil, nil); len(got) != 1 || got[0].Text != "Nothing new: approve? 92% sure" || got[0].Kind != "approve" {
		t.Errorf("sure = %+v", got)
	}
	unsure := store.Review{Approval: 55, ApprovalReason: "Nobody read the Terraform."}
	if got := waitingBadges(unsure, nil, nil); len(got) != 1 || got[0].Text != "Nothing new, but only 55% sure" || got[0].Kind != "open" {
		t.Errorf("unsure = %+v", got)
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

// The tally says how the verifier judged the reviewers' findings and lists the rest by severity and certainty, worst
// first; the reviewers come design first, then by how much they found.
func TestTallyFindings(t *testing.T) {
	fs := []store.Finding{
		{Agent: "logic", Severity: "low", Confidence: 90, Verdict: "confirmed", Status: store.Posted},
		{Agent: "logic", Severity: "high", Confidence: 62, Verdict: "plausible", Status: store.Pending},
		{Agent: "security", Severity: "high", Confidence: 91, Verdict: "confirmed", Status: store.AutoPosted},
		{Agent: "logic", Severity: "medium", Confidence: 40, Verdict: "rejected", Status: store.Rejected},
		{Agent: "design", Severity: "medium", Confidence: 80, Verdict: "design", Status: store.Pending},
		{Agent: "logic", Severity: "low", Status: store.Candidate}, // still with the verifier
	}
	got := tallyFindings(fs)
	if want := "4 found → 2 confirmed · 1 plausible · 1 rejected · 1 design concern"; got.Line != want {
		t.Errorf("line %q, want %q", got.Line, want)
	}
	var chips []string
	for _, c := range got.Chips {
		chips = append(chips, fmt.Sprintf("%s %d", c.Severity, c.Confidence))
	}
	if want := []string{"high 91", "high 62", "medium 80", "low 90"}; !slices.Equal(chips, want) {
		t.Errorf("chips %v, want %v (rejected left out)", chips, want)
	}
	var agents []string
	for _, a := range got.Agents {
		agents = append(agents, fmt.Sprintf("%s %d", a.Name, a.N))
	}
	if want := []string{"design 1", "logic 3", "security 1"}; !slices.Equal(agents, want) || got.Total != 5 {
		t.Errorf("agents %v (total %d), want %v (5)", agents, got.Total, want)
	}
	if got := tallyFindings(nil).Line; got != "The reviewers found nothing" {
		t.Errorf("nothing found: %q", got)
	}
}

// A bot's `code` and **bold** show as such; everything else is escaped.
func TestTicks(t *testing.T) {
	if got := string(ticks("**Reads:** `a<b>` & <i>x</i>")); got != "<b>Reads:</b> <code>a&lt;b&gt;</code> &amp; &lt;i&gt;x&lt;/i&gt;" {
		t.Errorf("ticks = %q", got)
	}
}
