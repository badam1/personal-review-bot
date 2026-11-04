package gh

import (
	"os"
	"strconv"
	"strings"
	"testing"
	"time"
)

// Posts on a real PR, so run it only on a PR of your own in a sandbox repo:
//
//	PERSONAL_REVIEW_BOT_LIVE_REMIND_PR=https://github.com/you/sandbox/pull/1 \
//	PERSONAL_REVIEW_BOT_LIVE_REMIND_PATH=README.md PERSONAL_REVIEW_BOT_LIVE_REMIND_LINE=3 \
//	go test ./internal/gh -run LiveRemind -v
//
// PATH and LINE must be a line the PR adds. It posts a comment, resolves its thread, then posts a review that replies
// in the thread and reopens it, and checks GitHub shows exactly that.
func TestLiveRemind(t *testing.T) {
	url := os.Getenv("PERSONAL_REVIEW_BOT_LIVE_REMIND_PR")
	if url == "" {
		t.Skip("set PERSONAL_REVIEW_BOT_LIVE_REMIND_PR (and _PATH, _LINE) to run")
	}
	path := os.Getenv("PERSONAL_REVIEW_BOT_LIVE_REMIND_PATH")
	line, _ := strconv.Atoi(os.Getenv("PERSONAL_REVIEW_BOT_LIVE_REMIND_LINE"))
	repo, n, err := ParseURL(url)
	if err != nil {
		t.Fatal(err)
	}
	c, err := New()
	if err != nil {
		t.Fatal(err)
	}
	pr, err := c.GetPR(repo, n)
	if err != nil {
		t.Fatal(err)
	}
	stamp := time.Now().Format("15:04:05")
	finding := "Live test finding " + stamp + "."
	if _, err := c.PostReview(repo, n, pr.HeadSHA, "COMMENT", "", []Comment{{Path: path, Line: line, Side: "RIGHT",
		Body: finding + "\n\n<sub>🤖 live-test · 90%</sub>"}}); err != nil {
		t.Fatal(err)
	}
	find := func() Thread {
		t.Helper()
		threads, err := c.ReviewThreads(repo, n)
		if err != nil {
			t.Fatal(err)
		}
		for _, th := range threads {
			if len(th.Comments) > 0 && strings.HasPrefix(th.Comments[0].Body, finding) {
				return th
			}
		}
		t.Fatal("the posted comment's thread is not listed")
		return Thread{}
	}
	th := find()
	if th.Comments[0].Author != c.Login || th.Path != path {
		t.Errorf("thread = %+v", th)
	}
	const resolve = `mutation($thread: ID!) { resolveReviewThread(input: {threadId: $thread}) { thread { id } } }`
	if err := c.gql.Do(resolve, map[string]any{"thread": th.ID}, &struct{}{}); err != nil {
		t.Fatal(err)
	}
	if !find().Resolved {
		t.Fatal("the thread didn't resolve")
	}

	reply := "Still open at `" + pr.HeadSHA[:7] + "`: live test reminder " + stamp + "."
	url2, err := c.PostReviewWithReplies(repo, n, pr.HeadSHA, "COMMENT", "Live test review body "+stamp+".", nil,
		[]Reply{{ThreadID: th.ID, Body: reply, Reopen: true}})
	if err != nil {
		t.Fatalf("post with replies: %v (url %q)", err, url2)
	}
	if !strings.Contains(url2, "#pullrequestreview-") {
		t.Errorf("review url = %q", url2)
	}
	th = find()
	if th.Resolved || len(th.Comments) != 2 || th.Comments[1].Body != reply {
		t.Errorf("after the reminder: resolved %v, comments %+v", th.Resolved, th.Comments)
	}
	t.Logf("posted %s", url2)
}
