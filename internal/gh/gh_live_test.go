package gh

import (
	"os"
	"testing"
)

// Run against GitHub with: PERSONAL_REVIEW_BOT_LIVE_PR=https://github.com/owner/repo/pull/N go test ./internal/gh -run Live -v
func TestConversationLive(t *testing.T) {
	url := os.Getenv("PERSONAL_REVIEW_BOT_LIVE_PR")
	if url == "" {
		t.Skip("set PERSONAL_REVIEW_BOT_LIVE_PR to run")
	}
	c, err := New()
	if err != nil {
		t.Fatal(err)
	}
	repo, n, err := ParseURL(url)
	if err != nil {
		t.Fatal(err)
	}
	conv, err := c.Conversation(repo, n, "🤖 acme-bot")
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("conversation of %s#%d:\n%s", repo, n, conv)
	prs, err := c.RequestedFromMe([]string{"acme"})
	if err != nil {
		t.Fatal(err)
	}
	for _, pr := range prs {
		t.Logf("requested from me: %s#%d at %s", pr.Repo, pr.Number, pr.RequestedAt)
	}
}
