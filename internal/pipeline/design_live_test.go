package pipeline

import (
	"cmp"
	"context"
	"os"
	"testing"
	"time"

	"github.com/badam1/personal-review-bot/internal/agents"
	"github.com/badam1/personal-review-bot/internal/config"
	"github.com/badam1/personal-review-bot/internal/gh"
	"github.com/badam1/personal-review-bot/internal/triage"
	"github.com/badam1/personal-review-bot/internal/workspace"
)

// Runs only the design reviewer on one PR, to try a prompt change for one run's cost instead of a full dry run.
// It posts and stores nothing, in a checkout of its own. The model is triage's pick unless PERSONAL_REVIEW_BOT_LIVE_MODEL says
// otherwise; PERSONAL_REVIEW_BOT_LIVE_FRESH=1 leaves out the PR's conversation, so concerns already posted don't hide a repeat.
//
//	PERSONAL_REVIEW_BOT_LIVE_DESIGN=https://github.com/owner/repo/pull/N go test ./internal/pipeline -run Live -v -timeout 30m
func TestDesignLive(t *testing.T) {
	url := os.Getenv("PERSONAL_REVIEW_BOT_LIVE_DESIGN")
	if url == "" {
		t.Skip("set PERSONAL_REVIEW_BOT_LIVE_DESIGN to a PR URL to run")
	}
	ctx := context.Background()
	c, err := gh.New()
	if err != nil {
		t.Fatal(err)
	}
	repo, n, err := gh.ParseURL(url)
	if err != nil {
		t.Fatal(err)
	}
	pr, err := c.GetPR(repo, n)
	if err != nil {
		t.Fatal(err)
	}
	files, err := c.Files(repo, n)
	if err != nil {
		t.Fatal(err)
	}
	diff, err := c.Diff(repo, n)
	if err != nil {
		t.Fatal(err)
	}
	conv := "(none)"
	if os.Getenv("PERSONAL_REVIEW_BOT_LIVE_FRESH") == "" {
		if conv, err = c.Conversation(repo, n, config.Load().Watermark); err != nil {
			t.Fatal(err)
		}
	}
	ws, err := workspace.Prepare(ctx, t.TempDir(), repo, n, pr.HeadSHA)
	if err != nil {
		t.Fatal(err)
	}
	defer ws.Cleanup()

	model := cmp.Or(os.Getenv("PERSONAL_REVIEW_BOT_LIVE_MODEL"), triage.Make(pr, files, agents.Builtin().Reviewers).DesignModel)
	in := agents.Input{Repo: repo, Number: n, Title: pr.Title, Author: pr.Author, Body: pr.Body, Base: pr.BaseRef,
		Head: pr.HeadRef, Diff: diff, Dir: ws.Dir, HeadSHA: pr.HeadSHA, Conversation: conv, Owner: c.Login,
		Org: relatedOrg("", repo)}
	concerns, run, err := agents.Builtin().Design(ctx, model, in, 20*time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("%s on %s#%d: %d turns, %s, $%.2f\n\nassessment: %s", model, repo, n, run.Turns,
		run.Duration.Round(time.Second), run.Cost, run.Checked)
	for _, c := range concerns {
		t.Logf("\n[%s · %d%%] %s\n%s\n(%d characters)", c.Severity, c.Confidence, c.Title, c.Text(), len(c.Text()))
	}
}
