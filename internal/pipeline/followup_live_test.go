package pipeline

import (
	"cmp"
	"context"
	"math"
	"os"
	"testing"
	"time"

	"github.com/badam1/personal-review-bot/internal/agents"
	"github.com/badam1/personal-review-bot/internal/config"
	"github.com/badam1/personal-review-bot/internal/gh"
	"github.com/badam1/personal-review-bot/internal/store"
	"github.com/badam1/personal-review-bot/internal/workspace"
)

// Runs only the follow-up check on one PR: what became of the findings posted on it, read from a copy of the database.
// It posts nothing and stores nothing in that copy, in a checkout of its own.
//
//	PERSONAL_REVIEW_BOT_LIVE_FOLLOWUP=https://github.com/owner/repo/pull/N PERSONAL_REVIEW_BOT_LIVE_DB=/path/to/copy.db \
//	  go test ./internal/pipeline -run FollowupLive -v -timeout 30m
func TestFollowupLive(t *testing.T) {
	url, dbPath := os.Getenv("PERSONAL_REVIEW_BOT_LIVE_FOLLOWUP"), os.Getenv("PERSONAL_REVIEW_BOT_LIVE_DB")
	if url == "" || dbPath == "" {
		t.Skip("set PERSONAL_REVIEW_BOT_LIVE_FOLLOWUP to a PR URL and PERSONAL_REVIEW_BOT_LIVE_DB to a copy of the database to run")
	}
	ctx := context.Background()
	st, err := store.Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	c, err := gh.New()
	if err != nil {
		t.Fatal(err)
	}
	repo, n, err := gh.ParseURL(url)
	if err != nil {
		t.Fatal(err)
	}
	earlier := unsettled(Deps{Store: st, Log: t.Logf}, repo, n, math.MaxInt64)
	if len(earlier) == 0 {
		t.Skip("no unsettled posted findings on this PR")
	}
	pr, err := c.GetPR(repo, n)
	if err != nil {
		t.Fatal(err)
	}
	conv, err := c.Conversation(repo, n, config.Load().Watermark)
	if err != nil {
		t.Fatal(err)
	}
	ws, err := workspace.Prepare(ctx, t.TempDir(), repo, n, pr.HeadSHA)
	if err != nil {
		t.Fatal(err)
	}
	defer ws.Cleanup()

	model := cmp.Or(os.Getenv("PERSONAL_REVIEW_BOT_LIVE_MODEL"), "sonnet")
	in := agents.Input{Repo: repo, Number: n, Title: pr.Title, Author: pr.Author, Body: pr.Body, Base: pr.BaseRef,
		Head: pr.HeadRef, Dir: ws.Dir, HeadSHA: pr.HeadSHA, Conversation: conv, Owner: c.Login, Org: relatedOrg("", repo)}
	fus, run, err := agents.Builtin().CheckEarlier(ctx, model, in, earlier, 20*time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("%s on %s#%d: %d earlier, %d turns, %s, $%.2f", model, repo, n, len(earlier), run.Turns,
		run.Duration.Round(time.Second), run.Cost)
	titles := map[int64]string{}
	for _, e := range earlier {
		titles[e.ID] = e.Title
	}
	for _, f := range fus {
		t.Logf("\n[%s] %s\n  %s", f.Status, titles[f.ID], f.Note)
	}
}
